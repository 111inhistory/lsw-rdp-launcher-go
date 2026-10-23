//! WTS session snapshot, logon-instance identity and SYSTEM logoff command.
//!
//! Session 0 is never a user target. A session number alone is not an
//! identity: every executable reference also carries the logon
//! AuthenticationId and the current boot identifier. The configured account is
//! matched by SID, never by a possibly duplicated display name.
use std::io;
use std::mem;
use std::net::IpAddr;
use std::path::PathBuf;
use std::ptr;
use std::slice;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use windows_sys::core::GUID;
use windows_sys::Wdk::System::SystemInformation::NtQuerySystemInformation;
use windows_sys::Win32::Foundation::*;
use windows_sys::Win32::Security::Authorization::{ConvertSidToStringSidW, ConvertStringSidToSidW};
use windows_sys::Win32::Security::*;
use windows_sys::Win32::System::LibraryLoader::GetModuleHandleW;
use windows_sys::Win32::System::RemoteDesktop::*;
use windows_sys::Win32::System::Threading::*;
use windows_sys::Win32::UI::WindowsAndMessaging::*;

use crate::protocol::{
    apply_observation, format_authentication_id, format_client_address, format_guid, invalidate,
    verify_worker_identity, Observation, SessionEntry, SessionRef, Snapshot, WorkerSessionStatus,
};
use crate::worker;

/// Overall budget for one logoff request, including command wait and the final
/// disappearance confirmation. The command never receives a second budget.
pub const LOGOFF_TIMEOUT: Duration = Duration::from_secs(30);
/// The snapshot refresh query must be bounded even when a WTS call hangs.
pub const SNAPSHOT_QUERY_TIMEOUT: Duration = Duration::from_secs(20);
/// A caller waits slightly longer than the refresh worker's own timeout so it
/// normally observes a published fresh/stale result instead of racing it.
pub const SNAPSHOT_WAIT_TIMEOUT: Duration = Duration::from_secs(22);
const SNAPSHOT_INTERVAL: Duration = Duration::from_secs(60);
/// UI activity is optional display data, so it must not delay the snapshot
/// indefinitely. At most this many sessions are sampled per refresh.
const UI_PROBE_BUDGET: Duration = Duration::from_secs(4);
const UI_PROBE_TIMEOUT: Duration = Duration::from_secs(3);
const MAX_UI_SESSIONS: usize = 4;

const SYSTEM_BOOT_ENVIRONMENT_INFORMATION_CLASS: i32 = 90;

pub struct Access {
    pub user: String,
    pub host: IpAddr,
    pub sid: String,
}

impl Access {
    pub fn new(user: String, host: IpAddr) -> io::Result<Self> {
        let sid = resolve_sid(&user)?;
        Ok(Self { user, host, sid })
    }

    pub fn allows_host(&self, peer: IpAddr) -> bool {
        self.host == peer
    }

    pub fn allows(&self, peer: IpAddr, user: &str) -> bool {
        self.allows_host(peer) && self.user.eq_ignore_ascii_case(user)
    }

    /// `inspect_sessions` may omit the user because the backend is already
    /// bound to one configured account; when present it must still match.
    pub fn allows_optional_user(&self, peer: IpAddr, user: Option<&str>) -> bool {
        self.allows_host(peer)
            && user
                .map(|user| self.user.eq_ignore_ascii_case(user))
                .unwrap_or(true)
    }
}

pub struct VerifiedUserToken(HANDLE);

impl VerifiedUserToken {
    pub(crate) fn as_raw(&self) -> HANDLE {
        self.0
    }
}

impl Drop for VerifiedUserToken {
    fn drop(&mut self) {
        unsafe {
            if !self.0.is_null() && self.0 != INVALID_HANDLE_VALUE {
                CloseHandle(self.0);
            }
        }
    }
}



fn wide(value: &str) -> Vec<u16> {
    value.encode_utf16().chain(Some(0)).collect()
}

fn text(buffer: &[u16]) -> String {
    let length = buffer
        .iter()
        .position(|value| *value == 0)
        .unwrap_or(buffer.len());
    String::from_utf16_lossy(&buffer[..length])
}

unsafe fn sid_to_string(sid: PSID) -> io::Result<String> {
    let mut raw = ptr::null_mut();
    if ConvertSidToStringSidW(sid, &mut raw) == 0 {
        return Err(io::Error::last_os_error());
    }
    let mut length = 0;
    while *raw.add(length) != 0 {
        length += 1;
    }
    let value = String::from_utf16_lossy(slice::from_raw_parts(raw, length));
    LocalFree(raw.cast());
    Ok(value)
}

pub fn resolve_sid(account: &str) -> io::Result<String> {
    let account = wide(account);
    let mut sid_size = 0u32;
    let mut domain_size = 0u32;
    let mut kind = 0i32;
    unsafe {
        LookupAccountNameW(
            ptr::null(),
            account.as_ptr(),
            ptr::null_mut(),
            &mut sid_size,
            ptr::null_mut(),
            &mut domain_size,
            &mut kind,
        );
    }
    if sid_size == 0 {
        return Err(io::Error::new(
            io::ErrorKind::NotFound,
            format!(
                "cannot resolve configured session account {} to a SID",
                String::from_utf16_lossy(&account[..account.len().saturating_sub(1)])
            ),
        ));
    }
    let mut sid = vec![0u8; sid_size as usize];
    let mut domain = vec![0u16; domain_size as usize];
    unsafe {
        if LookupAccountNameW(
            ptr::null(),
            account.as_ptr(),
            sid.as_mut_ptr().cast(),
            &mut sid_size,
            domain.as_mut_ptr(),
            &mut domain_size,
            &mut kind,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        sid_to_string(sid.as_mut_ptr().cast())
    }
}

struct TokenIdentity {
    session_id: u32,
    sid: String,
    authentication_id: Option<String>,
}

fn inspect_token_handle(token: HANDLE) -> io::Result<TokenIdentity> {
    unsafe {
        let mut session_id = 0u32;
        let mut session_len = 0u32;
        if GetTokenInformation(
            token,
            TokenSessionId,
            (&mut session_id as *mut u32).cast(),
            mem::size_of::<u32>() as u32,
            &mut session_len,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }

        let mut needed = 0u32;
        GetTokenInformation(token, TokenUser, ptr::null_mut(), 0, &mut needed);
        if needed == 0 {
            return Err(io::Error::last_os_error());
        }
        let mut user = vec![0u8; needed as usize];
        if GetTokenInformation(
            token,
            TokenUser,
            user.as_mut_ptr().cast(),
            needed,
            &mut needed,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        let sid = sid_to_string(
            ptr::read_unaligned(user.as_ptr() as *const TOKEN_USER)
                .User
                .Sid,
        )?;

        let mut statistics: TOKEN_STATISTICS = mem::zeroed();
        let mut length = 0u32;
        let authentication_id = (GetTokenInformation(
            token,
            TokenStatistics,
            (&mut statistics as *mut TOKEN_STATISTICS).cast(),
            mem::size_of::<TOKEN_STATISTICS>() as u32,
            &mut length,
        ) != 0)
            .then(|| {
                format_authentication_id(
                    statistics.AuthenticationId.HighPart,
                    statistics.AuthenticationId.LowPart,
                )
            });
        Ok(TokenIdentity {
            session_id,
            sid,
            authentication_id,
        })
    }
}

fn open_session_token(session_id: u32) -> io::Result<VerifiedUserToken> {
    unsafe {
        let mut raw_token = ptr::null_mut();
        if WTSQueryUserToken(session_id, &mut raw_token) == 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(VerifiedUserToken(raw_token))
    }
}

fn token_identity(session_id: u32) -> io::Result<TokenIdentity> {
    let token = open_session_token(session_id)?;
    inspect_token_handle(token.0)
}

#[repr(C)]
struct BootEnvironmentInformation {
    boot_identifier: GUID,
    firmware_type: u32,
    boot_flags: u64,
}

fn account_name_from_sid(sid_string: &str) -> io::Result<String> {
    let sid_text = wide(sid_string);
    let mut sid = ptr::null_mut();
    unsafe {
        if ConvertStringSidToSidW(sid_text.as_ptr(), &mut sid) == 0 {
            return Err(io::Error::last_os_error());
        }
        let mut name = [0u16; 256];
        let mut name_length = name.len() as u32;
        let mut domain = [0u16; 256];
        let mut domain_length = domain.len() as u32;
        let mut kind = 0i32;
        let ok = LookupAccountSidW(
            ptr::null(),
            sid,
            name.as_mut_ptr(),
            &mut name_length,
            domain.as_mut_ptr(),
            &mut domain_length,
            &mut kind,
        );
        let error = io::Error::last_os_error();
        LocalFree(sid.cast());
        if ok == 0 {
            return Err(error);
        }
        let domain = text(&domain[..domain_length as usize]);
        let name = text(&name[..name_length as usize]);
        Ok(if domain.is_empty() {
            name
        } else {
            format!(r"{domain}\{name}")
        })
    }
}

fn current_process_identity() -> io::Result<(String, String)> {
    unsafe {
        let mut token = ptr::null_mut();
        if OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) == 0 {
            return Err(io::Error::last_os_error());
        }
        let token = VerifiedUserToken(token);

        let mut needed = 0u32;
        GetTokenInformation(token.0, TokenUser, ptr::null_mut(), 0, &mut needed);
        if needed == 0 {
            return Err(io::Error::last_os_error());
        }
        let mut user = vec![0u8; needed as usize];
        if GetTokenInformation(
            token.0,
            TokenUser,
            user.as_mut_ptr().cast(),
            needed,
            &mut needed,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        let sid = sid_to_string(
            ptr::read_unaligned(user.as_ptr() as *const TOKEN_USER)
                .User
                .Sid,
        )?;

        let mut statistics: TOKEN_STATISTICS = mem::zeroed();
        let mut length = 0u32;
        if GetTokenInformation(
            token.0,
            TokenStatistics,
            (&mut statistics as *mut TOKEN_STATISTICS).cast(),
            mem::size_of::<TOKEN_STATISTICS>() as u32,
            &mut length,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        let authentication_id = format_authentication_id(
            statistics.AuthenticationId.HighPart,
            statistics.AuthenticationId.LowPart,
        );
        Ok((sid, authentication_id))
    }
}

fn current_session_id() -> io::Result<u32> {
    let mut session_id = 0u32;
    if unsafe { ProcessIdToSessionId(GetCurrentProcessId(), &mut session_id) } == 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(session_id)
}

/// Read the worker's own complete logon identity. The SYSTEM backend already
/// verified the target session before spawning, so this only reports the
/// session the worker actually landed in.
pub fn current_worker_identity() -> io::Result<(SessionRef, String)> {
    let session_id = current_session_id()?;
    let (sid, authentication_id) = current_process_identity()?;
    let reference = SessionRef {
        session_id,
        authentication_id,
        boot_identifier: boot_identifier()?,
    };
    reference.validate().map_err(io::Error::other)?;
    Ok((reference, sid))
}

/// Report the worker's actual execution session and reference as observed
/// data about the verified session it was started in.
pub fn worker_status() -> io::Result<WorkerSessionStatus> {
    let (reference, sid) = current_worker_identity()?;
    let session = raw_session(reference.session_id)?;
    if session.user.is_empty() {
        return Err(io::Error::other(
            "user worker has no logged-on session identity",
        ));
    }
    let user = account_name_from_sid(&sid).unwrap_or_else(|_| session.user.clone());
    let ui = crate::session_ui::observe();
    Ok(WorkerSessionStatus {
        reference: reference.clone(),
        session_id: reference.session_id,
        session_active: session.state == WTSActive,
        sid,
        user,
        windows: ui.windows,
        tray_icons: ui.tray_icons,
        note: None,
    })
}

pub fn boot_identifier() -> io::Result<String> {
    let mut information: BootEnvironmentInformation = unsafe { mem::zeroed() };
    let mut returned = 0u32;
    let status = unsafe {
        NtQuerySystemInformation(
            SYSTEM_BOOT_ENVIRONMENT_INFORMATION_CLASS,
            (&mut information as *mut BootEnvironmentInformation).cast(),
            mem::size_of::<BootEnvironmentInformation>() as u32,
            &mut returned,
        )
    };
    if status < 0 {
        return Err(io::Error::other(format!(
            "SystemBootEnvironmentInformation failed with NTSTATUS {status:#x}"
        )));
    }
    if returned < mem::size_of::<GUID>() as u32 {
        return Err(io::Error::other(
            "SystemBootEnvironmentInformation returned a short identifier",
        ));
    }
    let guid = information.boot_identifier;
    Ok(format_guid(guid.data1, guid.data2, guid.data3, guid.data4))
}

struct RawSession {
    user: String,
    domain: String,
    logon: i64,
    connect: i64,
    state: i32,
}

fn raw_session(id: u32) -> io::Result<RawSession> {
    unsafe {
        let mut buffer = ptr::null_mut();
        let mut size = 0u32;
        if WTSQuerySessionInformationW(
            ptr::null_mut(),
            id,
            WTSSessionInfoEx,
            &mut buffer,
            &mut size,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        if size < mem::size_of::<WTSINFOEXW>() as u32 {
            WTSFreeMemory(buffer.cast());
            return Err(io::Error::other("short WTS session info"));
        }
        let information = *(buffer as *const WTSINFOEXW);
        WTSFreeMemory(buffer.cast());
        if information.Level != 1 {
            return Err(io::Error::other("unsupported WTS session info level"));
        }
        let information = information.Data.WTSInfoExLevel1;
        Ok(RawSession {
            user: text(&information.UserName),
            domain: text(&information.DomainName),
            logon: information.LogonTime,
            connect: information.ConnectTime,
            state: information.SessionState,
        })
    }
}

fn session_ids() -> io::Result<Vec<u32>> {
    unsafe {
        let mut data = ptr::null_mut();
        let mut count = 0u32;
        if WTSEnumerateSessionsW(ptr::null_mut(), 0, 1, &mut data, &mut count) == 0 {
            return Err(io::Error::last_os_error());
        }
        let ids = slice::from_raw_parts(data, count as usize)
            .iter()
            .filter(|session| session.SessionId != 0)
            .map(|session| session.SessionId)
            .collect();
        WTSFreeMemory(data.cast());
        Ok(ids)
    }
}

fn session_present(id: u32) -> io::Result<bool> {
    Ok(session_ids()?.contains(&id))
}

fn client_name(id: u32) -> io::Result<String> {
    unsafe {
        let mut buffer = ptr::null_mut();
        let mut bytes = 0u32;
        if WTSQuerySessionInformationW(ptr::null_mut(), id, WTSClientName, &mut buffer, &mut bytes)
            == 0
        {
            return Err(io::Error::last_os_error());
        }
        let units = (bytes as usize / mem::size_of::<u16>()).max(1);
        let value = text(slice::from_raw_parts(buffer, units));
        WTSFreeMemory(buffer.cast());
        Ok(value)
    }
}

fn client_address(id: u32) -> io::Result<String> {
    unsafe {
        let mut buffer = ptr::null_mut();
        let mut bytes = 0u32;
        if WTSQuerySessionInformationW(
            ptr::null_mut(),
            id,
            WTSClientAddress,
            &mut buffer,
            &mut bytes,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        if bytes < mem::size_of::<WTS_CLIENT_ADDRESS>() as u32 {
            WTSFreeMemory(buffer.cast());
            return Err(io::Error::other("short WTS client address"));
        }
        let address = *(buffer as *const WTS_CLIENT_ADDRESS);
        WTSFreeMemory(buffer.cast());
        Ok(format_client_address(
            address.AddressFamily,
            &address.Address,
        ))
    }
}

/// Build the full snapshot of logons for the configured account SID. A session
/// whose identity cannot be read remains absent from the list (it cannot be
/// attributed safely), and the snapshot is marked not fresh.
pub fn observe(configured_sid: &str) -> io::Result<Observation> {
    let boot = boot_identifier()?;
    let ids = session_ids()?;
    let mut complete = true;
    let mut sessions = Vec::new();
    let ui_budget = Instant::now() + UI_PROBE_BUDGET;
    let mut ui_sampled = 0usize;

    for id in ids {
        let raw = match raw_session(id) {
            Ok(raw) => raw,
            Err(error) => {
                complete = false;
                log::warn!("WTS session {id} info unavailable: {error}");
                continue;
            }
        };
        if raw.user.is_empty() {
            continue;
        }
        let identity = match token_identity(id) {
            Ok(identity) => identity,
            Err(error) => {
                complete = false;
                log::warn!("WTS session {id} token identity unavailable: {error}");
                continue;
            }
        };
        if identity.sid != configured_sid {
            continue;
        }

        let authentication_id = identity.authentication_id.unwrap_or_default();
        if authentication_id.is_empty() {
            complete = false;
        }
        let client_name = client_name(id).unwrap_or_else(|error| {
            log::debug!("WTS session {id} client name unavailable: {error}");
            String::new()
        });
        let client_address = client_address(id).unwrap_or_else(|error| {
            log::debug!("WTS session {id} client address unavailable: {error}");
            String::new()
        });

        let mut note = if authentication_id.is_empty() || boot.is_empty() {
            Some("session identity incomplete; dispatch and logoff disabled".to_string())
        } else {
            None
        };
        let mut windows = None;
        let mut tray_icons = None;
        if ui_sampled < MAX_UI_SESSIONS && Instant::now() < ui_budget {
            let remaining = ui_budget.saturating_duration_since(Instant::now());
            let timeout = remaining.min(UI_PROBE_TIMEOUT);
            match worker::observe_ui(id, configured_sid, timeout) {
                Ok(ui) => {
                    windows = ui.windows;
                    tray_icons = ui.tray_icons;
                }
                Err(error) => {
                    log::debug!("WTS session {id} UI activity unavailable: {error}");
                    note = Some(match note {
                        Some(existing) => format!("{existing}; UI activity unavailable"),
                        None => "UI activity unavailable".to_string(),
                    });
                }
            }
            ui_sampled += 1;
        }

        sessions.push(SessionEntry {
            reference: SessionRef {
                session_id: id,
                authentication_id,
                boot_identifier: boot.clone(),
            },
            sid: identity.sid,
            user: raw.user,
            domain: raw.domain,
            state: raw.state,
            client_name,
            client_address,
            logon_time: raw.logon,
            connect_time: raw.connect,
            windows,
            tray_icons,
            note,
        });
    }
    sessions.sort_by_key(|session| session.reference.session_id);
    Ok(Observation {
        fresh: complete && !boot.is_empty(),
        sessions,
    })
}

/// Open the WTS user token for the target session and verify that the exact
/// opened token handle (Session ID, SID, AuthenticationId LUID) and current
/// boot identifier match reference and configured_sid. Callers pass this
/// same returned token handle directly to CreateEnvironmentBlock and
/// CreateProcessAsUserW, eliminating any re-query window.
pub fn open_verified_user_token(
    reference: &SessionRef,
    configured_sid: &str,
) -> io::Result<VerifiedUserToken> {
    reference.validate().map_err(io::Error::other)?;
    let boot = boot_identifier()?;
    if !session_present(reference.session_id)? {
        return Err(io::Error::other("target session no longer exists"));
    }
    let raw = raw_session(reference.session_id)?;
    if raw.user.is_empty() {
        return Err(io::Error::other("target session has no logged-on user"));
    }
    let token = open_session_token(reference.session_id)?;
    let identity = inspect_token_handle(token.0)?;
    verify_worker_identity(
        reference,
        configured_sid,
        identity.session_id,
        &identity.sid,
        identity.authentication_id.as_deref(),
        &boot,
    )
    .map_err(io::Error::other)?;
    Ok(token)
}

/// Final pre-execution identity check. Errors mean the target must not be
/// dispatched or logged off.
pub fn verify_reference(reference: &SessionRef, configured_sid: &str) -> io::Result<()> {
    open_verified_user_token(reference, configured_sid).map(|_| ())
}

#[derive(Debug, PartialEq, Eq)]
pub enum PollState {
    Gone,
    Same,
    Reused,
    Unknown,
}

/// Pure classification of one post-command observation for tests and the
/// polling loop.
pub fn classify_poll(
    present: bool,
    sid: Option<&str>,
    authentication_id: Option<&str>,
    configured_sid: &str,
    expected_authentication_id: &str,
) -> PollState {
    if !present {
        return PollState::Gone;
    }
    match (sid, authentication_id) {
        (Some(sid), Some(authentication_id)) if sid == configured_sid => {
            if authentication_id == expected_authentication_id {
                PollState::Same
            } else {
                PollState::Reused
            }
        }
        (Some(sid), Some(_)) if sid != configured_sid => PollState::Reused,
        _ => PollState::Unknown,
    }
}

fn poll_state(
    session_id: u32,
    configured_sid: &str,
    expected_authentication_id: &str,
) -> PollState {
    match session_present(session_id) {
        Ok(false) => PollState::Gone,
        Err(_) => PollState::Unknown,
        Ok(true) => match token_identity(session_id) {
            Ok(identity) => classify_poll(
                true,
                Some(&identity.sid),
                identity.authentication_id.as_deref(),
                configured_sid,
                expected_authentication_id,
            ),
            Err(_) => PollState::Unknown,
        },
    }
}

#[derive(Debug, PartialEq, Eq)]
pub enum LogoffResult {
    Confirmed(String),
    NotStarted(String),
    Failed(String),
    Unknown(String),
    Anomaly(String),
}

impl LogoffResult {
    pub fn outcome(&self) -> &'static str {
        match self {
            Self::Confirmed(_) => "confirmed",
            Self::NotStarted(_) => "not_started",
            Self::Failed(_) => "failed",
            Self::Unknown(_) => "unknown",
            Self::Anomaly(_) => "anomaly",
        }
    }

    pub fn message(self) -> String {
        match self {
            Self::Confirmed(message)
            | Self::NotStarted(message)
            | Self::Failed(message)
            | Self::Unknown(message)
            | Self::Anomaly(message) => message,
        }
    }
}

fn system_logoff_path() -> PathBuf {
    let root = std::env::var_os("SystemRoot")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(r"C:\Windows"));
    root.join("System32").join("logoff.exe")
}

/// Log off exactly one verified logon with the fixed system `logoff.exe`.
///
/// The command uses one numeric argument, runs in the Agent's existing SYSTEM /
/// Session 0 context, and is never retried. Exit code 0 is not evidence by
/// itself: the original SessionRef must disappear before the same operation
/// deadline. A reused Session ID is reported as an anomaly and no further
/// action is attempted.
///
/// The final identity query and post-command confirmation are native WTS calls
/// that cannot be cancelled. If a confirmation query blocks past the deadline,
/// this function can return late, but it still reports `Unknown` unless the
/// original logon was actually observed gone. No command is started after the
/// deadline has already expired.
pub fn logoff(configured_sid: &str, reference: &SessionRef) -> LogoffResult {
    use std::os::windows::process::CommandExt;
    use std::process::Command;

    let deadline = Instant::now() + LOGOFF_TIMEOUT;
    if let Err(error) = verify_reference(reference, configured_sid) {
        return LogoffResult::NotStarted(format!(
            "target session changed; logoff was not started: {error}"
        ));
    }
    if Instant::now() >= deadline {
        return LogoffResult::NotStarted(
            "target verification did not finish before the logoff deadline; logoff was not started"
                .to_string(),
        );
    }

    let path = system_logoff_path();
    let mut child = match Command::new(&path)
        .arg(reference.session_id.to_string())
        .creation_flags(0x0800_0000) // CREATE_NO_WINDOW; no shell, no extra privileges
        .spawn()
    {
        Ok(child) => child,
        Err(error) => {
            return LogoffResult::Failed(format!("failed to start {}: {error}", path.display()))
        }
    };

    let mut exit = None;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => {
                exit = Some(status);
                break;
            }
            Ok(None) => {}
            Err(error) => {
                return LogoffResult::Failed(format!("failed to observe logoff.exe: {error}"))
            }
        }
        if Instant::now() >= deadline {
            break;
        }
        std::thread::sleep(Duration::from_millis(50));
    }

    loop {
        match poll_state(
            reference.session_id,
            configured_sid,
            &reference.authentication_id,
        ) {
            PollState::Gone => {
                return LogoffResult::Confirmed(match exit {
                    Some(status) if status.success() => "session logoff confirmed".to_string(),
                    Some(status) => {
                        format!("session logoff confirmed after logoff.exe exited with {status}")
                    }
                    None => "session logoff confirmed before logoff.exe exited".to_string(),
                })
            }
            PollState::Reused => {
                return LogoffResult::Anomaly(
                    "session ID was reused by a different logon after logoff.exe was started; \
                     no further action was attempted"
                        .to_string(),
                )
            }
            PollState::Same | PollState::Unknown => {}
        }
        if Instant::now() >= deadline {
            break;
        }
        std::thread::sleep(Duration::from_millis(200));
    }

    match exit {
        Some(status) if !status.success() => LogoffResult::Failed(format!(
            "logoff.exe exited with {status}, but the original session reference was not \
             confirmed gone before the deadline"
        )),
        Some(_) => LogoffResult::Unknown(
            "logoff.exe exited successfully, but the original session reference was not \
             confirmed gone before the deadline"
                .to_string(),
        ),
        None => LogoffResult::Unknown(
            "logoff.exe did not exit before the 30 second deadline; outcome is unknown".to_string(),
        ),
    }
}

// WTS session-change notifications only wake the snapshot refresher. The
// callback never performs a full query.
static SESSION_SIGNAL: std::sync::OnceLock<tokio::sync::mpsc::Sender<()>> =
    std::sync::OnceLock::new();

unsafe extern "system" fn watch_proc(
    window: HWND,
    message: u32,
    wparam: usize,
    lparam: isize,
) -> isize {
    if message == WM_WTSSESSION_CHANGE {
        if let Some(signal) = SESSION_SIGNAL.get() {
            let _ = signal.try_send(());
        }
        return 0;
    }
    DefWindowProcW(window, message, wparam, lparam)
}

/// Start a hidden, message-only WTS observer for all sessions. The callback
/// only signals; snapshot queries run in the bounded refresher.
pub fn start_watcher(signal: tokio::sync::mpsc::Sender<()>) -> io::Result<()> {
    let _ = SESSION_SIGNAL.set(signal);
    std::thread::Builder::new()
        .name("session-watch".into())
        .spawn(|| unsafe {
            let class_name = wide("RdpLauncherSessionWatchClass");
            let window_name = wide("Rdp Launcher Session Watch");
            let class = WNDCLASSW {
                style: 0,
                lpfnWndProc: Some(watch_proc),
                cbClsExtra: 0,
                cbWndExtra: 0,
                hInstance: GetModuleHandleW(ptr::null()),
                hIcon: ptr::null_mut(),
                hCursor: ptr::null_mut(),
                hbrBackground: ptr::null_mut(),
                lpszMenuName: ptr::null(),
                lpszClassName: class_name.as_ptr(),
            };
            RegisterClassW(&class);
            let window = CreateWindowExW(
                0,
                class_name.as_ptr(),
                window_name.as_ptr(),
                0,
                0,
                0,
                0,
                0,
                -3_isize as HWND, // HWND_MESSAGE
                ptr::null_mut(),
                class.hInstance,
                ptr::null_mut(),
            );
            if window.is_null() {
                log::warn!("failed to create session watcher window");
                return;
            }
            if WTSRegisterSessionNotification(window, NOTIFY_FOR_ALL_SESSIONS) == 0 {
                log::warn!(
                    "WTSRegisterSessionNotification(all sessions) failed: {}",
                    io::Error::last_os_error()
                );
            }
            let mut message: MSG = mem::zeroed();
            while GetMessageW(&mut message, ptr::null_mut(), 0, 0) > 0 {
                TranslateMessage(&message);
                DispatchMessageW(&message);
            }
            WTSUnRegisterSessionNotification(window);
        })?;
    Ok(())
}

struct RefreshGate {
    in_flight: AtomicBool,
    next_attempt: AtomicU64,
    latest_attempt: AtomicU64,
}

impl RefreshGate {
    fn new() -> Self {
        Self {
            in_flight: AtomicBool::new(false),
            next_attempt: AtomicU64::new(0),
            latest_attempt: AtomicU64::new(0),
        }
    }
}

/// Owns the last published snapshot and one serial refresh loop. All WTS
/// notifications and inspect requests are coalesced into this loop.
pub struct SnapshotHub {
    state: tokio::sync::watch::Sender<Snapshot>,
    refresh: tokio::sync::mpsc::Sender<()>,
    completed: tokio::sync::watch::Receiver<u64>,
}

impl SnapshotHub {
    pub fn start(configured_sid: String, mut shutdown: tokio::sync::watch::Receiver<bool>) -> Self {
        let (state, _) = tokio::sync::watch::channel(Snapshot::default());
        let (refresh, mut requests) = tokio::sync::mpsc::channel::<()>(1);
        let (completed_tx, completed) = tokio::sync::watch::channel(0u64);
        let gate = Arc::new(RefreshGate::new());
        let state_task = state.clone();

        tokio::spawn(async move {
            let mut interval = tokio::time::interval(SNAPSHOT_INTERVAL);
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                tokio::select! {
                    _ = shutdown.changed() => break,
                    request = requests.recv() => {
                        if request.is_none() { break; }
                    }
                    _ = interval.tick() => {}
                }
                if *shutdown.borrow() {
                    break;
                }
                // Coalesce a burst of notifications into one bounded query.
                tokio::time::sleep(Duration::from_millis(50)).await;
                while requests.try_recv().is_ok() {}

                refresh_once(
                    &configured_sid,
                    &state_task,
                    Arc::clone(&gate),
                    SNAPSHOT_QUERY_TIMEOUT,
                )
                .await;
                let completed_now = *completed_tx.borrow();
                completed_tx.send_replace(completed_now.saturating_add(1));
            }
        });

        Self {
            state,
            refresh,
            completed,
        }
    }

    pub fn current(&self) -> Snapshot {
        self.state.borrow().clone()
    }

    pub fn subscribe(&self) -> tokio::sync::watch::Receiver<Snapshot> {
        self.state.subscribe()
    }

    /// Wake the refresher after a WTS session-change notification.
    pub fn signal(&self) -> tokio::sync::mpsc::Sender<()> {
        self.refresh.clone()
    }

    /// Request a refresh and wait up to `timeout` for one completed query. On a
    /// caller timeout the old snapshot is invalidated for display but is never
    /// used as a fresh dispatch target.
    pub async fn inspect(&self, timeout: Duration) -> Snapshot {
        let before = *self.completed.borrow();
        let _ = self.refresh.try_send(());
        let deadline = Instant::now() + timeout;
        let mut completed = self.completed.clone();
        loop {
            if *completed.borrow() > before {
                return self.current();
            }
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                break;
            }
            match tokio::time::timeout(remaining, completed.changed()).await {
                Ok(Ok(())) => {}
                _ => break,
            }
        }
        let previous = self.state.borrow().clone();
        let invalidated = invalidate(&previous);
        if invalidated != previous {
            self.state.send_replace(invalidated.clone());
        }
        invalidated
    }
}

async fn refresh_once(
    configured_sid: &str,
    state: &tokio::sync::watch::Sender<Snapshot>,
    gate: Arc<RefreshGate>,
    timeout: Duration,
) {
    let attempt = gate.next_attempt.fetch_add(1, Ordering::SeqCst) + 1;
    gate.latest_attempt.store(attempt, Ordering::SeqCst);
    let sid = configured_sid.to_string();
    let gate_task = Arc::clone(&gate);
    let task = tokio::task::spawn_blocking(move || {
        if gate_task
            .in_flight
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
            .is_err()
        {
            return Err("a previous WTS snapshot query is still running".to_string());
        }
        let result = observe(&sid);
        gate_task.in_flight.store(false, Ordering::SeqCst);
        if gate_task.latest_attempt.load(Ordering::SeqCst) != attempt {
            return Err("late WTS snapshot result discarded after timeout".to_string());
        }
        result.map_err(|error| error.to_string())
    });

    let result = match tokio::time::timeout(timeout, task).await {
        Ok(Ok(result)) => result,
        Ok(Err(error)) => Err(format!("WTS snapshot task failed: {error}")),
        Err(_) => {
            // Ensure the still-running blocking query can never publish its
            // late result into a newer revision.
            gate.latest_attempt
                .store(attempt.wrapping_add(1), Ordering::SeqCst);
            Err(format!(
                "WTS snapshot query exceeded {} seconds",
                timeout.as_secs()
            ))
        }
    };

    let previous = state.borrow().clone();
    let next = match result {
        Ok(observation) => apply_observation(&previous, observation),
        Err(error) => {
            log::warn!("WTS snapshot refresh failed: {error}");
            invalidate(&previous)
        }
    };
    if next != previous {
        state.send_replace(next);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn access_requires_both_configured_host_and_user() {
        let access = Access {
            user: "user".into(),
            host: "192.0.2.1".parse().unwrap(),
            sid: "S-1-5-21-1".into(),
        };
        assert!(access.allows("192.0.2.1".parse().unwrap(), "USER"));
        assert!(!access.allows("127.0.0.1".parse().unwrap(), "user"));
        assert!(!access.allows("192.0.2.1".parse().unwrap(), "other"));
        assert!(access.allows_optional_user("192.0.2.1".parse().unwrap(), None));
        assert!(access.allows_optional_user("192.0.2.1".parse().unwrap(), Some("USER")));
        assert!(!access.allows_optional_user("192.0.2.1".parse().unwrap(), Some("other")));
    }

    #[test]
    fn poll_classification_distinguishes_gone_same_reuse_and_unknown() {
        let sid = "S-1-5-21-1";
        let expected = "00000000000003E7";
        assert_eq!(
            classify_poll(false, None, None, sid, expected),
            PollState::Gone
        );
        assert_eq!(
            classify_poll(true, Some(sid), Some(expected), sid, expected),
            PollState::Same
        );
        assert_eq!(
            classify_poll(true, Some(sid), Some("00000000000003E8"), sid, expected),
            PollState::Reused
        );
        assert_eq!(
            classify_poll(true, Some("S-1-5-21-2"), Some(expected), sid, expected),
            PollState::Reused
        );
        // A lost identity read is unknown, never reused or gone.
        assert_eq!(
            classify_poll(true, Some(sid), None, sid, expected),
            PollState::Unknown
        );
        assert_eq!(
            classify_poll(true, None, Some(expected), sid, expected),
            PollState::Unknown
        );
    }

    #[test]
    fn logoff_outcomes_are_reported_without_false_success() {
        assert_eq!(LogoffResult::Confirmed("ok".into()).outcome(), "confirmed");
        assert_eq!(
            LogoffResult::NotStarted("stale".into()).outcome(),
            "not_started"
        );
        assert_eq!(LogoffResult::Failed("exit 1".into()).outcome(), "failed");
        assert_eq!(LogoffResult::Unknown("timeout".into()).outcome(), "unknown");
        assert_eq!(LogoffResult::Anomaly("reuse".into()).outcome(), "anomaly");
        assert_ne!(
            LogoffResult::Unknown("timeout".into()).outcome(),
            "confirmed"
        );
    }
}
