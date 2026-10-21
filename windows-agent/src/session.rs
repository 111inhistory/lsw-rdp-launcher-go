//! Session-scoped observation and explicit logoff. SSH/service session 0 is
//! never selected. A short-lived copy of this binary observes UI in the user's
//! desktop; the SYSTEM backend itself never creates a GUI window.
use serde::{Deserialize, Serialize};
use std::{
    fs, io, mem,
    path::{Path, PathBuf},
    ptr, slice,
};
use windows_sys::Win32::{
    Foundation::*,
    Security::{Authorization::*, *},
    Storage::FileSystem::CreateDirectoryW,
    System::{RemoteDesktop::*, Threading::*},
};

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Activity {
    pub session_id: u32,
    pub user: String,
    pub fingerprint: String,
    pub windows: Option<u32>,
    pub tray_icons: Option<u32>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub note: String,
}
#[derive(Serialize, Deserialize)]
pub struct Ui {
    pub windows: Option<u32>,
    pub tray_icons: Option<u32>,
}
struct Session {
    id: u32,
    user: String,
    domain: String,
    logon: i64,
    state: WTS_CONNECTSTATE_CLASS,
}
fn wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(Some(0)).collect()
}
fn text(s: &[u16]) -> String {
    String::from_utf16_lossy(&s[..s.iter().position(|v| *v == 0).unwrap_or(s.len())])
}
impl Session {
    fn owner(&self) -> String {
        format!("{}\\{}", self.domain, self.user)
    }
    fn fingerprint(&self) -> String {
        if self.logon <= 0 {
            return String::new();
        }
        format!("{}:{}:{}", self.id, self.logon, self.owner())
    }
    fn matches(&self, user: &str) -> bool {
        self.user.eq_ignore_ascii_case(user) || self.owner().eq_ignore_ascii_case(user)
    }
}
fn session(id: u32) -> io::Result<Session> {
    unsafe {
        let mut buffer = ptr::null_mut();
        let mut size = 0;
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
        if size < (mem::size_of::<WTSINFOEXW>() as u32) {
            WTSFreeMemory(buffer.cast());
            return Err(io::Error::other("short WTS session info"));
        }
        let info = *(buffer as *const WTSINFOEXW);
        WTSFreeMemory(buffer.cast());
        if info.Level != 1 {
            return Err(io::Error::other("unsupported WTS session info level"));
        }
        let info = info.Data.WTSInfoExLevel1;
        Ok(Session {
            id,
            user: text(&info.UserName),
            domain: text(&info.DomainName),
            logon: info.LogonTime,
            state: info.SessionState,
        })
    }
}
fn sessions(user: &str) -> io::Result<Vec<Session>> {
    unsafe {
        let mut data = ptr::null_mut();
        let mut count = 0;
        if WTSEnumerateSessionsW(ptr::null_mut(), 0, 1, &mut data, &mut count) == 0 {
            return Err(io::Error::last_os_error());
        }
        let ids: Vec<_> = slice::from_raw_parts(data, count as usize)
            .iter()
            .filter(|s| s.SessionId != 0)
            .map(|s| s.SessionId)
            .collect();
        WTSFreeMemory(data.cast());
        let mut found = Vec::new();
        for id in ids {
            let s = session(id)?;
            if !s.user.is_empty() && s.matches(user) {
                found.push(s)
            }
        }
        Ok(found)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn logon_identity_is_not_just_the_session_number() {
        let mut s = Session {
            id: 2,
            user: "user".into(),
            domain: "machine".into(),
            logon: 100,
            state: WTSActive,
        };
        let old = s.fingerprint();
        assert!(s.matches("USER"));
        assert!(s.matches("MACHINE\\USER"));
        assert!(!s.matches("other"));
        s.logon += 1;
        assert_ne!(old, s.fingerprint());
        s.logon = 0;
        assert!(s.fingerprint().is_empty());
    }
}

pub fn current_active() -> (Option<u32>, Option<bool>) {
    let mut id = 0;
    unsafe {
        if ProcessIdToSessionId(GetCurrentProcessId(), &mut id) == 0 {
            return (None, None);
        }
    }
    (Some(id), session(id).ok().map(|s| s.state == WTSActive))
}

pub fn inspect(user: &str) -> io::Result<Activity> {
    let mut all = sessions(user)?;
    if all.is_empty() {
        return Ok(Activity {
            session_id: 0,
            user: user.into(),
            fingerprint: String::new(),
            windows: Some(0),
            tray_icons: Some(0),
            note: String::new(),
        });
    }
    let active: Vec<_> = all
        .iter()
        .enumerate()
        .filter(|(_, s)| s.state == WTSActive)
        .map(|(i, _)| i)
        .collect();
    // Pro normally has one active user desktop, but may retain disconnected
    // logons. State selects a target, not whether its applications are idle.
    let selected = if active.len() == 1 {
        Some(active[0])
    } else if all.len() == 1 {
        Some(0)
    } else {
        None
    };
    let Some(index) = selected else {
        return Ok(Activity {
            session_id: 0,
            user: user.into(),
            fingerprint: String::new(),
            windows: None,
            tray_icons: None,
            note: "多个用户会话，无法唯一确定注销目标；仅允许保留或取消。".into(),
        });
    };
    let s = all.remove(index);
    let mut result = Activity {
        session_id: s.id,
        user: s.owner(),
        fingerprint: s.fingerprint(),
        windows: None,
        tray_icons: None,
        note: String::new(),
    };
    match probe(s.id) {
        Ok(ui) => {
            result.windows = ui.windows;
            result.tray_icons = ui.tray_icons;
            if ui.tray_icons.is_none() {
                result.note = "无法可靠确认用户托盘状态，不会自动注销。".into()
            }
        }
        Err(e) => result.note = format!("无法读取该会话的界面状态：{e}；不会自动注销。"),
    }
    Ok(result)
}
pub fn logoff(user: &str, id: u32, fingerprint: &str, require_empty: bool) -> io::Result<()> {
    if id == 0 || fingerprint.is_empty() {
        return Err(io::Error::other("an identified user session is required"));
    }
    let s = session(id)?;
    if !s.matches(user) || s.fingerprint() != fingerprint {
        return Err(io::Error::other(
            "session owner or logon changed; refusing logoff",
        ));
    }
    // The pre-dialog observation can go stale. Automatic logoff requires a
    // fresh empty snapshot; explicit consent permits closing all target apps.
    if require_empty {
        let ui = probe(id)?;
        if ui.windows != Some(0) || ui.tray_icons != Some(0) {
            return Err(io::Error::other(
                "session is no longer confirmed empty; refusing automatic logoff",
            ));
        }
    }
    if session(id)?.fingerprint() != fingerprint {
        return Err(io::Error::other("session changed before logoff"));
    }
    unsafe {
        if WTSLogoffSession(ptr::null_mut(), id, 1) == 0 {
            return Err(io::Error::last_os_error());
        }
    }
    if sessions(user)?
        .iter()
        .any(|s| s.id == id && s.fingerprint() == fingerprint)
    {
        return Err(io::Error::other("session logoff was not confirmed"));
    }
    Ok(())
}
struct ProbeDir(PathBuf);
impl Drop for ProbeDir {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
struct Handle(HANDLE);
impl Drop for Handle {
    fn drop(&mut self) {
        unsafe {
            if !self.0.is_null() {
                CloseHandle(self.0);
            }
        }
    }
}
fn probe(id: u32) -> io::Result<Ui> {
    unsafe {
        let mut token = ptr::null_mut();
        if WTSQueryUserToken(id, &mut token) == 0 {
            return Err(io::Error::last_os_error());
        }
        let token = Handle(token);
        let mut needed = 0;
        GetTokenInformation(token.0, TokenUser, ptr::null_mut(), 0, &mut needed);
        let mut info = vec![0u8; needed as usize];
        if GetTokenInformation(
            token.0,
            TokenUser,
            info.as_mut_ptr().cast(),
            needed,
            &mut needed,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        let mut sid = ptr::null_mut();
        if ConvertSidToStringSidW(
            ptr::read_unaligned(info.as_ptr() as *const TOKEN_USER)
                .User
                .Sid,
            &mut sid,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        let mut length = 0;
        while *sid.add(length) != 0 {
            length += 1
        }
        let sid_string = String::from_utf16_lossy(slice::from_raw_parts(sid, length));
        LocalFree(sid.cast());
        let mut nonce = [0u8; 16];
        if windows_sys::Win32::Security::Cryptography::BCryptGenRandom(
            ptr::null_mut(),
            nonce.as_mut_ptr(),
            16,
            windows_sys::Win32::Security::Cryptography::BCRYPT_USE_SYSTEM_PREFERRED_RNG,
        ) < 0
        {
            return Err(io::Error::other("probe nonce generation failed"));
        }
        let name: String = nonce.iter().map(|b| format!("{b:02x}")).collect();
        let path = std::env::temp_dir().join(format!("rdp-session-probe-{name}"));
        // These counts can authorize automatic logoff. Other local users must
        // not be able to replace the helper's response with forged zero counts.
        let sddl = wide(&format!("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;{sid_string})"));
        let mut descriptor = ptr::null_mut();
        if ConvertStringSecurityDescriptorToSecurityDescriptorW(
            sddl.as_ptr(),
            1,
            &mut descriptor,
            ptr::null_mut(),
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        let sa = SECURITY_ATTRIBUTES {
            nLength: mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: descriptor,
            bInheritHandle: 0,
        };
        let created = CreateDirectoryW(wide(&path.to_string_lossy()).as_ptr(), &sa);
        let error = io::Error::last_os_error();
        LocalFree(descriptor);
        if created == 0 {
            return Err(error);
        }
        let directory = ProbeDir(path);
        let output = directory.0.join("activity.json");
        let exe = std::env::current_exe()?;
        // Windows paths cannot contain double quotes. Only fixed flags and absolute
        // paths generated by this process appear in this private helper command line.
        let mut cmd = wide(&format!(
            "\"{}\" session-probe \"{}\"",
            exe.display(),
            output.display()
        ));
        let mut startup: STARTUPINFOW = mem::zeroed();
        startup.cb = mem::size_of::<STARTUPINFOW>() as u32;
        let mut desktop = wide("winsta0\\default");
        startup.lpDesktop = desktop.as_mut_ptr();
        startup.dwFlags = STARTF_USESHOWWINDOW;
        startup.wShowWindow = 0;
        let mut process: PROCESS_INFORMATION = mem::zeroed();
        if CreateProcessAsUserW(
            token.0,
            wide(&exe.to_string_lossy()).as_ptr(),
            cmd.as_mut_ptr(),
            ptr::null(),
            ptr::null(),
            0,
            CREATE_NO_WINDOW,
            ptr::null(),
            ptr::null(),
            &startup,
            &mut process,
        ) == 0
        {
            return Err(io::Error::last_os_error());
        }
        let process_handle = Handle(process.hProcess);
        let _thread_handle = Handle(process.hThread);
        if WaitForSingleObject(process_handle.0, 8000) != WAIT_OBJECT_0 {
            TerminateProcess(process_handle.0, 1);
            WaitForSingleObject(process_handle.0, 1000);
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "session UI probe timed out",
            ));
        }
        let raw = fs::read(&output)?;
        if raw.len() > 65536 {
            return Err(io::Error::other("oversized UI probe response"));
        }
        serde_json::from_slice(&raw).map_err(io::Error::other)
    }
}
pub fn write_probe(path: &Path) -> io::Result<()> {
    let ui = crate::session_ui::observe();
    fs::write(path, serde_json::to_vec(&ui).map_err(io::Error::other)?)
}
