//! SSH activity observation for the Windows backend.
//!
//! The controller only needs the boundary between "no established TCP
//! connection to the guest SSH port" and "at least one such connection".
//! This module reports that boundary as `Option<bool>`:
//!
//! - `Some(false)`: both IPv4 and IPv6 TCP tables were read and neither has an
//!   established connection whose local port is 22.
//! - `Some(true)`: both tables were read and at least one such connection
//!   exists.
//! - `None`: the observation is incomplete. `None` is never converted into
//!   zero.
//!
//! # API
//!
//! [`start`] starts a monitor and synchronously invokes the callback once with
//! the initial observation. Afterwards the callback is invoked only when the
//! observed state changes, including transitions to/from `None` when a table
//! check becomes unavailable or available again. Count changes inside the
//! non-zero state (for example 1 -> 2) do not invoke the callback.
//!
//! ```ignore
//! let mut monitor = ssh::start(|state| println!("ssh: {:?}", state));
//! let initial = monitor.current();
//! let source = monitor.source(); // "etw" or "poll"
//! monitor.stop(); // or drop(monitor)
//! ```
//!
//! The monitor owns its ETW session and worker thread. [`Monitor::stop`] and
//! `Drop` stop the ETW session before returning. No Tokio runtime is required;
//! the backend may forward the callback into a Tokio watch channel.
//!
//! # Observation sources
//!
//! The authoritative check is `GetExtendedTcpTable` for IPv4 and IPv6, as in
//! the original Agent. Both tables must be read completely; a partial read
//! yields `None`.
//!
//! When possible, real-time ETW events from `Microsoft-Windows-Kernel-Network`
//! are used only as wake-ups:
//!
//! - provider GUID `7dd42a49-5329-4832-8dfd-43d979153a88`
//! - IPv4 keyword `0x10`, IPv6 keyword `0x20`
//! - accepted connection: event 15 (IPv4) or 31 (IPv6)
//! - disconnect: event 13 (IPv4) or 29 (IPv6)
//!
//! The event is accepted when either endpoint port is 22, because the exact
//! orientation of `dport`/`sport` on the accept event must still be confirmed
//! on the target Windows build. The event never sets the published state: it
//! only requests the TCP table check, which prevents a false state. Ports in
//! these payloads are in network byte order.
//!
//! If ETW cannot be started, the monitor falls back to the same check every
//! [`POLL_INTERVAL`] and [`Monitor::source`] returns `"poll"`. The periodic
//! check also runs while ETW is active, so missed or dropped events cannot make
//! the observation permanently stale.
//!
//! # Privileges and validation limits
//!
//! Real-time ETW sessions require elevation, membership in the Performance Log
//! Users group, or a LocalSystem/LocalService/NetworkService account. Without
//! one of those, `start` logs a warning and uses polling. The trace session is
//! named `rdp-launcher-ssh-{pid}` so independent backend instances do not stop
//! each other's sessions. The monitor only stops the session it created.
//! Because the name is process-specific, a session left behind by a crashed
//! process is not reclaimed by a later process; normal `stop`/`Drop` releases
//! it.

use std::sync::atomic::{AtomicBool, AtomicU8, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, SyncSender};
use std::sync::{Arc, Mutex};
use std::thread::{self, JoinHandle};
use std::time::Duration;

use log::{debug, info, warn};

#[cfg(windows)]
use ferrisetw::trace::UserTrace;

/// Local port used by the guest's SSH server.
pub const SSH_PORT: u16 = 22;

/// Fallback and low-frequency consistency interval.
pub const POLL_INTERVAL: Duration = Duration::from_secs(60);

/// Value returned by [`Monitor::source`] while ETW events are active.
pub const SOURCE_ETW: &str = "etw";

/// Value returned by [`Monitor::source`] when only periodic checks are active.
pub const SOURCE_POLL: &str = "poll";

#[cfg(windows)]
const KERNEL_NETWORK_PROVIDER: &str = "7dd42a49-5329-4832-8dfd-43d979153a88";
#[cfg(windows)]
const KEYWORD_IPV4: u64 = 0x10;
#[cfg(windows)]
const KEYWORD_IPV6: u64 = 0x20;

const EVENT_DISCONNECT_V4: u16 = 13;
const EVENT_ACCEPT_V4: u16 = 15;
const EVENT_DISCONNECT_V6: u16 = 29;
const EVENT_ACCEPT_V6: u16 = 31;

const REQUEST_ACCEPT: u8 = 1 << 0;
const REQUEST_DISCONNECT: u8 = 1 << 1;

/// Check both address families and return `None` if either table is
/// incomplete.
pub fn count() -> Option<usize> {
    #[cfg(windows)]
    {
        unsafe { windows_impl::count_active_ssh_connections() }
    }
    #[cfg(not(windows))]
    {
        None
    }
}

/// Return the zero/non-zero SSH state, or `None` when the check is incomplete.
pub fn check() -> Option<bool> {
    state_from_count(count())
}

fn state_from_count(count: Option<usize>) -> Option<bool> {
    count.map(|count| count > 0)
}

/// Start observing SSH activity.
///
/// `on_change` is invoked once synchronously with the initial observation
/// before this function returns. See the module documentation for the callback
/// contract.
pub fn start(on_change: impl Fn(Option<bool>) + Send + Sync + 'static) -> Monitor {
    let shared = Arc::new(Shared::new(on_change));
    let stopped = Arc::new(AtomicBool::new(false));
    let pending = Arc::new(AtomicU8::new(0));
    let (wake_tx, wake_rx) = mpsc::sync_channel(1);

    #[cfg(windows)]
    let trace = windows_impl::start_etw(wake_tx.clone(), Arc::clone(&pending));
    #[cfg(not(windows))]
    let trace = None;

    let source = if trace.is_some() {
        SOURCE_ETW
    } else {
        SOURCE_POLL
    };
    if trace.is_none() {
        warn!("SSH ETW unavailable; using {POLL_INTERVAL:?} polling");
    }

    // Establish the initial state before the worker can publish anything, and
    // invoke the initial callback on the caller's thread.
    let initial = check();
    shared.publish_initial(initial);
    info!("SSH observation started (source={source}, initial={initial:?})");

    let worker = {
        let shared = Arc::clone(&shared);
        let stopped = Arc::clone(&stopped);
        let pending = Arc::clone(&pending);
        thread::spawn(move || worker_loop(wake_rx, pending, shared, stopped))
    };

    Monitor {
        shared,
        stopped,
        wake_tx,
        trace,
        worker: Some(worker),
        source,
    }
}

/// Handle returned by [`start`]. Dropping it releases the ETW session and joins
/// the observation worker.
pub struct Monitor {
    shared: Arc<Shared>,
    stopped: Arc<AtomicBool>,
    wake_tx: SyncSender<()>,
    trace: Option<UserTrace>,
    worker: Option<JoinHandle<()>>,
    source: &'static str,
}

impl Monitor {
    /// Latest published state. This remains readable after [`Monitor::stop`].
    pub fn current(&self) -> Option<bool> {
        self.shared.current()
    }

    /// `"etw"` when the real-time provider started, otherwise `"poll"`.
    pub fn source(&self) -> &'static str {
        self.source
    }

    /// Stop the ETW session and worker. Idempotent.
    pub fn stop(&mut self) {
        if self.stopped.swap(true, Ordering::AcqRel) {
            return;
        }
        self.shared.stop();

        if let Some(trace) = self.trace.take() {
            if let Err(error) = trace.stop() {
                warn!("failed to stop SSH ETW session: {error:?}");
            }
        }

        // Wake a worker waiting for an event. If the channel is full the worker
        // already has a wake-up pending and will observe `stopped` next.
        let _ = self.wake_tx.try_send(());

        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

impl Drop for Monitor {
    fn drop(&mut self) {
        self.stop();
    }
}

struct Shared {
    current: Mutex<Option<bool>>,
    on_change: Box<dyn Fn(Option<bool>) + Send + Sync>,
    stopped: AtomicBool,
}

impl Shared {
    fn new(on_change: impl Fn(Option<bool>) + Send + Sync + 'static) -> Self {
        Self {
            current: Mutex::new(None),
            on_change: Box::new(on_change),
            stopped: AtomicBool::new(false),
        }
    }

    fn current(&self) -> Option<bool> {
        *self.current.lock().expect("SSH state lock")
    }

    /// Publish the first observation unconditionally so the backend receives
    /// the initialization callback even when the state is `None`.
    fn publish_initial(&self, state: Option<bool>) {
        *self.current.lock().expect("SSH state lock") = state;
        (self.on_change)(state);
    }

    /// Publish a state change. Repeated identical observations are suppressed.
    fn publish(&self, state: Option<bool>) {
        let changed = {
            let mut current = self.current.lock().expect("SSH state lock");
            let changed = *current != state;
            *current = state;
            changed
        };

        if changed && !self.stopped.load(Ordering::Acquire) {
            debug!("SSH observation changed to {state:?}");
            (self.on_change)(state);
        }
    }

    fn stop(&self) {
        self.stopped.store(true, Ordering::Release);
    }
}

#[cfg(windows)]
mod windows_impl {
    use std::ptr;
    use std::sync::atomic::{AtomicU8, Ordering};
    use std::sync::mpsc::{SyncSender, TrySendError};
    use std::sync::Arc;

    use ferrisetw::parser::Parser;
    use ferrisetw::provider::Provider;
    use ferrisetw::schema_locator::SchemaLocator;
    use ferrisetw::trace::UserTrace;
    use ferrisetw::EventRecord;
    use windows_sys::Win32::Foundation::NO_ERROR;
    use windows_sys::Win32::NetworkManagement::IpHelper::{
        GetExtendedTcpTable, MIB_TCP6TABLE_OWNER_PID, MIB_TCPTABLE_OWNER_PID, MIB_TCP_STATE_ESTAB,
        TCP_TABLE_OWNER_PID_ALL,
    };
    use windows_sys::Win32::Networking::WinSock::{AF_INET, AF_INET6};

    use super::{
        boundary_event, event_hits_ssh_port, host_port, KERNEL_NETWORK_PROVIDER, KEYWORD_IPV4,
        KEYWORD_IPV6, SSH_PORT,
    };

    pub(super) unsafe fn count_active_ssh_connections() -> Option<usize> {
        match (count_ssh_v4_connections(), count_ssh_v6_connections()) {
            (Some(v4), Some(v6)) => Some(v4 + v6),
            _ => None,
        }
    }

    unsafe fn count_ssh_v4_connections() -> Option<usize> {
        let mut size = 0u32;
        let _ = GetExtendedTcpTable(
            ptr::null_mut(),
            &mut size,
            0,
            AF_INET as u32,
            TCP_TABLE_OWNER_PID_ALL,
            0,
        );
        if size == 0 {
            return None;
        }

        let mut buffer = vec![0u32; (size as usize).div_ceil(4)];
        if GetExtendedTcpTable(
            buffer.as_mut_ptr() as *mut _,
            &mut size,
            0,
            AF_INET as u32,
            TCP_TABLE_OWNER_PID_ALL,
            0,
        ) != NO_ERROR
        {
            return None;
        }

        let table = &*(buffer.as_ptr() as *const MIB_TCPTABLE_OWNER_PID);
        let rows = table.table.as_ptr();
        let mut count = 0usize;
        for index in 0..table.dwNumEntries as usize {
            let row = &*rows.add(index);
            let local_port = u16::from_be((row.dwLocalPort & 0xFFFF) as u16);
            if local_port == SSH_PORT && row.dwState == MIB_TCP_STATE_ESTAB as u32 {
                count += 1;
            }
        }
        Some(count)
    }

    unsafe fn count_ssh_v6_connections() -> Option<usize> {
        let mut size = 0u32;
        let _ = GetExtendedTcpTable(
            ptr::null_mut(),
            &mut size,
            0,
            AF_INET6 as u32,
            TCP_TABLE_OWNER_PID_ALL,
            0,
        );
        if size == 0 {
            return None;
        }

        let mut buffer = vec![0u32; (size as usize).div_ceil(4)];
        if GetExtendedTcpTable(
            buffer.as_mut_ptr() as *mut _,
            &mut size,
            0,
            AF_INET6 as u32,
            TCP_TABLE_OWNER_PID_ALL,
            0,
        ) != NO_ERROR
        {
            return None;
        }

        let table = &*(buffer.as_ptr() as *const MIB_TCP6TABLE_OWNER_PID);
        let rows = table.table.as_ptr();
        let mut count = 0usize;
        for index in 0..table.dwNumEntries as usize {
            let row = &*rows.add(index);
            let local_port = u16::from_be((row.dwLocalPort & 0xFFFF) as u16);
            if local_port == SSH_PORT && row.dwState == MIB_TCP_STATE_ESTAB as u32 {
                count += 1;
            }
        }
        Some(count)
    }

    pub(super) fn start_etw(wake: SyncSender<()>, pending: Arc<AtomicU8>) -> Option<UserTrace> {
        let provider = Provider::by_guid(KERNEL_NETWORK_PROVIDER)
            // Kernel network event manifests attach exactly one address-family
            // keyword to each event, so MatchAny is required for IPv4 + IPv6.
            .any(KEYWORD_IPV4 | KEYWORD_IPV6)
            .add_callback(move |record: &EventRecord, schemas: &SchemaLocator| {
                if let Some(event) = classify_record(record, schemas) {
                    pending.fetch_or(event, Ordering::Release);
                    match wake.try_send(()) {
                        Ok(()) | Err(TrySendError::Full(())) => {}
                        Err(TrySendError::Disconnected(())) => {}
                    }
                }
            })
            .build();

        let session_name = format!("rdp-launcher-ssh-{}", std::process::id());
        match UserTrace::new()
            .named(session_name)
            .enable(provider)
            .start_and_process()
        {
            Ok(trace) => Some(trace),
            Err(error) => {
                log::warn!(
                    "could not start SSH ETW session (requires elevation or Performance Log Users): {error:?}"
                );
                None
            }
        }
    }

    fn classify_record(record: &EventRecord, schemas: &SchemaLocator) -> Option<u8> {
        let event = boundary_event(record.event_id())?;
        let schema = match schemas.event_schema(record) {
            Ok(schema) => schema,
            Err(error) => {
                log::debug!(
                    "SSH ETW event {} has no schema: {error:?}",
                    record.event_id()
                );
                return None;
            }
        };

        let parser = Parser::create(record, &schema);
        let dport: u16 = parser.try_parse("dport").ok()?;
        let sport: u16 = parser.try_parse("sport").ok()?;
        let dport = host_port(dport);
        let sport = host_port(sport);
        if !event_hits_ssh_port(dport, sport) {
            return None;
        }

        log::debug!("SSH ETW {event:?}: dport={dport} sport={sport}");
        Some(event)
    }
}

fn boundary_event(event_id: u16) -> Option<u8> {
    match event_id {
        EVENT_ACCEPT_V4 | EVENT_ACCEPT_V6 => Some(REQUEST_ACCEPT),
        EVENT_DISCONNECT_V4 | EVENT_DISCONNECT_V6 => Some(REQUEST_DISCONNECT),
        _ => None,
    }
}

fn host_port(raw: u16) -> u16 {
    u16::from_be(raw)
}

fn event_hits_ssh_port(dport: u16, sport: u16) -> bool {
    dport == SSH_PORT || sport == SSH_PORT
}

/// Decide whether a queued event needs a fresh table check.
///
/// An accepted connection is only meaningful while the state is not already
/// non-zero; a disconnect is only meaningful while it is not already zero.
/// `None` (unknown) is checked for either kind.
fn should_check(pending: u8, current: Option<bool>) -> bool {
    match current {
        Some(true) => pending & REQUEST_DISCONNECT != 0,
        Some(false) => pending & REQUEST_ACCEPT != 0,
        None => pending != 0,
    }
}

fn worker_loop(
    wake_rx: Receiver<()>,
    pending: Arc<AtomicU8>,
    shared: Arc<Shared>,
    stopped: Arc<AtomicBool>,
) {
    loop {
        let woke = match wake_rx.recv_timeout(POLL_INTERVAL) {
            Ok(()) => Some(()),
            Err(RecvTimeoutError::Timeout) => None,
            Err(RecvTimeoutError::Disconnected) => break,
        };

        if stopped.load(Ordering::Acquire) {
            break;
        }

        match woke {
            Some(()) => {
                let bits = pending.swap(0, Ordering::AcqRel);
                if should_check(bits, shared.current()) {
                    check_and_publish(&shared);
                }
            }
            // Periodic consistency check; ETW may have missed an event.
            None => check_and_publish(&shared),
        }
    }

    shared.stop();
}

fn check_and_publish(shared: &Shared) {
    shared.publish(check());
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn repeated_observations_do_not_publish() {
        use std::sync::atomic::{AtomicUsize, Ordering};

        let calls = Arc::new(AtomicUsize::new(0));
        let seen = Arc::new(Mutex::new(Vec::new()));
        let calls_for_callback = Arc::clone(&calls);
        let seen_for_callback = Arc::clone(&seen);
        let shared = Shared::new(move |state| {
            calls_for_callback.fetch_add(1, Ordering::SeqCst);
            seen_for_callback.lock().unwrap().push(state);
        });

        shared.publish_initial(None);
        shared.publish(Some(false));
        shared.publish(Some(false));
        shared.publish(Some(true));
        shared.publish(Some(true));
        shared.publish(Some(false));
        shared.publish(None);
        shared.publish(None);

        assert_eq!(calls.load(Ordering::SeqCst), 5);
        assert_eq!(
            *seen.lock().unwrap(),
            vec![None, Some(false), Some(true), Some(false), None]
        );
    }

    #[test]
    fn stopped_shared_suppresses_late_publications() {
        use std::sync::atomic::{AtomicUsize, Ordering};

        let calls = Arc::new(AtomicUsize::new(0));
        let calls_for_callback = Arc::clone(&calls);
        let shared = Shared::new(move |_| {
            calls_for_callback.fetch_add(1, Ordering::SeqCst);
        });

        shared.publish_initial(Some(false));
        shared.stop();
        shared.publish(Some(true));

        assert_eq!(calls.load(Ordering::SeqCst), 1);
        assert_eq!(shared.current(), Some(true));
    }
}
