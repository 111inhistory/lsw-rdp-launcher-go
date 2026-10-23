//! On-demand user-session worker and its local named-pipe transport.
//!
//! The SYSTEM backend never calls ShellExecute or mapping APIs itself. For an
//! already verified SessionRef it starts this same executable under the WTS
//! user token, with the user's environment and `winsta0\default` desktop, and
//! exchanges exactly one request/response over a per-request named pipe. The
//! pipe DACL admits only SYSTEM and the configured account; there is no global
//! interactive TCP endpoint and no temporary response file.
use std::ffi::c_void;
use std::io;
use std::mem;
use std::ptr;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use windows_sys::Win32::Foundation::*;
use windows_sys::Win32::Security::Authorization::ConvertStringSecurityDescriptorToSecurityDescriptorW;
use windows_sys::Win32::Security::*;
use windows_sys::Win32::Storage::FileSystem::{
    CreateFileW, FlushFileBuffers, ReadFile, WriteFile, FILE_FLAG_OVERLAPPED, OPEN_EXISTING,
    PIPE_ACCESS_DUPLEX,
};
use windows_sys::Win32::System::Com::{CoInitializeEx, CoUninitialize, COINIT_APARTMENTTHREADED};
use windows_sys::Win32::System::Environment::{CreateEnvironmentBlock, DestroyEnvironmentBlock};
use windows_sys::Win32::System::Pipes::*;
use windows_sys::Win32::System::RemoteDesktop::WTSQueryUserToken;
use windows_sys::Win32::System::Threading::*;
use windows_sys::Win32::System::IO::{CancelIoEx, GetOverlappedResult, OVERLAPPED};
use windows_sys::Win32::UI::WindowsAndMessaging::SW_HIDE;

use crate::protocol::{
    take_frame, SessionRef, Ui, WorkerEnvelope, WorkerExpectedIdentity, WorkerRequest,
    WorkerResponse, MAX_WORKER_FRAME_BYTES,
};

const WORKER_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const WORKER_EXIT_GRACE: Duration = Duration::from_secs(1);

static PIPE_SEQUENCE: AtomicU64 = AtomicU64::new(0);

struct Handle(HANDLE);

impl Drop for Handle {
    fn drop(&mut self) {
        unsafe {
            if !self.0.is_null() && self.0 != INVALID_HANDLE_VALUE {
                CloseHandle(self.0);
            }
        }
    }
}

struct EnvironmentBlock(*mut c_void);

impl Drop for EnvironmentBlock {
    fn drop(&mut self) {
        unsafe {
            if !self.0.is_null() {
                DestroyEnvironmentBlock(self.0);
            }
        }
    }
}

fn wide(value: &str) -> Vec<u16> {
    value.encode_utf16().chain(Some(0)).collect()
}

fn pipe_name() -> String {
    let sequence = PIPE_SEQUENCE.fetch_add(1, Ordering::SeqCst);
    format!(
        r"\\.\pipe\rdp-launcher-user-{}-{sequence}",
        std::process::id()
    )
}

fn create_pipe(name: &str, configured_sid: &str) -> io::Result<Handle> {
    // PIPE_REJECT_REMOTE_CLIENTS keeps this transport local; the DACL admits
    // only the SYSTEM backend and the configured account.
    let sddl = wide(&format!("D:P(A;;GA;;;SY)(A;;GA;;;{configured_sid})"));
    let mut descriptor = ptr::null_mut();
    unsafe {
        if ConvertStringSecurityDescriptorToSecurityDescriptorW(
            sddl.as_ptr(),
            1,
            &mut descriptor,
            ptr::null_mut(),
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
    }
    let attributes = SECURITY_ATTRIBUTES {
        nLength: mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
        lpSecurityDescriptor: descriptor,
        bInheritHandle: 0,
    };
    let handle = unsafe {
        CreateNamedPipeW(
            wide(name).as_ptr(),
            PIPE_ACCESS_DUPLEX | FILE_FLAG_OVERLAPPED,
            PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS,
            1,
            64 * 1024,
            64 * 1024,
            0,
            &attributes,
        )
    };
    let error = io::Error::last_os_error();
    unsafe {
        LocalFree(descriptor);
    }
    if handle == INVALID_HANDLE_VALUE {
        return Err(error);
    }
    Ok(Handle(handle))
}

fn event() -> io::Result<Handle> {
    let handle = unsafe { CreateEventW(ptr::null(), 1, 0, ptr::null()) };
    if handle.is_null() {
        return Err(io::Error::last_os_error());
    }
    Ok(Handle(handle))
}

fn remaining(deadline: Instant) -> io::Result<Duration> {
    let remaining = deadline.saturating_duration_since(Instant::now());
    if remaining.is_zero() {
        Err(io::Error::new(
            io::ErrorKind::TimedOut,
            "user worker IPC deadline exceeded",
        ))
    } else {
        Ok(remaining)
    }
}

/// Wait for one pending overlapped operation. The budget is fixed before the
/// native call issues the I/O, not after it is already pending.
///
/// On timeout or wait failure, cancellation is drained before the caller's
/// OVERLAPPED and buffer leave scope. `CancelIoEx` only requests cancellation;
/// returning immediately would let the kernel write into dropped stack state.
fn wait_overlapped(
    pipe: &Handle,
    event: &Handle,
    overlapped: &OVERLAPPED,
    budget_ms: u32,
    timeout_message: &'static str,
) -> io::Result<()> {
    match unsafe { WaitForSingleObject(event.0, budget_ms) } {
        WAIT_OBJECT_0 => Ok(()),
        _ => {
            unsafe {
                CancelIoEx(pipe.0, overlapped);
            }
            // Cancellation completion is required for memory safety. For a
            // local byte-mode pipe this drain is expected to be prompt, but it
            // is intentionally not claimable as a new deadline.
            let mut transferred = 0u32;
            unsafe {
                GetOverlappedResult(pipe.0, overlapped, &mut transferred, 1);
            }
            Err(io::Error::new(io::ErrorKind::TimedOut, timeout_message))
        }
    }
}

fn connect_server(pipe: &Handle, event: &Handle, deadline: Instant) -> io::Result<()> {
    let budget = remaining(deadline)?.as_millis().min(u32::MAX as u128) as u32;
    let mut overlapped: OVERLAPPED = unsafe { mem::zeroed() };
    overlapped.hEvent = event.0;
    let connected = unsafe { ConnectNamedPipe(pipe.0, &mut overlapped) };
    if connected != 0 {
        return Ok(());
    }
    let error = io::Error::last_os_error();
    if error.raw_os_error() == Some(ERROR_PIPE_CONNECTED as i32) {
        return Ok(());
    }
    if error.raw_os_error() != Some(ERROR_IO_PENDING as i32) {
        return Err(error);
    }
    wait_overlapped(
        pipe,
        event,
        &overlapped,
        budget,
        "user worker did not connect to its pipe",
    )?;
    let mut transferred = 0u32;
    if unsafe { GetOverlappedResult(pipe.0, &overlapped, &mut transferred, 0) } == 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn read_some(
    pipe: &Handle,
    event: &Handle,
    buffer: &mut [u8],
    deadline: Instant,
) -> io::Result<usize> {
    let budget = remaining(deadline)?.as_millis().min(u32::MAX as u128) as u32;
    let mut overlapped: OVERLAPPED = unsafe { mem::zeroed() };
    overlapped.hEvent = event.0;
    unsafe {
        ResetEvent(event.0);
    }
    let mut read = 0u32;
    let ok = unsafe {
        ReadFile(
            pipe.0,
            buffer.as_mut_ptr(),
            buffer.len() as u32,
            &mut read,
            &mut overlapped,
        )
    };
    if ok == 0 {
        let error = io::Error::last_os_error();
        if error.raw_os_error() != Some(ERROR_IO_PENDING as i32) {
            return Err(error);
        }
        wait_overlapped(
            pipe,
            event,
            &overlapped,
            budget,
            "user worker response timed out",
        )?;
        if unsafe { GetOverlappedResult(pipe.0, &overlapped, &mut read, 0) } == 0 {
            return Err(io::Error::last_os_error());
        }
    }
    Ok(read as usize)
}

fn write_all(pipe: &Handle, event: &Handle, mut bytes: &[u8], deadline: Instant) -> io::Result<()> {
    while !bytes.is_empty() {
        let budget = remaining(deadline)?.as_millis().min(u32::MAX as u128) as u32;
        let mut overlapped: OVERLAPPED = unsafe { mem::zeroed() };
        overlapped.hEvent = event.0;
        unsafe {
            ResetEvent(event.0);
        }
        let mut written = 0u32;
        let ok = unsafe {
            WriteFile(
                pipe.0,
                bytes.as_ptr(),
                bytes.len() as u32,
                &mut written,
                &mut overlapped,
            )
        };
        if ok == 0 {
            let error = io::Error::last_os_error();
            if error.raw_os_error() != Some(ERROR_IO_PENDING as i32) {
                return Err(error);
            }
            wait_overlapped(
                pipe,
                event,
                &overlapped,
                budget,
                "user worker request write timed out",
            )?;
            if unsafe { GetOverlappedResult(pipe.0, &overlapped, &mut written, 0) } == 0 {
                return Err(io::Error::last_os_error());
            }
        }
        if written == 0 {
            return Err(io::Error::new(
                io::ErrorKind::WriteZero,
                "user worker pipe accepted zero bytes",
            ));
        }
        bytes = &bytes[written as usize..];
    }
    Ok(())
}

fn read_frame(pipe: &Handle, event: &Handle, deadline: Instant, max: usize) -> io::Result<Vec<u8>> {
    let mut buffered = Vec::new();
    let mut chunk = [0u8; 8192];
    loop {
        if let Some(frame) = take_frame(&mut buffered, max).map_err(io::Error::other)? {
            return Ok(frame);
        }
        let read = read_some(pipe, event, &mut chunk, deadline)?;
        if read == 0 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "user worker closed its pipe before a complete response",
            ));
        }
        buffered.extend_from_slice(&chunk[..read]);
    }
}

fn spawn_user_worker(session_id: u32, name: &str) -> io::Result<Handle> {
    unsafe {
        let mut raw_token: HANDLE = ptr::null_mut();
        if WTSQueryUserToken(session_id, &mut raw_token) == 0 {
            return Err(io::Error::last_os_error());
        }
        let token = Handle(raw_token);
        let mut raw_environment: *mut c_void = ptr::null_mut();
        if CreateEnvironmentBlock(&mut raw_environment, token.0, 0) == 0 {
            return Err(io::Error::last_os_error());
        }
        let environment = EnvironmentBlock(raw_environment);

        let executable = std::env::current_exe()?;
        let command_line = format!("\"{}\" user-worker --pipe {}", executable.display(), name);
        let mut command_line = wide(&command_line);
        let mut startup: STARTUPINFOW = mem::zeroed();
        startup.cb = mem::size_of::<STARTUPINFOW>() as u32;
        let mut desktop = wide(r"winsta0\default");
        startup.lpDesktop = desktop.as_mut_ptr();
        startup.dwFlags = STARTF_USESHOWWINDOW;
        startup.wShowWindow = SW_HIDE as u16;
        let mut process: PROCESS_INFORMATION = mem::zeroed();
        if CreateProcessAsUserW(
            token.0,
            wide(&executable.to_string_lossy()).as_ptr(),
            command_line.as_mut_ptr(),
            ptr::null(),
            ptr::null(),
            0,
            CREATE_UNICODE_ENVIRONMENT | CREATE_NO_WINDOW,
            environment.0,
            ptr::null(),
            &startup,
            &mut process,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        if !process.hThread.is_null() {
            CloseHandle(process.hThread);
        }
        Ok(Handle(process.hProcess))
    }
}

fn finish_process(process: &Handle) {
    let deadline = Instant::now() + WORKER_EXIT_GRACE;
    loop {
        if unsafe { WaitForSingleObject(process.0, 50) } == WAIT_OBJECT_0 {
            return;
        }
        if Instant::now() >= deadline {
            unsafe {
                TerminateProcess(process.0, 1);
            }
            return;
        }
    }
}

/// Run one fixed user action in an already verified session and return the
/// worker's response. Every stage is bounded by `timeout`.
pub fn execute(
    session_id: u32,
    configured_sid: &str,
    reference: &SessionRef,
    request: WorkerRequest,
    timeout: Duration,
) -> io::Result<WorkerResponse> {
    let envelope = WorkerEnvelope {
        expected: Some(WorkerExpectedIdentity {
            reference: reference.clone(),
            sid: configured_sid.to_string(),
        }),
        request,
    };
    execute_envelope(session_id, configured_sid, &envelope, timeout)
}

fn execute_envelope(
    session_id: u32,
    configured_sid: &str,
    envelope: &WorkerEnvelope,
    timeout: Duration,
) -> io::Result<WorkerResponse> {
    let deadline = Instant::now() + timeout;
    remaining(deadline)?;
    let name = pipe_name();
    let pipe = create_pipe(&name, configured_sid)?;
    let event = event()?;
    let process = spawn_user_worker(session_id, &name)?;

    let result = (|| -> io::Result<WorkerResponse> {
        // Native process creation is not cancellable. If the budget expired
        // after it returned, do not submit the business frame at all.
        remaining(deadline)?;
        connect_server(&pipe, &event, deadline)?;
        let mut frame = serde_json::to_vec(envelope).map_err(io::Error::other)?;
        frame.push(b'\n');
        write_all(&pipe, &event, &frame, deadline)?;
        let response = read_frame(&pipe, &event, deadline, MAX_WORKER_FRAME_BYTES)?;
        let response: WorkerResponse =
            serde_json::from_slice(&response).map_err(io::Error::other)?;
        Ok(response)
    })();
    finish_process(&process);
    result
}

pub fn observe_ui(session_id: u32, configured_sid: &str, timeout: Duration) -> io::Result<Ui> {
    let envelope = WorkerEnvelope {
        expected: None,
        request: WorkerRequest::ObserveUi,
    };
    let response = execute_envelope(session_id, configured_sid, &envelope, timeout)?;
    if response.status != "ok" {
        return Err(io::Error::other(
            response
                .message
                .unwrap_or_else(|| "user worker observation failed".into()),
        ));
    }
    response
        .ui
        .ok_or_else(|| io::Error::other("user worker returned no UI activity"))
}

fn connect_client(name: &str, deadline: Instant) -> io::Result<Handle> {
    let name = wide(name);
    loop {
        let handle = unsafe {
            CreateFileW(
                name.as_ptr(),
                GENERIC_READ | GENERIC_WRITE,
                0,
                ptr::null(),
                OPEN_EXISTING,
                0,
                ptr::null_mut(),
            )
        };
        if handle != INVALID_HANDLE_VALUE {
            return Ok(Handle(handle));
        }
        let error = io::Error::last_os_error();
        if Instant::now() >= deadline {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                format!("user worker could not open its pipe: {error}"),
            ));
        }
        if error.raw_os_error() == Some(ERROR_PIPE_BUSY as i32) {
            unsafe {
                WaitNamedPipeW(name.as_ptr(), 100);
            }
        } else {
            std::thread::sleep(Duration::from_millis(20));
        }
    }
}

fn read_client_frame(pipe: &Handle, deadline: Instant) -> io::Result<Vec<u8>> {
    let mut buffered = Vec::new();
    let mut chunk = [0u8; 8192];
    loop {
        if let Some(frame) =
            take_frame(&mut buffered, MAX_WORKER_FRAME_BYTES).map_err(io::Error::other)?
        {
            return Ok(frame);
        }
        remaining(deadline)?;
        let mut available = 0u32;
        let peeked = unsafe {
            PeekNamedPipe(
                pipe.0,
                ptr::null_mut(),
                0,
                ptr::null_mut(),
                &mut available,
                ptr::null_mut(),
            )
        };
        if peeked == 0 {
            return Err(io::Error::last_os_error());
        }
        if available == 0 {
            std::thread::sleep(Duration::from_millis(10));
            continue;
        }
        let mut read = 0u32;
        let ok = unsafe {
            ReadFile(
                pipe.0,
                chunk.as_mut_ptr(),
                chunk.len() as u32,
                &mut read,
                ptr::null_mut(),
            )
        };
        if ok == 0 || read == 0 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "user worker pipe closed before a complete request",
            ));
        }
        buffered.extend_from_slice(&chunk[..read as usize]);
    }
}

fn write_client_all(pipe: &Handle, bytes: &[u8]) -> io::Result<()> {
    let mut written = 0usize;
    while written < bytes.len() {
        let mut count = 0u32;
        let ok = unsafe {
            WriteFile(
                pipe.0,
                bytes[written..].as_ptr(),
                (bytes.len() - written) as u32,
                &mut count,
                ptr::null_mut(),
            )
        };
        if ok == 0 || count == 0 {
            return Err(io::Error::last_os_error());
        }
        written += count as usize;
    }
    Ok(())
}

fn validate_worker_request(
    expected: Option<&WorkerExpectedIdentity>,
    request: &WorkerRequest,
) -> Result<(), String> {
    if matches!(request, WorkerRequest::ObserveUi) {
        // Internal UI observation is display-only; it never authorizes an
        // action and is allowed to omit the expected logon identity.
        return Ok(());
    }
    let expected = expected
        .ok_or_else(|| "user worker action requires expected SessionRef and SID".to_string())?;
    crate::session::verify_worker_expected(expected)
        .map_err(|error| format!("user worker identity check failed: {error}"))
}

fn execute_in_worker(request: WorkerRequest) -> WorkerResponse {
    match request {
        WorkerRequest::Run {
            target,
            params,
            working_dir,
        } => match crate::launch_application(&target, params.as_deref(), working_dir.as_deref()) {
            Ok(()) => WorkerResponse::ok(),
            Err(error) => WorkerResponse::error(error),
        },
        WorkerRequest::Open { file } => match crate::launch_application(&file, None, None) {
            Ok(()) => WorkerResponse::ok(),
            Err(error) => WorkerResponse::error(error),
        },
        WorkerRequest::ListApps { with_icons } => WorkerResponse {
            apps: Some(crate::scan_shortcuts(with_icons)),
            ..WorkerResponse::ok()
        },
        WorkerRequest::GetMappedDrives => WorkerResponse {
            drives: Some(crate::query_mapped_network_drives()),
            ..WorkerResponse::ok()
        },
        WorkerRequest::ObserveUi => WorkerResponse {
            ui: Some(crate::session_ui::observe()),
            ..WorkerResponse::ok()
        },
        WorkerRequest::SessionStatus => match crate::session::worker_status() {
            Ok(status) => WorkerResponse {
                session: Some(status),
                ..WorkerResponse::ok()
            },
            Err(error) => WorkerResponse::error(error.to_string()),
        },
    }
}

/// Entry point for the on-demand user worker process. It connects to the
/// backend-created pipe, handles exactly one request, returns one response and
/// exits. It intentionally has no host-facing listener.
pub fn run_client(name: &str) -> io::Result<()> {
    // The backend runs as SYSTEM. ShellExecute must not inherit SYSTEM's cwd,
    // so use the user profile loaded into this worker's environment.
    if let Some(profile) = std::env::var_os("USERPROFILE") {
        let profile = std::path::PathBuf::from(profile);
        if profile.is_dir() {
            let _ = std::env::set_current_dir(profile);
        }
    }
    let deadline = Instant::now() + WORKER_CONNECT_TIMEOUT;
    let pipe = connect_client(name, deadline)?;
    let frame = read_client_frame(&pipe, deadline)?;
    let envelope: WorkerEnvelope = serde_json::from_slice(&frame).map_err(io::Error::other)?;

    let com_initialized =
        unsafe { CoInitializeEx(ptr::null(), COINIT_APARTMENTTHREADED as u32) } >= 0;
    let response = match validate_worker_request(envelope.expected.as_ref(), &envelope.request) {
        Ok(()) => execute_in_worker(envelope.request),
        Err(error) => WorkerResponse::error(error),
    };
    if com_initialized {
        unsafe {
            CoUninitialize();
        }
    }

    let mut frame = serde_json::to_vec(&response).map_err(io::Error::other)?;
    frame.push(b'\n');
    write_client_all(&pipe, &frame)?;
    unsafe {
        FlushFileBuffers(pipe.0);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generated_pipe_names_are_unique_local_pipe_paths() {
        let first = pipe_name();
        let second = pipe_name();
        assert!(first.starts_with(r"\\.\pipe\rdp-launcher-user-"));
        assert!(second.starts_with(r"\\.\pipe\rdp-launcher-user-"));
        assert_ne!(first, second);
        assert!(!first.contains('\n'));
    }

    #[test]
    fn business_worker_actions_require_expected_identity_but_ui_observation_does_not() {
        let run = WorkerRequest::Run {
            target: "notepad.exe".into(),
            params: None,
            working_dir: None,
        };
        let error = validate_worker_request(None, &run).expect_err("unbound business action");
        assert!(error.contains("requires expected SessionRef"), "{error}");
        assert!(validate_worker_request(None, &WorkerRequest::ObserveUi).is_ok());
    }

    #[test]
    fn worker_response_round_trip_keeps_optional_payloads() {
        let response = WorkerResponse {
            drives: Some(vec![("F:".into(), r"\\server\share".into())]),
            ..WorkerResponse::ok()
        };
        let encoded = serde_json::to_vec(&response).unwrap();
        let decoded: WorkerResponse = serde_json::from_slice(&encoded).unwrap();
        assert_eq!(decoded.status, "ok");
        assert_eq!(
            decoded.drives,
            Some(vec![("F:".to_string(), r"\\server\share".to_string())])
        );
        assert!(decoded.apps.is_none());
    }
}
