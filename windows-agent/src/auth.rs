//! Shared-token authentication for the host-facing TCP backend endpoint.
//!
//! The token itself never enters argv or logs: the process receives a file path
//! once at startup, loads the token, and compares each request's `token` field
//! in constant time. This is authentication for a controlled plaintext network,
//! not transport encryption. User workers use a local named pipe and do not
//! receive the shared token.

use std::io;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};

use serde_json::Value;
use subtle::ConstantTimeEq;

pub struct Token {
    secret: String,
}

impl Token {
    /// Read one token line. A single final LF or CRLF is accepted so common
    /// provisioning tools work; other whitespace and embedded line breaks are
    /// rejected rather than silently changing the secret.
    pub fn load(path: &Path) -> io::Result<Self> {
        let mut secret = std::fs::read(path)?;
        if secret.last() == Some(&b'\n') {
            secret.pop();
            if secret.last() == Some(&b'\r') {
                secret.pop();
            }
        }
        if secret.is_empty() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("agent token file {} is empty", path.display()),
            ));
        }
        if secret.iter().any(|byte| *byte == b'\r' || *byte == b'\n') {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!(
                    "agent token file {} must contain one token without CR/LF",
                    path.display()
                ),
            ));
        }
        let secret = String::from_utf8(secret).map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                format!("agent token file {} is not UTF-8", path.display()),
            )
        })?;
        Ok(Self { secret })
    }

    pub fn matches(&self, provided: Option<&str>) -> bool {
        let Some(provided) = provided else {
            return false;
        };
        let provided = provided.as_bytes();
        // Length is not secret; compare the equal-length bytes without an
        // early exit so mismatches do not reveal the matching prefix.
        provided.len() == self.secret.len() && bool::from(self.secret.as_bytes().ct_eq(provided))
    }

    /// Expose the loaded value only so this executable's local CLI can attach
    /// it to its own loopback request; handlers log and compare without it.
    pub fn value(&self) -> &str {
        &self.secret
    }
}

/// Parse `--auth-token-file PATH` from the process arguments.
pub fn token_file_arg(args: &[String]) -> io::Result<Option<PathBuf>> {
    let Some(index) = args.iter().position(|arg| arg == "--auth-token-file") else {
        return Ok(None);
    };
    let path = args
        .get(index + 1)
        .filter(|value| !value.is_empty())
        .ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                "--auth-token-file requires a path",
            )
        })?;
    Ok(Some(PathBuf::from(path)))
}

/// Load the token once before binding. Without a token only an explicit
/// loopback bind is allowed; a non-loopback listener never starts unauthenticated.
pub fn load_for_bind(bind: SocketAddr, path: Option<&Path>) -> io::Result<Option<Token>> {
    if let Some(path) = path {
        return Token::load(path).map(Some);
    }
    if bind.ip().is_loopback() {
        return Ok(None);
    }
    Err(io::Error::new(
        io::ErrorKind::InvalidInput,
        format!(
            "refusing to listen on non-loopback {bind} without --auth-token-file; \
             loopback without a token is local-only mode"
        ),
    ))
}

/// Check an already parsed request. A configured token is required for every
/// action; without one the listener is in the documented loopback-only mode.
pub fn authorize(request: &Value, token: Option<&Token>) -> bool {
    match token {
        None => true,
        Some(token) => token.matches(request.get("token").and_then(Value::as_str)),
    }
}

/// The only request field used in logs. Never return the raw request or token.
pub fn action(request: &Value) -> &str {
    request
        .get("action")
        .and_then(Value::as_str)
        .unwrap_or("unknown")
}

/// Drop the envelope field before deserializing the per-action request.
pub fn remove_token_field(request: &mut Value) {
    if let Value::Object(fields) = request {
        fields.remove("token");
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    static FILES: AtomicUsize = AtomicUsize::new(0);

    fn token_file(bytes: &[u8]) -> (PathBuf, Token) {
        let path = std::env::temp_dir().join(format!(
            "rdp-launcher-auth-{}-{}.token",
            std::process::id(),
            FILES.fetch_add(1, Ordering::SeqCst)
        ));
        std::fs::write(&path, bytes).unwrap();
        let token = Token::load(&path).unwrap();
        (path, token)
    }

    fn bind(address: &str) -> SocketAddr {
        address.parse().unwrap()
    }

    #[test]
    fn accepts_exact_token_and_rejects_missing_or_different_values() {
        let (path, token) = token_file(b"correct horse battery staple");
        assert!(token.matches(Some("correct horse battery staple")));
        assert!(!token.matches(Some("correct horse battery stapl")));
        assert!(!token.matches(Some("correct horse battery stapler")));
        assert!(!token.matches(Some("")));
        assert!(!token.matches(None));
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    fn allows_one_final_line_ending_but_rejects_other_line_breaks() {
        for ending in [&b"token"[..], b"token\n", b"token\r\n"] {
            let (path, token) = token_file(ending);
            assert!(token.matches(Some("token")));
            std::fs::remove_file(path).unwrap();
        }
        for invalid in [&b""[..], b"token\n\n", b"tok\r\nen", b"token\r"] {
            let path = std::env::temp_dir().join(format!(
                "rdp-launcher-auth-invalid-{}-{}.token",
                std::process::id(),
                FILES.fetch_add(1, Ordering::SeqCst)
            ));
            std::fs::write(&path, invalid).unwrap();
            assert!(Token::load(&path).is_err(), "accepted {invalid:?}");
            std::fs::remove_file(path).unwrap();
        }
    }

    #[test]
    fn non_loopback_bind_requires_a_token_file() {
        let error = match load_for_bind(bind("192.0.2.10:49153"), None) {
            Err(error) => error,
            Ok(_) => panic!("non-loopback bind started without a token"),
        };
        assert!(error.to_string().contains("--auth-token-file"));
        assert!(load_for_bind(bind("127.0.0.1:49153"), None)
            .unwrap()
            .is_none());
        assert!(load_for_bind(bind("[::1]:49153"), None).unwrap().is_none());

        let (path, _) = token_file(b"token");
        assert!(load_for_bind(bind("192.0.2.10:49153"), Some(&path))
            .unwrap()
            .is_some());
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    fn parses_the_flag_once_and_requires_its_value() {
        let args = vec![
            "backend".to_string(),
            "--bind".to_string(),
            "127.0.0.1:49153".to_string(),
            "--auth-token-file".to_string(),
            r"C:\ProgramData\rdp-launcher\agent-token".to_string(),
        ];
        assert_eq!(
            token_file_arg(&args).unwrap().unwrap(),
            PathBuf::from(r"C:\ProgramData\rdp-launcher\agent-token")
        );
        assert!(token_file_arg(&["backend".to_string()]).unwrap().is_none());
        assert!(token_file_arg(&["backend".to_string(), "--auth-token-file".to_string()]).is_err());
    }

    #[test]
    fn authorization_covers_the_envelope_field_only() {
        let (path, token) = token_file(b"secret");
        let mut request = serde_json::json!({"action": "run", "token": "secret", "target": "x"});
        assert!(authorize(&request, Some(&token)));
        remove_token_field(&mut request);
        assert_eq!(request, serde_json::json!({"action": "run", "target": "x"}));

        assert!(!authorize(
            &serde_json::json!({"action": "status", "token": "other"}),
            Some(&token)
        ));
        assert!(!authorize(
            &serde_json::json!({"action": "status"}),
            Some(&token)
        ));
        // Explicit loopback-only mode has no expected token.
        assert!(authorize(&serde_json::json!({"action": "status"}), None));
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    fn authorization_applies_to_every_action_name() {
        let (path, token) = token_file(b"secret");
        for action_name in [
            "ping",
            "status",
            "watch_status",
            "quit",
            "run",
            "open",
            "list_apps",
            "get_mapped_drives",
            "inspect_sessions",
            "logoff_session",
            "session_status",
        ] {
            assert!(
                authorize(
                    &serde_json::json!({"action": action_name, "token": "secret"}),
                    Some(&token)
                ),
                "{action_name} accepted its token"
            );
            assert!(
                !authorize(&serde_json::json!({"action": action_name}), Some(&token)),
                "{action_name} accepted a missing token"
            );
            assert!(
                !authorize(
                    &serde_json::json!({"action": action_name, "token": "wrong"}),
                    Some(&token)
                ),
                "{action_name} accepted a wrong token"
            );
        }
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    fn logged_action_is_only_the_action_name() {
        let request = serde_json::json!({
            "action": "run",
            "token": "do-not-log-this",
            "target": r"C:\secret-path\app.exe",
        });
        assert_eq!(action(&request), "run");
        assert!(!action(&request).contains("do-not-log-this"));
    }

    #[test]
    fn typed_requests_parse_after_the_envelope_field_is_removed() {
        let mut ping: Value =
            serde_json::from_str(r#"{"action":"ping","protocol":2,"token":"secret"}"#).unwrap();
        remove_token_field(&mut ping);
        assert!(matches!(
            serde_json::from_value::<crate::protocol::HostAction>(ping).unwrap(),
            crate::protocol::HostAction::Ping
        ));

        let mut run: Value = serde_json::from_str(
            r#"{"action":"run","protocol":2,"token":"secret","target":"notepad.exe","params":null,"session_ref":{"session_id":4,"authentication_id":"0000000003C517EA","boot_identifier":"11111111-2222-4333-8444-555555555555"}}"#,
        )
        .unwrap();
        remove_token_field(&mut run);
        match serde_json::from_value::<crate::protocol::HostAction>(run).unwrap() {
            crate::protocol::HostAction::Run { target, .. } => {
                assert_eq!(target, "notepad.exe")
            }
            other => panic!("unexpected request: {other:?}"),
        }
    }
}
