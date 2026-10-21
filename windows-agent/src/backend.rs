//! Resident role: no window, RDP keepalive, user-app launch or mapped-drive access.
use std::io;
use std::net::{IpAddr, SocketAddr};
use std::sync::Arc;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{tcp::OwnedWriteHalf, TcpListener, TcpStream};
use tokio::sync::watch;

use crate::{auth, memory, ssh};

#[derive(Deserialize)]
#[serde(tag = "action", rename_all = "snake_case")]
enum Request {
    Ping,
    Status,
    WatchStatus,
    Quit,
    InspectSession {
        user: String,
    },
    LogoffSession {
        user: String,
        session_id: u32,
        fingerprint: String,
        #[serde(default = "require_empty_default")]
        require_empty: bool,
    },
}
fn require_empty_default() -> bool {
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn session_control_requires_both_host_and_user() {
        let access = SessionAccess {
            user: "user".into(),
            host: "192.0.2.1".parse().unwrap(),
        };
        assert!(access.allows("192.0.2.1".parse().unwrap(), "USER"));
        assert!(!access.allows("127.0.0.1".parse().unwrap(), "user"));
        assert!(!access.allows("192.0.2.1".parse().unwrap(), "other"));
    }
    #[test]
    fn typed_background_request_parses_after_envelope_removal() {
        for (wire, expected) in [
            (
                r#"{"action":"watch_status","token":"secret"}"#,
                "watch_status",
            ),
            (r#"{"action":"quit","token":"secret"}"#, "quit"),
        ] {
            let mut value: serde_json::Value = serde_json::from_str(wire).unwrap();
            auth::remove_token_field(&mut value);
            let request = serde_json::from_value::<Request>(value).unwrap();
            match (request, expected) {
                (Request::WatchStatus, "watch_status") => {}
                (Request::Quit, "quit") => {}
                _ => panic!("unexpected request for {expected}"),
            }
        }
    }

    #[test]
    fn omitted_logoff_policy_requires_empty() {
        let request: Request = serde_json::from_str(
            r#"{"action":"logoff_session","user":"user","session_id":2,"fingerprint":"logon"}"#,
        )
        .unwrap();
        assert!(matches!(
            request,
            Request::LogoffSession {
                require_empty: true,
                ..
            }
        ));
    }
}

struct SessionAccess {
    user: String,
    host: IpAddr,
}
impl SessionAccess {
    fn allows(&self, peer: IpAddr, user: &str) -> bool {
        self.host == peer && self.user.eq_ignore_ascii_case(user)
    }
}

#[derive(Serialize)]
struct Status {
    status: &'static str,
    role: &'static str,
    kind: &'static str,
    ssh_active: Option<bool>,
    memory: Option<memory::Usage>,
    source: &'static str,
}

pub fn run(addr: SocketAddr, args: &[String]) -> io::Result<()> {
    let option = |name: &str| {
        args.iter()
            .position(|v| v == name)
            .and_then(|i| args.get(i + 1))
            .cloned()
    };
    let access = match (option("--session-user"), option("--session-host")) {
        (None, None) => None,
        (Some(user), Some(host)) if !user.is_empty() => Some(Arc::new(SessionAccess {
            user,
            host: host.parse().map_err(io::Error::other)?,
        })),
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
    drop(monitor); // Stops ETW and the check thread before exiting the process.
    result
}

async fn serve(
    addr: SocketAddr,
    current: watch::Receiver<Option<bool>>,
    source: &'static str,
    access: Option<Arc<SessionAccess>>,
    token: Option<Arc<auth::Token>>,
) -> io::Result<()> {
    let listener = TcpListener::bind(addr).await?;
    let (memory_updates, memory) = watch::channel(memory::read());
    let mut samples = tokio::time::interval(Duration::from_secs(1));
    samples.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let (shutdown, mut stopping) = watch::channel(false);
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
    access: Option<Arc<SessionAccess>>,
    token: Option<Arc<auth::Token>>,
    peer: IpAddr,
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
    } = context;
    let (read, mut write) = stream.into_split();
    let mut read = BufReader::new(read).take(crate::MAX_REQUEST_BYTES + 1);
    let mut line = String::new();
    tokio::time::timeout(Duration::from_secs(5), read.read_line(&mut line)).await??;
    if !line.ends_with('\n') || line.len() as u64 > crate::MAX_REQUEST_BYTES {
        return send(
            &mut write,
            &serde_json::json!({"status":"error","message":"incomplete or oversized request"}),
        )
        .await;
    }
    let mut request: serde_json::Value = match serde_json::from_str(&line) {
        Ok(request) => request,
        Err(error) => return send(
            &mut write,
            &serde_json::json!({"status":"error","message":format!("background request: {error}")}),
        )
        .await,
    };
    let action = auth::action(&request).to_string();
    if !auth::authorize(&request, token.as_deref()) {
        log::warn!("Background rejected unauthenticated request (action: {action})");
        return send(
            &mut write,
            &serde_json::json!({"status":"error","message":"authentication required"}),
        )
        .await;
    }
    auth::remove_token_field(&mut request);
    let request = match serde_json::from_value::<Request>(request) {
        Ok(request) => request,
        Err(error) => return send(
            &mut write,
            &serde_json::json!({"status":"error","message":format!("background request: {error}")}),
        )
        .await,
    };
    match request {
        Request::InspectSession { user } => {
            if !access
                .as_ref()
                .map(|a| a.allows(peer, &user))
                .unwrap_or(false)
            {
                return send(&mut write, &serde_json::json!({"status":"error","message":"session management is not authorized for this host/user"})).await;
            }
            let result = tokio::task::spawn_blocking(move || crate::session::inspect(&user)).await;
            let value = match result {
                Ok(Ok(activity)) => {
                    serde_json::json!({"status":"ok","role":"background","activity":activity})
                }
                other => {
                    serde_json::json!({"status":"error","message":format!("session inspection failed: {other:?}")})
                }
            };
            send(&mut write, &value).await
        }
        Request::LogoffSession {
            user,
            session_id,
            fingerprint,
            require_empty,
        } => {
            if !access
                .as_ref()
                .map(|a| a.allows(peer, &user))
                .unwrap_or(false)
            {
                return send(&mut write, &serde_json::json!({"status":"error","message":"session management is not authorized for this host/user"})).await;
            }
            let result = tokio::task::spawn_blocking(move || {
                crate::session::logoff(&user, session_id, &fingerprint, require_empty)
            })
            .await;
            let value = match result {
                Ok(Ok(())) => serde_json::json!({"status":"ok","role":"background"}),
                other => {
                    serde_json::json!({"status":"error","message":format!("session logoff failed: {other:?}")})
                }
            };
            send(&mut write, &value).await
        }
        Request::Ping => {
            send(
                &mut write,
                &serde_json::json!({"status":"ok","role":"background","message":"pong"}),
            )
            .await
        }
        Request::Status => {
            let active = *state.borrow();
            let memory = *memory.borrow();
            send_status(&mut write, active, memory, "snapshot", source).await
        }
        Request::Quit => {
            send(
                &mut write,
                &serde_json::json!({"status":"ok","role":"background","message":"quitting"}),
            )
            .await?;
            shutdown.send_replace(true);
            Ok(())
        }
        Request::WatchStatus => {
            let initial = *state.borrow_and_update();
            let initial_memory = *memory.borrow_and_update();
            send_status(&mut write, initial, initial_memory, "snapshot", source).await?;
            let mut stopping = shutdown.subscribe();
            loop {
                tokio::select! {
                    changed = state.changed() => {
                        if changed.is_err() { return Ok(()); }
                    }
                    changed = memory.changed() => {
                        if changed.is_err() { return Ok(()); }
                    }
                    _ = read.read_u8() => return Ok(()), // EOF or unexpected extra request.
                    _ = stopping.changed() => return Ok(()),
                }
                // Both watchers only publish meaningful changes, so each frame
                // carries the current value of both observations.
                let active = *state.borrow_and_update();
                let memory = *memory.borrow_and_update();
                send_status(&mut write, active, memory, "changed", source).await?;
            }
        }
    }
}

async fn send_status(
    write: &mut OwnedWriteHalf,
    active: Option<bool>,
    memory: Option<memory::Usage>,
    kind: &'static str,
    source: &'static str,
) -> io::Result<()> {
    send(
        write,
        &Status {
            status: "ok",
            role: "background",
            kind,
            ssh_active: active,
            memory,
            source,
        },
    )
    .await
}
async fn send(write: &mut OwnedWriteHalf, value: &impl Serialize) -> io::Result<()> {
    let mut bytes = serde_json::to_vec(value)?;
    bytes.push(b'\n');
    tokio::time::timeout(Duration::from_secs(5), write.write_all(&bytes)).await?
}
