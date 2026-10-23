//! Wire contract shared by the host backend endpoint, the user worker and the
//! local CLI. All host requests carry `protocol: 2`; a request from a different
//! protocol generation is rejected with an explicit paired-upgrade message.
use serde::{Deserialize, Serialize};
use serde_json::Value;

pub const PROTOCOL: u32 = 2;
pub const ROLE_BACKGROUND: &str = "background";
pub const ROLE_USER: &str = "user";
/// Bound on one user-worker request or response frame (icons can be large).
pub const MAX_WORKER_FRAME_BYTES: usize = 32 * 1024 * 1024;
pub const UPGRADE_MESSAGE: &str =
    "protocol mismatch: Windows agent and host must be upgraded together (agent protocol 2)";

/// Identity of one Windows logon instance. The SID is verified separately; a
/// session number alone is never accepted as a logon identity.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct SessionRef {
    pub session_id: u32,
    /// AuthenticationId LUID as 16 uppercase hexadecimal characters.
    pub authentication_id: String,
    /// Firmware boot identifier as a canonical 8-4-4-4-12 GUID.
    pub boot_identifier: String,
}

impl SessionRef {
    pub fn validate(&self) -> Result<(), String> {
        if self.session_id == 0 {
            return Err("session_ref.session_id must be a non-zero user session".into());
        }
        if !is_authentication_id(&self.authentication_id) {
            return Err(
                "session_ref.authentication_id must be 16 uppercase hexadecimal characters".into(),
            );
        }
        if !is_boot_identifier(&self.boot_identifier) {
            return Err("session_ref.boot_identifier must be a canonical GUID".into());
        }
        Ok(())
    }

    /// The whole reference must be present before dispatch or logoff. Empty
    /// values are only used in display snapshots and are never executable.
    pub fn is_complete(&self) -> bool {
        self.validate().is_ok()
    }
}

pub fn is_authentication_id(value: &str) -> bool {
    value.len() == 16
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'A'..=b'F').contains(&byte))
}

pub fn is_boot_identifier(value: &str) -> bool {
    let bytes = value.as_bytes();
    bytes.len() == 36
        && [8usize, 13, 18, 23]
            .iter()
            .all(|index| bytes[*index] == b'-')
        && bytes.iter().enumerate().all(|(index, byte)| {
            [8usize, 13, 18, 23].contains(&index)
                || byte.is_ascii_digit()
                || (b'a'..=b'f').contains(byte)
                || (b'A'..=b'F').contains(byte)
        })
}

pub fn same_boot_identifier(left: &str, right: &str) -> bool {
    left.eq_ignore_ascii_case(right)
}

pub fn format_authentication_id(high_part: i32, low_part: u32) -> String {
    format!("{:08X}{:08X}", high_part as u32, low_part)
}

pub fn format_guid(data1: u32, data2: u16, data3: u16, data4: [u8; 8]) -> String {
    format!(
        "{data1:08x}-{data2:04x}-{data3:04x}-{:02x}{:02x}-{:02x}{:02x}{:02x}{:02x}{:02x}{:02x}",
        data4[0], data4[1], data4[2], data4[3], data4[4], data4[5], data4[6], data4[7]
    )
}

/// WTS_CLIENT_ADDRESS addresses are in network byte order. Unknown families
/// produce an empty display value rather than a guessed address.
pub fn format_client_address(address_family: u32, address: &[u8; 20]) -> String {
    match address_family {
        // AF_INET: WTS stores the IPv4 octets at Address[2..6], not [0..4].
        2 => format!(
            "{}.{}.{}.{}",
            address[2], address[3], address[4], address[5]
        ),
        // AF_INET6
        23 => {
            let mut segments = [0u16; 8];
            for (index, segment) in segments.iter_mut().enumerate() {
                *segment = u16::from_be_bytes([address[index * 2], address[index * 2 + 1]]);
            }
            std::net::Ipv6Addr::new(
                segments[0],
                segments[1],
                segments[2],
                segments[3],
                segments[4],
                segments[5],
                segments[6],
                segments[7],
            )
            .to_string()
        }
        _ => String::new(),
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct SessionEntry {
    pub reference: SessionRef,
    pub sid: String,
    pub user: String,
    pub domain: String,
    pub state: i32,
    pub client_name: String,
    pub client_address: String,
    pub logon_time: i64,
    pub connect_time: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub windows: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tray_icons: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub note: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Snapshot {
    pub revision: u64,
    pub fresh: bool,
    pub sessions: Vec<SessionEntry>,
}

/// A completed WTS observation before a revision is assigned.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Observation {
    pub fresh: bool,
    pub sessions: Vec<SessionEntry>,
}

/// Publish a new observation. A content change or a freshness change advances
/// the revision exactly once; an identical refresh leaves it unchanged.
pub fn apply_observation(previous: &Snapshot, observation: Observation) -> Snapshot {
    if previous.fresh == observation.fresh && previous.sessions == observation.sessions {
        return previous.clone();
    }
    Snapshot {
        revision: previous.revision.saturating_add(1),
        fresh: observation.fresh,
        sessions: observation.sessions,
    }
}

/// Invalidate an old snapshot after a timeout or failed query. The last session
/// list remains visible for display but is no longer usable for dispatch.
pub fn invalidate(previous: &Snapshot) -> Snapshot {
    if !previous.fresh {
        return previous.clone();
    }
    Snapshot {
        revision: previous.revision.saturating_add(1),
        fresh: false,
        sessions: previous.sessions.clone(),
    }
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct AppInfo {
    pub id: String,
    pub name: String,
    pub app_type: String, // "win32" or "uwp"
    pub target: String,
    // Discovery fact only. The host uses this to filter Start Menu/Desktop
    // folders; no host policy or exclusion rules belong in the scanner.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub shortcut_path: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub arguments: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub working_dir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub icon_base64: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Ui {
    pub windows: Option<u32>,
    pub tray_icons: Option<u32>,
}

#[derive(Deserialize, Debug, Clone)]
#[serde(tag = "action", rename_all = "snake_case")]
pub enum HostAction {
    Ping,
    Status,
    WatchStatus,
    Quit,
    InspectSessions {
        #[serde(default)]
        user: Option<String>,
    },
    LogoffSession {
        user: String,
        session_ref: SessionRef,
    },
    Run {
        target: String,
        #[serde(default)]
        params: Option<String>,
        #[serde(default)]
        working_dir: Option<String>,
        session_ref: SessionRef,
    },
    Open {
        file: String,
        session_ref: SessionRef,
    },
    ListApps {
        #[serde(default)]
        with_icons: bool,
        session_ref: SessionRef,
    },
    GetMappedDrives {
        session_ref: SessionRef,
    },
    SessionStatus {
        session_ref: SessionRef,
    },
}

impl HostAction {
    pub fn name(&self) -> &'static str {
        match self {
            Self::Ping => "ping",
            Self::Status => "status",
            Self::WatchStatus => "watch_status",
            Self::Quit => "quit",
            Self::InspectSessions { .. } => "inspect_sessions",
            Self::LogoffSession { .. } => "logoff_session",
            Self::Run { .. } => "run",
            Self::Open { .. } => "open",
            Self::ListApps { .. } => "list_apps",
            Self::GetMappedDrives { .. } => "get_mapped_drives",
            Self::SessionStatus { .. } => "session_status",
        }
    }

    pub fn session_ref(&self) -> Option<&SessionRef> {
        match self {
            Self::Run { session_ref, .. }
            | Self::Open { session_ref, .. }
            | Self::ListApps { session_ref, .. }
            | Self::GetMappedDrives { session_ref }
            | Self::SessionStatus { session_ref } => Some(session_ref),
            _ => None,
        }
    }
}

#[derive(Serialize, Deserialize, Debug, Clone)]
#[serde(tag = "action", rename_all = "snake_case")]
pub enum WorkerRequest {
    Run {
        target: String,
        #[serde(default)]
        params: Option<String>,
        #[serde(default)]
        working_dir: Option<String>,
    },
    Open {
        file: String,
    },
    ListApps {
        #[serde(default)]
        with_icons: bool,
    },
    GetMappedDrives,
    ObserveUi,
    SessionStatus,
}

impl From<HostAction> for Option<WorkerRequest> {
    fn from(action: HostAction) -> Self {
        match action {
            HostAction::Run {
                target,
                params,
                working_dir,
                ..
            } => Some(WorkerRequest::Run {
                target,
                params,
                working_dir,
            }),
            HostAction::Open { file, .. } => Some(WorkerRequest::Open { file }),
            HostAction::ListApps { with_icons, .. } => Some(WorkerRequest::ListApps { with_icons }),
            HostAction::GetMappedDrives { .. } => Some(WorkerRequest::GetMappedDrives),
            HostAction::SessionStatus { .. } => Some(WorkerRequest::SessionStatus),
            _ => None,
        }
    }
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct WorkerResponse {
    pub status: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub apps: Option<Vec<AppInfo>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub drives: Option<Vec<(String, String)>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ui: Option<Ui>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub session: Option<WorkerSessionStatus>,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq)]
pub struct WorkerSessionStatus {
    pub reference: SessionRef,
    pub session_id: u32,
    pub session_active: bool,
    pub sid: String,
    pub user: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub windows: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tray_icons: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub note: Option<String>,
}

impl WorkerResponse {
    pub fn ok() -> Self {
        Self {
            status: "ok".into(),
            message: None,
            apps: None,
            drives: None,
            ui: None,
            session: None,
        }
    }

    pub fn error(message: impl Into<String>) -> Self {
        Self {
            status: "error".into(),
            message: Some(message.into()),
            apps: None,
            drives: None,
            ui: None,
            session: None,
        }
    }
}

#[derive(Serialize, Debug, Clone)]
pub struct ErrorResponse {
    pub status: &'static str,
    pub protocol: u32,
    pub role: &'static str,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub outcome: Option<&'static str>,
}

impl ErrorResponse {
    pub fn protocol(message: impl Into<String>) -> Self {
        Self {
            status: "error",
            protocol: PROTOCOL,
            role: ROLE_BACKGROUND,
            message: message.into(),
            outcome: None,
        }
    }

    pub fn with_outcome(message: impl Into<String>, outcome: &'static str) -> Self {
        Self {
            outcome: Some(outcome),
            ..Self::protocol(message)
        }
    }
}

/// Return the request's protocol or the shared paired-upgrade error.
pub fn check_protocol(request: &Value) -> Result<(), ErrorResponse> {
    match request.get("protocol").and_then(Value::as_u64) {
        Some(value) if value == u64::from(PROTOCOL) => Ok(()),
        _ => Err(ErrorResponse::protocol(UPGRADE_MESSAGE)),
    }
}

/// Split one newline-terminated frame from a buffer. Returns `Ok(None)` when
/// more bytes are needed and an error when the frame is oversized or EOF ends
/// a partial frame.
pub fn take_frame(buffer: &mut Vec<u8>, max: usize) -> Result<Option<Vec<u8>>, String> {
    if let Some(index) = buffer.iter().position(|byte| *byte == b'\n') {
        if index + 1 > max {
            return Err(format!("frame exceeds {max} byte limit"));
        }
        let frame = buffer.drain(..=index).collect();
        return Ok(Some(frame));
    }
    if buffer.len() > max {
        return Err(format!("frame exceeds {max} byte limit"));
    }
    Ok(None)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn reference() -> SessionRef {
        SessionRef {
            session_id: 4,
            authentication_id: "00000000000003E7".into(),
            boot_identifier: "11111111-2222-4333-8444-555555555555".into(),
        }
    }

    #[test]
    fn session_reference_uses_the_approved_field_names_and_validation() {
        let value = serde_json::to_value(reference()).unwrap();
        assert_eq!(value["session_id"], 4);
        assert_eq!(value["authentication_id"], "00000000000003E7");
        assert_eq!(
            value["boot_identifier"],
            "11111111-2222-4333-8444-555555555555"
        );
        assert!(reference().validate().is_ok());

        for invalid in [
            SessionRef {
                session_id: 0,
                ..reference()
            },
            SessionRef {
                authentication_id: "00000000000003e7".into(),
                ..reference()
            },
            SessionRef {
                authentication_id: "00000000000003E".into(),
                ..reference()
            },
            SessionRef {
                boot_identifier: "not-a-guid".into(),
                ..reference()
            },
        ] {
            assert!(invalid.validate().is_err(), "accepted {invalid:?}");
        }
    }

    #[test]
    fn boot_identifiers_compare_without_case_folding_the_raw_reference() {
        assert!(same_boot_identifier(
            "aa11bb22-cc33-44dd-88ee-ff0011223344",
            "AA11BB22-CC33-44DD-88EE-FF0011223344"
        ));
        assert!(!same_boot_identifier(
            "aa11bb22-cc33-44dd-88ee-ff0011223344",
            "aa11bb22-cc33-44dd-88ee-ff0011223345"
        ));
    }

    #[test]
    fn formats_luid_guid_and_wts_client_addresses() {
        assert_eq!(format_authentication_id(0, 0x03E7), "00000000000003E7");
        assert_eq!(
            format_guid(
                0x1111_1111,
                0x2222,
                0x4333,
                [0x84, 0x44, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55]
            ),
            "11111111-2222-4333-8444-555555555555"
        );
        let mut v4 = [0u8; 20];
        // WTS IPv4 layout for the documentation address 192.0.2.1.
        v4[..6].copy_from_slice(&[0, 0, 192, 0, 2, 1]);
        assert_eq!(format_client_address(2, &v4), "192.0.2.1");
        v4[..2].copy_from_slice(&[0x12, 0x34]);
        assert_eq!(format_client_address(2, &v4), "192.0.2.1");
        assert_eq!(format_client_address(0, &v4), "");
        let mut v6 = [0u8; 20];
        v6[..16].copy_from_slice(&[0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1]);
        assert_eq!(format_client_address(23, &v6), "2001:db8::1");
    }

    #[test]
    fn missing_or_different_protocol_gets_the_paired_upgrade_error() {
        let error = check_protocol(&serde_json::json!({"action": "ping"})).unwrap_err();
        assert_eq!(error.protocol, PROTOCOL);
        assert_eq!(error.message, UPGRADE_MESSAGE);
        assert!(error.message.contains("upgraded together"));
        assert!(check_protocol(&serde_json::json!({"action": "ping", "protocol": 1})).is_err());
        assert!(check_protocol(&serde_json::json!({"action": "ping", "protocol": 2})).is_ok());
    }

    #[test]
    fn user_actions_require_a_complete_session_reference() {
        let wire = serde_json::json!({
            "action": "run",
            "protocol": 2,
            "target": "notepad.exe",
            "session_ref": reference(),
        });
        let action: HostAction = serde_json::from_value(wire).unwrap();
        assert_eq!(action.name(), "run");
        assert_eq!(action.session_ref(), Some(&reference()));

        let missing = serde_json::json!({"action":"open","protocol":2,"file":"x.txt"});
        assert!(serde_json::from_value::<HostAction>(missing).is_err());
    }

    #[test]
    fn inspect_and_logoff_requests_keep_user_scoping_fields() {
        let inspect: HostAction =
            serde_json::from_value(serde_json::json!({"action":"inspect_sessions","protocol":2}))
                .unwrap();
        assert!(matches!(
            inspect,
            HostAction::InspectSessions { user: None }
        ));
        let inspect: HostAction = serde_json::from_value(serde_json::json!({
            "action":"inspect_sessions","protocol":2,"user":"user"
        }))
        .unwrap();
        assert!(matches!(
            inspect,
            HostAction::InspectSessions { user: Some(ref user) } if user == "user"
        ));

        let logoff: HostAction = serde_json::from_value(serde_json::json!({
            "action":"logoff_session","protocol":2,"user":"user","session_ref":reference()
        }))
        .unwrap();
        match logoff {
            HostAction::LogoffSession { user, session_ref } => {
                assert_eq!(user, "user");
                assert_eq!(session_ref, reference());
            }
            other => panic!("unexpected {other:?}"),
        }
    }

    #[test]
    fn observation_revision_only_advances_on_visible_change() {
        let empty = Snapshot::default();
        let first = apply_observation(
            &empty,
            Observation {
                fresh: true,
                sessions: vec![],
            },
        );
        assert_eq!(first.revision, 1);
        assert!(first.fresh);
        let identical = apply_observation(
            &first,
            Observation {
                fresh: true,
                sessions: vec![],
            },
        );
        assert_eq!(identical, first);
        let invalidated = invalidate(&first);
        assert_eq!(invalidated.revision, 2);
        assert!(!invalidated.fresh);
        assert_eq!(invalidate(&invalidated), invalidated);
    }

    #[test]
    fn optional_session_ui_fields_are_omitted_when_unknown() {
        let entry = SessionEntry {
            reference: reference(),
            sid: "S-1-5-21-1".into(),
            user: "user".into(),
            domain: "machine".into(),
            state: 0,
            client_name: "rdp-test-client".into(),
            client_address: "192.0.2.10".into(),
            logon_time: 100,
            connect_time: 200,
            windows: None,
            tray_icons: None,
            note: None,
        };
        let value = serde_json::to_value(entry).unwrap();
        assert!(value.get("windows").is_none());
        assert!(value.get("tray_icons").is_none());
        assert!(value.get("note").is_none());
        assert_eq!(value["state"], 0);
        assert_eq!(value["reference"]["session_id"], 4);
    }

    #[test]
    fn worker_actions_drop_the_host_only_reference() {
        let action = HostAction::Run {
            target: "notepad.exe".into(),
            params: Some("file.txt".into()),
            working_dir: None,
            session_ref: reference(),
        };
        let worker = Option::<WorkerRequest>::from(action).unwrap();
        let encoded = serde_json::to_value(&worker).unwrap();
        assert_eq!(encoded["action"], "run");
        assert!(encoded.get("session_ref").is_none());
        assert_eq!(encoded["target"], "notepad.exe");

        let worker: WorkerRequest =
            serde_json::from_str(r#"{"action":"get_mapped_drives"}"#).unwrap();
        assert!(matches!(worker, WorkerRequest::GetMappedDrives));
        let worker: WorkerRequest = serde_json::from_str(r#"{"action":"observe_ui"}"#).unwrap();
        assert!(matches!(worker, WorkerRequest::ObserveUi));
    }

    #[test]
    fn session_status_is_a_routed_user_action() {
        let action: HostAction = serde_json::from_value(serde_json::json!({
            "action": "session_status",
            "protocol": 2,
            "session_ref": reference(),
        }))
        .unwrap();
        assert_eq!(action.name(), "session_status");
        assert_eq!(action.session_ref(), Some(&reference()));
        let worker = Option::<WorkerRequest>::from(action).unwrap();
        assert!(matches!(worker, WorkerRequest::SessionStatus));
    }

    #[test]
    fn frame_splitter_requires_a_newline_and_enforces_the_limit() {
        let mut partial = b"{\"a\":1}".to_vec();
        assert_eq!(take_frame(&mut partial, 64).unwrap(), None);
        partial.push(b'\n');
        assert_eq!(
            take_frame(&mut partial, 64).unwrap(),
            Some(b"{\"a\":1}\n".to_vec())
        );
        assert!(partial.is_empty());

        let mut oversized = vec![b'x'; 65];
        assert!(take_frame(&mut oversized, 64).is_err());
        let mut with_newline = vec![b'x'; 65];
        with_newline.push(b'\n');
        assert!(take_frame(&mut with_newline, 64).is_err());
    }
}
