//! Resident background role: host TCP entry, SSH/memory observation, full WTS
//! session snapshots, SYSTEM logoff and routing to on-demand user workers.
use std::io;
use std::net::{IpAddr, SocketAddr};
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde::Serialize;
use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{tcp::OwnedWriteHalf, TcpListener, TcpStream};
use tokio::sync::watch;

use crate::protocol::{
    check_protocol, ErrorResponse, HostAction, Snapshot, WorkerRequest, WorkerResponse, PROTOCOL,
    ROLE_BACKGROUND, ROLE_USER,
};
use crate::{auth, memory, session, ssh, worker};

const LIST_APPS_WORKER_TIMEOUT: Duration = Duration::from_secs(60);
const USER_ACTION_WORKER_TIMEOUT: Duration = Duration::from_secs(15);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn user_action_responses_carry_protocol_and_background_role() {
        let response = WorkerResponse {
            apps: Some(vec![crate::protocol::AppInfo {
                id: "notepad".into(),
                name: "Notepad".into(),
                app_type: "win32".into(),
                target: r"C:\Windows\notepad.exe".into(),
                shortcut_path: None,
                arguments: None,
                working_dir: None,
                icon_base64: None,
            }]),
            ..WorkerResponse::ok()
        };
        let value = host_response(response).unwrap();
        assert_eq!(value["status"], "ok");
        assert_eq!(value["protocol"], PROTOCOL);
        assert_eq!(value["role"], ROLE_BACKGROUND);
        assert_eq!(value["apps"][0]["id"], "notepad");
        assert!(value.get("drives").is_none());
    }

    #[test]
    fn worker_errors_stay_errors_and_report_protocol_two() {
        let value = host_response(WorkerResponse::error("launch failed")).unwrap();
        assert_eq!(value["status"], "error");
        assert_eq!(value["protocol"], PROTOCOL);
        assert_eq!(value["role"], ROLE_BACKGROUND);
        assert_eq!(value["message"], "launch failed");
    }

    #[test]
    fn snapshot_response_keeps_the_approved_shape() {
        let snapshot = Snapshot {
            revision: 7,
            fresh: true,
            sessions: vec![],
        };
        let value = snapshot_response(snapshot);
        assert_eq!(value["status"], "ok");
        assert_eq!(value["protocol"], PROTOCOL);
        assert_eq!(value["role"], ROLE_BACKGROUND);
        assert_eq!(value["snapshot"]["revision"], 7);
        assert_eq!(value["snapshot"]["fresh"], true);
        assert_eq!(value["snapshot"]["sessions"], json!([]));
    }

    #[test]
    fn session_status_response_reports_the_user_worker_activity() {
        let reference = crate::protocol::SessionRef {
            session_id: 4,
            authentication_id: "00000000000003E7".into(),
            boot_identifier: "11111111-2222-4333-8444-555555555555".into(),
        };
        let status = crate::protocol::WorkerSessionStatus {
            reference: reference.clone(),
            session_id: 4,
            session_active: true,
            sid: "S-1-5-21-1".into(),
            user: r"MACHINE\user".into(),
            windows: Some(1),
            tray_icons: Some(2),
            note: None,
        };
        let value = user_status_response(status);
        assert_eq!(value["status"], "ok");
        assert_eq!(value["protocol"], PROTOCOL);
        assert_eq!(value["role"], ROLE_USER);
        assert_eq!(
            value["session_ref"],
            serde_json::to_value(&reference).unwrap()
        );
        assert_eq!(value["session_active"], true);
        assert_eq!(
            value["activity"]["reference"],
            serde_json::to_value(&reference).unwrap()
        );
        assert_eq!(value["activity"]["sid"], "S-1-5-21-1");
        assert_eq!(value["activity"]["session_id"], 4);
        assert_eq!(value["activity"]["user"], r"MACHINE\user");
    }

    #[test]
    fn list_apps_gets_the_longer_worker_budget_only() {
        let inspect: HostAction = serde_json::from_value(json!({
            "action": "list_apps",
            "protocol": 2,
            "session_ref": {
                "session_id": 4,
                "authentication_id": "00000000000003E7",
                "boot_identifier": "11111111-2222-4333-8444-555555555555"
            }
        }))
        .unwrap();
        assert_eq!(worker_timeout(&inspect), LIST_APPS_WORKER_TIMEOUT);
        let run: HostAction = serde_json::from_value(json!({
            "action": "run",
            "protocol": 2,
            "target": "notepad.exe",
            "session_ref": {
                "session_id": 4,
                "authentication_id": "00000000000003E7",
                "boot_identifier": "11111111-2222-4333-8444-555555555555"
            }
        }))
        .unwrap();
        assert_eq!(worker_timeout(&run), USER_ACTION_WORKER_TIMEOUT);
    }
}

#[derive(Serialize)]
struct Status {
    status: &'static str,
    protocol: u32,
    role: &'static str,
    kind: &'static str,
    ssh_active: Option<bool>,
    memory: Option<memory::Usage>,
    source: &'static str,
    snapshot: Snapshot,
}

pub fn run(addr: SocketAddr, args: &[String]) -> io::Result<()> {
    let option = |name: &str| {
        args.iter()
            .position(|value| value == name)
            .and_then(|index| args.get(index + 1))
            .cloned()
    };
    let access = match (option("--session-user"), option("--session-host")) {
        (None, None) => None,
        (Some(user), Some(host)) if !user.is_empty() => Some(Arc::new(session::Access::new(
            user,
            host.parse().map_err(io::Error::other)?,
        )?)),
        _ => {
            return Err(io::Error::other(
                "session management requires both --session-user and --session-host",
            ))
        }
    };
    let token_path = auth::token_file_arg(args)?;
    let token = auth::load_for_bind(addr, token_path.as_deref())?.map(Arc::new);
    let (updates, current) = watch::channel(None);
    let monitor = ssh::start(move |active| {
        updates.send_replace(active);
    });
    let source = monitor.source();

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;
    let result = runtime.block_on(serve(addr, current, source, access, token));
    drop(monitor);
    result
}

async fn serve(
    addr: SocketAddr,
    current: watch::Receiver<Option<bool>>,
    source: &'static str,
    access: Option<Arc<session::Access>>,
    token: Option<Arc<auth::Token>>,
) -> io::Result<()> {
    let listener = TcpListener::bind(addr).await?;
    let (memory_updates, memory) = watch::channel(memory::read());
    let mut samples = tokio::time::interval(Duration::from_secs(1));
    samples.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let (shutdown, mut stopping) = watch::channel(false);
    let hub = access.as_ref().map(|access| {
        let hub = Arc::new(session::SnapshotHub::start(
            access.sid.clone(),
            shutdown.subscribe(),
        ));
        if let Err(error) = session::start_watcher(hub.signal()) {
            log::warn!("WTS session watcher unavailable: {error}");
        }
        hub
    });
    log::info!("Background listening on {addr}; SSH source={source}");
    loop {
        tokio::select! {
            connection = listener.accept() => {
                let (stream, peer) = connection?;
                let context = ConnectionContext {
                    source,
                    access: access.clone(),
                    token: token.clone(),
                    peer: peer.ip(),
                    hub: hub.clone(),
                };
                let state = current.clone();
                let memory = memory.clone();
                let shutdown = shutdown.clone();
                tokio::spawn(async move {
                    if let Err(error) = handle(stream, state, memory, shutdown, context).await {
                        log::debug!("Background connection ended: {error}");
                    }
                });
            }
            _ = samples.tick() => {
                let previous = *memory_updates.borrow();
                let next = memory::read();
                if memory::changed(previous, next) { memory_updates.send_replace(next); }
            }
            _ = stopping.changed() => break,
        }
    }
    Ok(())
}

struct ConnectionContext {
    source: &'static str,
    access: Option<Arc<session::Access>>,
    token: Option<Arc<auth::Token>>,
    peer: IpAddr,
    hub: Option<Arc<session::SnapshotHub>>,
}

async fn handle(
    stream: TcpStream,
    mut state: watch::Receiver<Option<bool>>,
    mut memory: watch::Receiver<Option<memory::Usage>>,
    shutdown: watch::Sender<bool>,
    context: ConnectionContext,
) -> io::Result<()> {
    let ConnectionContext {
        source,
        access,
        token,
        peer,
        hub,
    } = context;
    let (read, mut write) = stream.into_split();
    let mut read = BufReader::new(read).take(crate::MAX_REQUEST_BYTES + 1);
    let mut line = String::new();
    tokio::time::timeout(Duration::from_secs(5), read.read_line(&mut line)).await??;
    if !line.ends_with('\n') || line.len() as u64 > crate::MAX_REQUEST_BYTES {
        return send_error(
            &mut write,
            ErrorResponse::protocol("incomplete or oversized request"),
        )
        .await;
    }
    let mut request: Value = match serde_json::from_str(&line) {
        Ok(request) => request,
        Err(error) => {
            return send_error(
                &mut write,
                ErrorResponse::protocol(format!("background request: {error}")),
            )
            .await
        }
    };
    if let Err(error) = check_protocol(&request) {
        log::warn!("Background rejected protocol-mismatched request");
        return send_error(&mut write, error).await;
    }
    let action_name = auth::action(&request).to_string();
    if !auth::authorize(&request, token.as_deref()) {
        log::warn!("Background rejected unauthenticated request (action: {action_name})");
        return send_error(
            &mut write,
            ErrorResponse::protocol("authentication required"),
        )
        .await;
    }
    auth::remove_token_field(&mut request);
    let action = match serde_json::from_value::<HostAction>(request) {
        Ok(action) => action,
        Err(error) => {
            return send_error(
                &mut write,
                ErrorResponse::protocol(format!("background request: {error}")),
            )
            .await
        }
    };

    match action {
        HostAction::InspectSessions { user } => {
            let Some(access) = access else {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("session management is not enabled on this agent"),
                )
                .await;
            };
            if !access.allows_optional_user(peer, user.as_deref()) {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol(
                        "session management is not authorized for this host/user",
                    ),
                )
                .await;
            }
            let Some(hub) = hub else {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("session snapshot is not available"),
                )
                .await;
            };
            let snapshot = hub.inspect(session::SNAPSHOT_WAIT_TIMEOUT).await;
            send(&mut write, &snapshot_response(snapshot)).await
        }
        HostAction::LogoffSession { user, session_ref } => {
            let Some(access) = access else {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("session management is not enabled on this agent"),
                )
                .await;
            };
            if !access.allows(peer, &user) {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol(
                        "session management is not authorized for this host/user",
                    ),
                )
                .await;
            }
            if let Err(error) = session_ref.validate() {
                return send_error(
                    &mut write,
                    ErrorResponse::with_outcome(error, "not_started"),
                )
                .await;
            }
            let sid = access.sid.clone();
            let result =
                tokio::task::spawn_blocking(move || session::logoff(&sid, &session_ref)).await;
            match result {
                Ok(result) => {
                    let outcome = result.outcome();
                    let message = result.message();
                    if outcome == "confirmed" {
                        send(
                            &mut write,
                            &json!({
                                "status": "ok",
                                "protocol": PROTOCOL,
                                "role": ROLE_BACKGROUND,
                                "message": message,
                            }),
                        )
                        .await
                    } else {
                        send_error(&mut write, ErrorResponse::with_outcome(message, outcome)).await
                    }
                }
                Err(error) => {
                    send_error(
                        &mut write,
                        ErrorResponse::with_outcome(
                            format!("session logoff task failed: {error}"),
                            "unknown",
                        ),
                    )
                    .await
                }
            }
        }
        HostAction::Ping => {
            send(
                &mut write,
                &json!({
                    "status": "ok",
                    "protocol": PROTOCOL,
                    "role": ROLE_BACKGROUND,
                    "message": "pong",
                }),
            )
            .await
        }
        HostAction::Status => {
            let active = *state.borrow();
            let memory = *memory.borrow();
            let snapshot = hub.as_ref().map(|hub| hub.current()).unwrap_or_default();
            send_status(&mut write, active, memory, snapshot, "snapshot", source).await
        }
        HostAction::Quit => {
            send(
                &mut write,
                &json!({
                    "status": "ok",
                    "protocol": PROTOCOL,
                    "role": ROLE_BACKGROUND,
                    "message": "quitting",
                }),
            )
            .await?;
            shutdown.send_replace(true);
            Ok(())
        }
        HostAction::WatchStatus => {
            let initial = *state.borrow_and_update();
            let initial_memory = *memory.borrow_and_update();
            let initial_snapshot = hub.as_ref().map(|hub| hub.current()).unwrap_or_default();
            send_status(
                &mut write,
                initial,
                initial_memory,
                initial_snapshot,
                "snapshot",
                source,
            )
            .await?;
            let mut stopping = shutdown.subscribe();
            let mut snapshots = hub.as_ref().map(|hub| hub.subscribe());
            loop {
                tokio::select! {
                    changed = state.changed() => {
                        if changed.is_err() { return Ok(()); }
                    }
                    changed = memory.changed() => {
                        if changed.is_err() { return Ok(()); }
                    }
                    changed = async {
                        match snapshots.as_mut() {
                            Some(snapshots) => snapshots.changed().await.map_err(|_| ()),
                            None => std::future::pending().await,
                        }
                    } => {
                        if changed.is_err() { return Ok(()); }
                    }
                    _ = read.read_u8() => return Ok(()), // EOF or unexpected extra request.
                    _ = stopping.changed() => return Ok(()),
                }
                let active = *state.borrow_and_update();
                let memory = *memory.borrow_and_update();
                if let Some(snapshots) = snapshots.as_mut() {
                    snapshots.borrow_and_update();
                }
                let snapshot = hub.as_ref().map(|hub| hub.current()).unwrap_or_default();
                send_status(&mut write, active, memory, snapshot, "changed", source).await?;
            }
        }
        HostAction::SessionStatus { session_ref } => {
            let Some(access) = access else {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("session management is not enabled on this agent"),
                )
                .await;
            };
            if !access.allows_host(peer) {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("session management is not authorized for this host"),
                )
                .await;
            }
            if let Err(error) = session_ref.validate() {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol(format!("invalid session_ref: {error}")),
                )
                .await;
            }
            let sid = access.sid.clone();
            let deadline = Instant::now() + USER_ACTION_WORKER_TIMEOUT;
            let result = tokio::task::spawn_blocking(move || {
                worker::execute(&session_ref, &sid, WorkerRequest::SessionStatus, deadline)
            })
            .await;
            match result {
                Ok(Ok(response)) if response.status == "ok" => {
                    let status = match response.session {
                        Some(status) => status,
                        None => {
                            return send_error(
                                &mut write,
                                ErrorResponse::protocol("user worker returned no session status"),
                            )
                            .await
                        }
                    };
                    send(&mut write, &user_status_response(status)).await
                }
                Ok(Ok(response)) => {
                    send_error(
                        &mut write,
                        ErrorResponse::protocol(
                            response
                                .message
                                .unwrap_or_else(|| "user worker status failed".into()),
                        ),
                    )
                    .await
                }
                Ok(Err(error)) => {
                    send_error(
                        &mut write,
                        ErrorResponse::protocol(format!("user session status failed: {error}")),
                    )
                    .await
                }
                Err(error) => {
                    send_error(
                        &mut write,
                        ErrorResponse::protocol(format!(
                            "user session status task failed: {error}"
                        )),
                    )
                    .await
                }
            }
        }
        action @ (HostAction::Run { .. }
        | HostAction::Open { .. }
        | HostAction::ListApps { .. }
        | HostAction::GetMappedDrives { .. }) => {
            let Some(access) = access else {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("session management is not enabled on this agent"),
                )
                .await;
            };
            if !access.allows_host(peer) {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("session management is not authorized for this host"),
                )
                .await;
            }
            let Some(reference) = action.session_ref().cloned() else {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("request is missing session_ref"),
                )
                .await;
            };
            if let Err(error) = reference.validate() {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol(format!("invalid session_ref: {error}")),
                )
                .await;
            }
            let timeout = worker_timeout(&action);
            let Some(worker_request) = Option::<WorkerRequest>::from(action) else {
                return send_error(
                    &mut write,
                    ErrorResponse::protocol("unsupported user action"),
                )
                .await;
            };
            let sid = access.sid.clone();
            let deadline = Instant::now() + timeout;
            let result = tokio::task::spawn_blocking(move || {
                worker::execute(&reference, &sid, worker_request, deadline)
            })
            .await;
            match result {
                Ok(Ok(response)) => match host_response(response) {
                    Ok(value) => send(&mut write, &value).await,
                    Err(error) => {
                        send_error(&mut write, ErrorResponse::protocol(error.to_string())).await
                    }
                },
                Ok(Err(error)) => {
                    send_error(
                        &mut write,
                        ErrorResponse::protocol(format!("user session action failed: {error}")),
                    )
                    .await
                }
                Err(error) => {
                    send_error(
                        &mut write,
                        ErrorResponse::protocol(format!("user session task failed: {error}")),
                    )
                    .await
                }
            }
        }
    }
}

fn snapshot_response(snapshot: Snapshot) -> Value {
    json!({
        "status": "ok",
        "protocol": PROTOCOL,
        "role": ROLE_BACKGROUND,
        "snapshot": snapshot,
    })
}

fn user_status_response(status: crate::protocol::WorkerSessionStatus) -> Value {
    let reference = &status.reference;
    json!({
        "status": "ok",
        "protocol": PROTOCOL,
        "role": ROLE_USER,
        "session_ref": reference,
        "session_active": status.session_active,
        "activity": {
            "reference": reference,
            "sid": status.sid,
            "session_id": status.session_id,
            "user": status.user,
            "windows": status.windows,
            "tray_icons": status.tray_icons,
            "note": status.note,
        },
    })
}

fn host_response(response: WorkerResponse) -> serde_json::Result<Value> {
    let mut value = serde_json::to_value(response)?;
    if let Value::Object(fields) = &mut value {
        fields.insert("protocol".into(), json!(PROTOCOL));
        fields.insert("role".into(), json!(ROLE_BACKGROUND));
    }
    Ok(value)
}

fn worker_timeout(action: &HostAction) -> Duration {
    match action {
        HostAction::ListApps { .. } => LIST_APPS_WORKER_TIMEOUT,
        _ => USER_ACTION_WORKER_TIMEOUT,
    }
}

async fn send_status(
    write: &mut OwnedWriteHalf,
    active: Option<bool>,
    memory: Option<memory::Usage>,
    snapshot: Snapshot,
    kind: &'static str,
    source: &'static str,
) -> io::Result<()> {
    send(
        write,
        &Status {
            status: "ok",
            protocol: PROTOCOL,
            role: ROLE_BACKGROUND,
            kind,
            ssh_active: active,
            memory,
            source,
            snapshot,
        },
    )
    .await
}

async fn send_error(write: &mut OwnedWriteHalf, error: ErrorResponse) -> io::Result<()> {
    send(write, &error).await
}

async fn send(write: &mut OwnedWriteHalf, value: &impl Serialize) -> io::Result<()> {
    let mut bytes = serde_json::to_vec(value)?;
    bytes.push(b'\n');
    tokio::time::timeout(Duration::from_secs(5), write.write_all(&bytes)).await?
}
