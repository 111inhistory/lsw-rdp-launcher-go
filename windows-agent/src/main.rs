#![windows_subsystem = "windows"]
#![allow(non_snake_case, dead_code)]

mod auth;
mod backend;
mod memory;
mod session;
mod session_ui;
mod ssh;

use std::collections::HashMap;
use std::ffi::OsString;
use std::fs;
use std::os::windows::ffi::{OsStrExt, OsStringExt};
use std::path::{Path, PathBuf};
use std::ptr;
use std::result::Result::Ok;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{mpsc, Arc};

use serde::{Deserialize, Serialize};
use windows_sys::core::{GUID, HRESULT};
use windows_sys::Win32::Foundation::*;
use windows_sys::Win32::Graphics::GdiPlus::*;
use windows_sys::Win32::NetworkManagement::WNet::*;
use windows_sys::Win32::Security::Cryptography::*;
use windows_sys::Win32::Storage::FileSystem::*;
use windows_sys::Win32::System::Com::StructuredStorage::*;
use windows_sys::Win32::System::Com::*;
use windows_sys::Win32::System::Console::*;
use windows_sys::Win32::System::LibraryLoader::*;
use windows_sys::Win32::System::Memory::*;
use windows_sys::Win32::System::Power::*;
use windows_sys::Win32::System::RemoteDesktop::{
    WTSRegisterSessionNotification, WTSUnRegisterSessionNotification, NOTIFY_FOR_THIS_SESSION,
};
use windows_sys::Win32::System::Threading::*;
use windows_sys::Win32::UI::Input::KeyboardAndMouse::*;
use windows_sys::Win32::UI::Shell::*;
use windows_sys::Win32::UI::WindowsAndMessaging::*;

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

const DRIVE_REMOTE: u32 = 4;

fn query_mapped_network_drives() -> Vec<(String, String)> {
    let mut results = Vec::new();
    unsafe {
        let mask = GetLogicalDrives();
        for i in 0..26 {
            if (mask & (1 << i)) != 0 {
                let letter = (b'A' + i as u8) as char;
                let root_str = format!("{}:\\", letter);
                let wide_root = to_wide(&root_str);
                if GetDriveTypeW(wide_root.as_ptr()) == DRIVE_REMOTE {
                    let drive_name = format!("{}:", letter);
                    let wide_drive = to_wide(&drive_name);
                    let mut buf = [0u16; 512];
                    let mut len = 512u32;
                    if WNetGetConnectionW(wide_drive.as_ptr(), buf.as_mut_ptr(), &mut len)
                        == NO_ERROR
                    {
                        let unc = from_wide(&buf);
                        results.push((drive_name, unc));
                    }
                }
            }
        }
    }
    results
}

const CLSID_PNG: GUID = GUID {
    data1: 0x557cf406,
    data2: 0x1a04,
    data3: 0x11d3,
    data4: [0x9a, 0x73, 0x00, 0x00, 0xf8, 0x1e, 0xf3, 0x2e],
};

const CLSID_SHELL_LINK: GUID = GUID {
    data1: 0x00021401,
    data2: 0x0000,
    data3: 0x0000,
    data4: [0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46],
};

const IID_ISHELL_LINK_W: GUID = GUID {
    data1: 0x000214F9,
    data2: 0x0000,
    data3: 0x0000,
    data4: [0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46],
};

const IID_IPERSIST_FILE: GUID = GUID {
    data1: 0x0000010b,
    data2: 0x0000,
    data3: 0x0000,
    data4: [0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46],
};

#[repr(C)]
struct IShellLinkWVtbl {
    pub QueryInterface: unsafe extern "system" fn(
        this: *mut std::ffi::c_void,
        riid: *const GUID,
        ppvObject: *mut *mut std::ffi::c_void,
    ) -> HRESULT,
    pub AddRef: unsafe extern "system" fn(this: *mut std::ffi::c_void) -> u32,
    pub Release: unsafe extern "system" fn(this: *mut std::ffi::c_void) -> u32,
    pub GetPath: unsafe extern "system" fn(
        this: *mut std::ffi::c_void,
        pszFile: *mut u16,
        cch: i32,
        pfd: *mut std::ffi::c_void,
        fFlags: u32,
    ) -> HRESULT,
    pub GetIDList: usize,
    pub SetIDList: usize,
    pub GetDescription: usize,
    pub SetDescription: usize,
    pub GetWorkingDirectory: unsafe extern "system" fn(
        this: *mut std::ffi::c_void,
        pszDir: *mut u16,
        cch: i32,
    ) -> HRESULT,
    pub SetWorkingDirectory: usize,
    pub GetArguments: unsafe extern "system" fn(
        this: *mut std::ffi::c_void,
        pszArgs: *mut u16,
        cch: i32,
    ) -> HRESULT,
    pub SetArguments: usize,
    pub GetHotkey: usize,
    pub SetHotkey: usize,
    pub GetShowCmd: usize,
    pub SetShowCmd: usize,
    pub GetIconLocation: unsafe extern "system" fn(
        this: *mut std::ffi::c_void,
        pszIconPath: *mut u16,
        cch: i32,
        piIcon: *mut i32,
    ) -> HRESULT,
}

#[repr(C)]
struct IPersistFileVtbl {
    pub QueryInterface: unsafe extern "system" fn(
        this: *mut std::ffi::c_void,
        riid: *const GUID,
        ppvObject: *mut *mut std::ffi::c_void,
    ) -> HRESULT,
    pub AddRef: unsafe extern "system" fn(this: *mut std::ffi::c_void) -> u32,
    pub Release: unsafe extern "system" fn(this: *mut std::ffi::c_void) -> u32,
    pub GetClassID: usize,
    pub IsDirty: usize,
    pub Load: unsafe extern "system" fn(
        this: *mut std::ffi::c_void,
        pszFileName: *const u16,
        dwMode: u32,
    ) -> HRESULT,
    pub Save: usize,
    pub SaveCompleted: usize,
    pub GetCurFile: usize,
}

const WM_TRAYICON: u32 = WM_USER + 1;
const ID_TRAY_EXIT: usize = 1001;
const ID_TRAY_ABOUT: usize = 1002;
const TIMER_KEEPALIVE_ID: usize = 1;

const SEE_MASK_DOENVSUBST: u32 = 0x00000200;
const SEE_MASK_FLAG_NO_UI: u32 = 0x00000400;

static RUNNING: AtomicBool = AtomicBool::new(true);
static ACTIVE_HANDLERS: AtomicUsize = AtomicUsize::new(0);
static RDP_KEEPALIVE_ACTIVE: AtomicBool = AtomicBool::new(true);
static LIST_APPS_RUNNING: AtomicBool = AtomicBool::new(false);

/// Releases the active-handler slot even if a handler thread unwinds.
struct ActiveHandlerGuard;

impl Drop for ActiveHandlerGuard {
    fn drop(&mut self) {
        ACTIVE_HANDLERS.fetch_sub(1, Ordering::SeqCst);
    }
}

/// Exactly one list_apps scan may run at a time; the guard resets the flag even
/// if the scan thread unwinds.
struct ListAppsScanGuard;

impl Drop for ListAppsScanGuard {
    fn drop(&mut self) {
        LIST_APPS_RUNNING.store(false, Ordering::SeqCst);
    }
}

/// Maximum accepted request frame size; prevents unbounded line reads.
const MAX_REQUEST_BYTES: u64 = 64 * 1024;
/// Total I/O budget for reading and writing a single client connection.
const CLIENT_IO_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(5);
/// Upper bound on concurrently handled connections; surplus connections get a busy response.
const MAX_ACTIVE_HANDLERS: usize = 8;
/// Upper bound on queued serial application commands.
const MAX_PENDING_COMMANDS: usize = 32;
/// list_apps responses can be large (icons); allow a longer write budget.
const LIST_APPS_WRITE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(60);

// Session-change reasons from wtsapi32.h; windows-sys 0.59 does not re-export these.
const WTS_REMOTE_CONNECT: u32 = 0x3;
const WTS_REMOTE_DISCONNECT: u32 = 0x4;

fn apply_execution_state(active: bool) {
    unsafe {
        if active {
            SetThreadExecutionState(
                ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_AWAYMODE_REQUIRED | ES_DISPLAY_REQUIRED,
            );
        } else {
            SetThreadExecutionState(ES_CONTINUOUS);
        }
    }
}

fn to_wide(s: &str) -> Vec<u16> {
    std::ffi::OsStr::new(s)
        .encode_wide()
        .chain(std::iter::once(0))
        .collect()
}

fn from_wide(buf: &[u16]) -> String {
    let len = buf.iter().position(|&c| c == 0).unwrap_or(buf.len());
    OsString::from_wide(&buf[..len])
        .to_string_lossy()
        .into_owned()
}

fn extract_icon_base64_png(exe_path: &str, size: i32) -> Option<String> {
    unsafe {
        let wide_path = to_wide(exe_path);
        let mut hicon: HICON = ptr::null_mut();
        let mut icon_id: u32 = 0;
        let count = PrivateExtractIconsW(
            wide_path.as_ptr(),
            0,
            size,
            size,
            &mut hicon,
            &mut icon_id,
            1,
            0,
        );
        if count == 0 || hicon.is_null() {
            if size > 64 {
                return extract_icon_base64_png(exe_path, 64);
            }
            return None;
        }

        let mut gp_bitmap: *mut GpBitmap = ptr::null_mut();
        if GdipCreateBitmapFromHICON(hicon, &mut gp_bitmap) != 0 {
            DestroyIcon(hicon);
            return None;
        }

        let mut stream: *mut std::ffi::c_void = ptr::null_mut();
        if CreateStreamOnHGlobal(ptr::null_mut(), 1, &mut stream) != 0 {
            GdipDisposeImage(gp_bitmap as *mut _);
            DestroyIcon(hicon);
            return None;
        }

        let status = GdipSaveImageToStream(
            gp_bitmap as *mut _,
            stream as *mut _,
            &CLSID_PNG,
            ptr::null(),
        );
        GdipDisposeImage(gp_bitmap as *mut _);
        DestroyIcon(hicon);

        if status != 0 {
            let vtbl = *(stream as *const *const usize);
            let release_fn: unsafe extern "system" fn(*mut std::ffi::c_void) -> u32 =
                std::mem::transmute(*vtbl.add(2));
            release_fn(stream);
            return None;
        }

        let mut hglobal: HGLOBAL = ptr::null_mut();
        GetHGlobalFromStream(stream, &mut hglobal);
        let sz = GlobalSize(hglobal);
        let ptr = GlobalLock(hglobal) as *const u8;

        let mut b64_len = 0u32;
        CryptBinaryToStringA(
            ptr,
            sz as u32,
            0x00000001 | 0x40000000,
            ptr::null_mut(),
            &mut b64_len,
        );

        let mut b64_buf = vec![0u8; b64_len as usize];
        CryptBinaryToStringA(
            ptr,
            sz as u32,
            0x00000001 | 0x40000000,
            b64_buf.as_mut_ptr(),
            &mut b64_len,
        );

        GlobalUnlock(hglobal);
        let vtbl = *(stream as *const *const usize);
        let release_fn: unsafe extern "system" fn(*mut std::ffi::c_void) -> u32 =
            std::mem::transmute(*vtbl.add(2));
        release_fn(stream);

        if let Some(&0) = b64_buf.last() {
            b64_buf.pop();
        }

        String::from_utf8(b64_buf).ok()
    }
}

fn sanitize_id(name: &str) -> String {
    match name {
        "记事本" => return "notepad".to_string(),
        "计算器" => return "calculator".to_string(),
        "终端" => return "terminal".to_string(),
        "设置" => return "settings".to_string(),
        "Windows 安全中心" => return "windows-security".to_string(),
        "Windows 备份" => return "windows-backup".to_string(),
        "图吧工具箱" => return "tb-toolbox".to_string(),
        "UU远程" => return "uu-remote".to_string(),
        "入门" => return "get-started".to_string(),
        "单击以执行" => return "click-to-do".to_string(),
        "Office 语言首选项" => return "office-language-preferences".to_string(),
        _ => {}
    }

    let s: String = name
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() {
                c.to_ascii_lowercase()
            } else {
                '-'
            }
        })
        .collect();
    let trimmed = s.trim_matches('-').to_string();
    if trimmed.is_empty() {
        name.to_lowercase()
    } else {
        trimmed
    }
}

type ShortcutTarget = (String, Option<String>, Option<String>, Option<String>);

fn parse_lnk_file(
    sl: *mut std::ffi::c_void,
    pf: *mut std::ffi::c_void,
    lnk_path: &Path,
) -> Option<ShortcutTarget> {
    unsafe {
        let vtbl_sl = *(sl as *const *const IShellLinkWVtbl);
        let vtbl_pf = *(pf as *const *const IPersistFileVtbl);

        let wide_path = to_wide(&lnk_path.to_string_lossy());
        let hr = ((*vtbl_pf).Load)(pf, wide_path.as_ptr(), 0);
        if hr != 0 {
            return None;
        }

        let mut target_buf = [0u16; 512];
        let mut args_buf = [0u16; 512];
        let mut work_buf = [0u16; 512];
        let mut icon_buf = [0u16; 512];
        let mut icon_idx = 0i32;

        ((*vtbl_sl).GetPath)(sl, target_buf.as_mut_ptr(), 512, ptr::null_mut(), 0);
        ((*vtbl_sl).GetArguments)(sl, args_buf.as_mut_ptr(), 512);
        ((*vtbl_sl).GetWorkingDirectory)(sl, work_buf.as_mut_ptr(), 512);
        ((*vtbl_sl).GetIconLocation)(sl, icon_buf.as_mut_ptr(), 512, &mut icon_idx);

        let target = from_wide(&target_buf);
        if target.is_empty() {
            return None;
        }

        let args = from_wide(&args_buf);
        let work = from_wide(&work_buf);
        let icon = from_wide(&icon_buf);

        Some((
            target,
            if args.is_empty() { None } else { Some(args) },
            if work.is_empty() { None } else { Some(work) },
            if icon.is_empty() {
                None
            } else {
                Some(format!("{},{}", icon, icon_idx))
            },
        ))
    }
}

fn scan_shortcuts(with_icons: bool) -> Vec<AppInfo> {
    let mut apps: HashMap<String, AppInfo> = HashMap::new();

    unsafe {
        CoInitialize(ptr::null());
        let mut gdi_token: usize = 0;
        if with_icons {
            let mut input: GdiplusStartupInput = std::mem::zeroed();
            input.GdiplusVersion = 1;
            GdiplusStartup(&mut gdi_token, &input, ptr::null_mut());
        }

        let mut sl: *mut std::ffi::c_void = ptr::null_mut();
        if CoCreateInstance(
            &CLSID_SHELL_LINK,
            ptr::null_mut(),
            CLSCTX_INPROC_SERVER,
            &IID_ISHELL_LINK_W,
            &mut sl,
        ) == 0
            && !sl.is_null()
        {
            let vtbl_sl = *(sl as *const *const IShellLinkWVtbl);
            let mut pf: *mut std::ffi::c_void = ptr::null_mut();
            let persist_ok =
                ((*vtbl_sl).QueryInterface)(sl, &IID_IPERSIST_FILE, &mut pf) == 0 && !pf.is_null();

            // Shortcut scanning needs IPersistFile; skip it rather than
            // dereferencing a null interface when QueryInterface fails.
            if persist_ok {
                // 1. Scan Start Menu FIRST (Priority)
                let mut start_menu_dirs = Vec::new();
                if let Ok(progdata) = std::env::var("ProgramData") {
                    start_menu_dirs.push(
                        PathBuf::from(progdata).join("Microsoft\\Windows\\Start Menu\\Programs"),
                    );
                }
                if let Ok(appdata) = std::env::var("APPDATA") {
                    start_menu_dirs.push(
                        PathBuf::from(appdata).join("Microsoft\\Windows\\Start Menu\\Programs"),
                    );
                }

                for dir in start_menu_dirs {
                    scan_dir_shortcuts(&dir, sl, pf, with_icons, &mut apps, true);
                }

                // 2. Scan Desktop SECOND (De-duplicate: skip if TargetPath already exists)
                let mut desktop_dirs = Vec::new();
                if let Ok(pub_profile) = std::env::var("PUBLIC") {
                    desktop_dirs.push(PathBuf::from(pub_profile).join("Desktop"));
                }
                if let Ok(user_profile) = std::env::var("USERPROFILE") {
                    desktop_dirs.push(PathBuf::from(user_profile).join("Desktop"));
                }

                for dir in desktop_dirs {
                    scan_dir_shortcuts(&dir, sl, pf, with_icons, &mut apps, false);
                }

                let vtbl_pf = *(pf as *const *const IPersistFileVtbl);
                ((*vtbl_pf).Release)(pf);
            }

            ((*vtbl_sl).Release)(sl);
        }

        // 3. Scan UWP / MSIX Apps from Get-StartApps
        scan_uwp_apps(&mut apps);

        if with_icons && gdi_token != 0 {
            GdiplusShutdown(gdi_token);
        }
        CoUninitialize();
    }

    let mut result: Vec<AppInfo> = apps.into_values().collect();
    result.sort_by_key(|a| a.name.to_lowercase());
    result
}

fn scan_uwp_apps(apps: &mut HashMap<String, AppInfo>) {
    use std::process::Command;
    let output = Command::new("powershell.exe")
        .args([
            "-NoProfile",
            "-Command",
            "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; Get-StartApps | Where-Object { $_.AppID -match '!' } | Select-Object Name, AppID | ConvertTo-Json",
        ])
        .output();

    if let Ok(out) = output {
        if out.status.success() {
            #[derive(Deserialize)]
            struct StartAppItem {
                Name: String,
                AppID: String,
            }

            let text = String::from_utf8_lossy(&out.stdout);
            if let Ok(items) = serde_json::from_str::<Vec<StartAppItem>>(&text) {
                for item in items {
                    let norm_key = item.AppID.to_lowercase();
                    apps.entry(norm_key).or_insert_with(|| AppInfo {
                        id: sanitize_id(&item.Name),
                        name: item.Name,
                        app_type: "uwp".to_string(),
                        target: item.AppID,
                        shortcut_path: None,
                        arguments: None,
                        working_dir: None,
                        icon_base64: None,
                    });
                }
            } else if let Ok(item) = serde_json::from_str::<StartAppItem>(&text) {
                let norm_key = item.AppID.to_lowercase();
                apps.entry(norm_key).or_insert_with(|| AppInfo {
                    id: sanitize_id(&item.Name),
                    name: item.Name,
                    app_type: "uwp".to_string(),
                    target: item.AppID,
                    shortcut_path: None,
                    arguments: None,
                    working_dir: None,
                    icon_base64: None,
                });
            }
        }
    }
}

fn scan_dir_shortcuts(
    dir: &Path,
    sl: *mut std::ffi::c_void,
    pf: *mut std::ffi::c_void,
    with_icons: bool,
    apps: &mut HashMap<String, AppInfo>,
    is_start_menu: bool,
) {
    if !dir.exists() {
        return;
    }

    if let Ok(entries) = fs::read_dir(dir) {
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                scan_dir_shortcuts(&path, sl, pf, with_icons, apps, is_start_menu);
            } else if path
                .extension()
                .and_then(|e| e.to_str())
                .map(|e| e.eq_ignore_ascii_case("lnk"))
                .unwrap_or(false)
            {
                if let Some((target, args, work, icon_loc)) = parse_lnk_file(sl, pf, &path) {
                    let norm_key = target.to_lowercase();
                    if !is_start_menu && apps.contains_key(&norm_key) {
                        continue;
                    }

                    let file_stem = path.file_stem().and_then(|s| s.to_str()).unwrap_or("App");
                    let name = file_stem.to_string();
                    let id = sanitize_id(&name);

                    let icon_base64 = if with_icons {
                        let icon_target = if let Some(ref loc) = icon_loc {
                            let icon_file = loc.split(',').next().unwrap_or(&target);
                            if icon_file.is_empty() {
                                &target
                            } else {
                                icon_file
                            }
                        } else {
                            &target
                        };
                        extract_icon_base64_png(icon_target, 256)
                    } else {
                        None
                    };

                    apps.insert(
                        norm_key,
                        AppInfo {
                            id,
                            name,
                            app_type: "win32".to_string(),
                            target,
                            shortcut_path: Some(path.to_string_lossy().into_owned()),
                            arguments: args,
                            working_dir: work,
                            icon_base64,
                        },
                    );
                }
            }
        }
    }
}

unsafe extern "system" fn wnd_proc(hwnd: HWND, msg: u32, wparam: usize, lparam: isize) -> isize {
    match msg {
        WM_TRAYICON => {
            if lparam as u32 == WM_RBUTTONUP || lparam as u32 == WM_CONTEXTMENU {
                let mut pt = POINT { x: 0, y: 0 };
                GetCursorPos(&mut pt);

                let hmenu = CreatePopupMenu();
                let about_text = to_wide("关于 (&About)");
                let exit_text = to_wide("退出会话 (&Exit)");

                AppendMenuW(hmenu, MF_STRING, ID_TRAY_ABOUT, about_text.as_ptr());
                AppendMenuW(hmenu, MF_SEPARATOR, 0, ptr::null());
                AppendMenuW(hmenu, MF_STRING, ID_TRAY_EXIT, exit_text.as_ptr());

                SetForegroundWindow(hwnd);
                TrackPopupMenu(hmenu, TPM_RIGHTBUTTON, pt.x, pt.y, 0, hwnd, ptr::null());
                DestroyMenu(hmenu);
                return 0;
            }
        }
        WM_COMMAND => {
            let id = wparam & 0xFFFF;
            if id == ID_TRAY_EXIT {
                DestroyWindow(hwnd);
                return 0;
            } else if id == ID_TRAY_ABOUT {
                let text = to_wide(
                    "RemoteApp 守护保活进程\n用于在单个 RDP Session 中维持会话并拉起应用。",
                );
                let title = to_wide("RemoteApp Launcher");
                MessageBoxW(
                    hwnd,
                    text.as_ptr(),
                    title.as_ptr(),
                    MB_ICONINFORMATION | MB_OK,
                );
                return 0;
            }
        }
        WM_TIMER => {
            if wparam == TIMER_KEEPALIVE_ID {
                if RDP_KEEPALIVE_ACTIVE.load(Ordering::SeqCst) {
                    apply_execution_state(true);
                    keybd_event(0x87, 0, 0, 0); // VK_F24 down
                    keybd_event(0x87, 0, 2, 0); // VK_F24 up
                }
                return 0;
            }
        }
        WM_WTSSESSION_CHANGE => {
            match wparam as u32 {
                WTS_REMOTE_DISCONNECT if RDP_KEEPALIVE_ACTIVE.swap(false, Ordering::SeqCst) => {
                    apply_execution_state(false);
                    log::info!("RDP session disconnected; released execution-state keepalive");
                }
                WTS_REMOTE_CONNECT if !RDP_KEEPALIVE_ACTIVE.swap(true, Ordering::SeqCst) => {
                    apply_execution_state(true);
                    log::info!("RDP session connected; reapplied execution-state keepalive");
                }
                _ => {}
            }
            return 0;
        }
        WM_DESTROY => {
            WTSUnRegisterSessionNotification(hwnd);
            SetThreadExecutionState(ES_CONTINUOUS);
            RUNNING.store(false, Ordering::SeqCst);
            PostQuitMessage(0);
            return 0;
        }
        _ => {}
    }

    DefWindowProcW(hwnd, msg, wparam, lparam)
}

fn init_logger() -> Option<flexi_logger::LoggerHandle> {
    let temp_dir = std::env::var("RDP_LAUNCHER_LOG_DIR")
        .or_else(|_| std::env::var("TEMP"))
        .unwrap_or_else(|_| "C:\\Windows\\Temp".to_string());
    let basename = format!(
        "rdp-launcher-{}-{}",
        std::env::args()
            .nth(1)
            .unwrap_or_else(|| "interactive".into()),
        std::process::id()
    );
    match flexi_logger::Logger::try_with_str("info") {
        Ok(l) => {
            match l
                .log_to_file(
                    flexi_logger::FileSpec::default()
                        .directory(temp_dir)
                        .basename(basename)
                        .suppress_timestamp(),
                )
                .rotate(
                    flexi_logger::Criterion::Size(1024 * 1024),
                    flexi_logger::Naming::Numbers,
                    flexi_logger::Cleanup::KeepLogFiles(1),
                )
                .start()
            {
                Ok(h) => Some(h),
                Err(e) => {
                    eprintln!("flexi_logger start error: {}", e);
                    None
                }
            }
        }
        Err(e) => {
            eprintln!("flexi_logger try_with_str error: {}", e);
            None
        }
    }
}

fn parse_bind_addr(args: &[String], default_port: u16) -> Result<std::net::SocketAddr, String> {
    let mut i = 0;
    while i < args.len() {
        if args[i] == "--bind" {
            let val = args
                .get(i + 1)
                .ok_or_else(|| "--bind requires an address value".to_string())?;
            if let Ok(addr) = val.parse::<std::net::SocketAddr>() {
                return Ok(addr);
            }
            if let Ok(ip) = val.parse::<std::net::IpAddr>() {
                return Ok(std::net::SocketAddr::new(ip, default_port));
            }
            return Err(format!(
                "invalid --bind address '{}': expected an IP or IP:port",
                val
            ));
        }
        i += 1;
    }
    // Safe localhost default when no external bind flag is supplied
    Ok(std::net::SocketAddr::new(
        std::net::IpAddr::V4(std::net::Ipv4Addr::new(127, 0, 0, 1)),
        default_port,
    ))
}

fn start_daemon(args: &[String]) {
    let configuration = parse_bind_addr(args, DEFAULT_TCP_PORT).and_then(|bind_addr| {
        let path = auth::token_file_arg(args).map_err(|error| error.to_string())?;
        let token =
            auth::load_for_bind(bind_addr, path.as_deref()).map_err(|error| error.to_string())?;
        Ok((bind_addr, token.map(Arc::new)))
    });
    match configuration {
        Ok((bind_addr, token)) => run_daemon(bind_addr, token),
        Err(e) => {
            log::error!("Invalid daemon bind configuration: {}", e);
            eprintln!("{}", e);
            std::process::exit(2);
        }
    }
}

const DEFAULT_TCP_PORT: u16 = 49152;

#[derive(Serialize, Deserialize, Debug)]
#[serde(tag = "action")]
pub enum AgentRequest {
    #[serde(rename = "ping")]
    Ping,
    #[serde(rename = "status")]
    Status,
    #[serde(rename = "run")]
    Run {
        target: String,
        params: Option<String>,
        #[serde(default)]
        working_dir: Option<String>,
    },
    #[serde(rename = "open")]
    Open { file: String },
    #[serde(rename = "list_apps")]
    ListApps {
        #[serde(default)]
        with_icons: bool,
    },
    #[serde(rename = "get_mapped_drives")]
    GetMappedDrives,
    #[serde(rename = "quit")]
    Quit,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct AgentResponse {
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub session_id: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub session_active: Option<bool>,
    #[serde(default)]
    pub role: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub apps: Option<Vec<AppInfo>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub drives: Option<Vec<(String, String)>>,
}

impl AgentResponse {
    fn ok() -> Self {
        AgentResponse {
            status: "ok".to_string(),
            session_id: None,
            session_active: None,
            role: "interactive".to_string(),
            message: None,
            apps: None,
            drives: None,
        }
    }

    fn ok_with_message(message: &str) -> Self {
        AgentResponse {
            message: Some(message.to_string()),
            ..Self::ok()
        }
    }

    fn error(message: impl Into<String>) -> Self {
        AgentResponse {
            status: "error".to_string(),
            message: Some(message.into()),
            ..Self::ok()
        }
    }
}

enum AppCommand {
    Run {
        target: String,
        params: Option<String>,
        working_dir: Option<String>,
        reply: mpsc::Sender<AgentResponse>,
    },
    Open {
        file: String,
        reply: mpsc::Sender<AgentResponse>,
    },
    GetMappedDrives {
        reply: mpsc::Sender<AgentResponse>,
    },
    Quit {
        reply: mpsc::Sender<AgentResponse>,
    },
}

/// A queued application command together with the instant after which it must
/// no longer be executed.
struct PendingCommand {
    deadline: std::time::Instant,
    command: AppCommand,
}

fn reject_expired(command: AppCommand) {
    let reply = match command {
        AppCommand::Run { reply, .. } => reply,
        AppCommand::Open { reply, .. } => reply,
        AppCommand::GetMappedDrives { reply } => reply,
        AppCommand::Quit { reply } => reply,
    };
    let _ = reply.send(AgentResponse::error("command expired before execution"));
}

/// Serial dispatcher for application-affecting commands. run/open/quit keep a
/// single FIFO order; Ping and the slow list_apps scan run on their own handler
/// threads so they never block the accept loop or the serial side effects.
fn run_dispatcher(rx: mpsc::Receiver<PendingCommand>, hwnd: HWND) {
    let com_initialized =
        unsafe { CoInitializeEx(ptr::null(), COINIT_APARTMENTTHREADED as u32) } >= 0;

    while RUNNING.load(Ordering::SeqCst) {
        let pending = match rx.recv_timeout(std::time::Duration::from_millis(500)) {
            Ok(pending) => pending,
            Err(mpsc::RecvTimeoutError::Timeout) => continue,
            Err(mpsc::RecvTimeoutError::Disconnected) => break,
        };

        // A queued command past its deadline must never execute later, even if
        // its asker has already given up.
        if std::time::Instant::now() >= pending.deadline {
            reject_expired(pending.command);
            continue;
        }

        match pending.command {
            AppCommand::Run {
                target,
                params,
                working_dir,
                reply,
            } => {
                let response =
                    match launch_application(&target, params.as_deref(), working_dir.as_deref()) {
                        Ok(()) => AgentResponse::ok(),
                        Err(e) => AgentResponse::error(e),
                    };
                let _ = reply.send(response);
            }
            AppCommand::Open { file, reply } => {
                let response = match launch_application(&file, None, None) {
                    Ok(()) => AgentResponse::ok(),
                    Err(e) => AgentResponse::error(e),
                };
                let _ = reply.send(response);
            }
            AppCommand::GetMappedDrives { reply } => {
                let response = AgentResponse {
                    drives: Some(query_mapped_network_drives()),
                    ..AgentResponse::ok()
                };
                let _ = reply.send(response);
            }
            AppCommand::Quit { reply } => {
                let _ = reply.send(AgentResponse::ok_with_message("quitting"));
                unsafe {
                    PostMessageW(hwnd, WM_DESTROY, 0, 0);
                }
                break;
            }
        }
    }

    if com_initialized {
        unsafe {
            CoUninitialize();
        }
    }
}

fn read_request_frame(stream: &std::net::TcpStream) -> Result<String, String> {
    use std::io::Read;

    // Total frame deadline: a trickle of bytes that keeps satisfying the
    // per-read timeout must still be bounded, so recompute the remaining
    // budget before every read instead of trusting a single SO_RCVTIMEO.
    let deadline = std::time::Instant::now() + CLIENT_IO_TIMEOUT;
    let mut reader = stream
        .try_clone()
        .map_err(|e| format!("connection clone failed: {}", e))?;
    let mut buf: Vec<u8> = Vec::with_capacity(1024);
    let mut chunk = [0u8; 4096];
    let mut saw_newline = false;

    loop {
        let remaining = deadline.saturating_duration_since(std::time::Instant::now());
        if remaining < std::time::Duration::from_millis(1) {
            return Err("request read deadline exceeded".to_string());
        }
        reader
            .set_read_timeout(Some(remaining))
            .map_err(|e| format!("failed to set read timeout: {}", e))?;
        match reader.read(&mut chunk) {
            Ok(0) => break,
            Ok(n) => {
                let newline = chunk[..n].iter().position(|&b| b == b'\n');
                let take = newline.map(|pos| pos + 1).unwrap_or(n);
                buf.extend_from_slice(&chunk[..take]);
                if buf.len() as u64 > MAX_REQUEST_BYTES {
                    return Err(format!(
                        "request exceeds {} byte frame limit",
                        MAX_REQUEST_BYTES
                    ));
                }
                if newline.is_some() {
                    saw_newline = true;
                    break;
                }
            }
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e)
                if e.kind() == std::io::ErrorKind::WouldBlock
                    || e.kind() == std::io::ErrorKind::TimedOut =>
            {
                return Err("request read deadline exceeded".to_string());
            }
            Err(e) => return Err(format!("request read failed: {}", e)),
        }
    }

    if buf.is_empty() {
        return Err("empty request".to_string());
    }
    if !saw_newline {
        // The host always terminates requests with a newline. EOF mid-frame
        // must not cause a partial request to be executed.
        return Err("request frame was not newline-terminated before EOF".to_string());
    }
    String::from_utf8(buf).map_err(|_| "request is not valid UTF-8".to_string())
}

fn write_response(
    stream: &mut std::net::TcpStream,
    response: &AgentResponse,
    budget: std::time::Duration,
) -> Result<(), String> {
    use std::io::Write;

    let mut payload =
        serde_json::to_vec(response).map_err(|e| format!("response encode failed: {}", e))?;
    payload.push(b'\n');

    // Total write deadline so a slow reader cannot extend the response forever.
    let deadline = std::time::Instant::now() + budget;
    let mut written = 0usize;
    while written < payload.len() {
        let remaining = deadline.saturating_duration_since(std::time::Instant::now());
        if remaining < std::time::Duration::from_millis(1) {
            return Err("response write deadline exceeded".to_string());
        }
        stream
            .set_write_timeout(Some(remaining))
            .map_err(|e| format!("failed to set write timeout: {}", e))?;
        match stream.write(&payload[written..]) {
            Ok(0) => return Err("connection closed before response was written".to_string()),
            Ok(n) => written += n,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e)
                if e.kind() == std::io::ErrorKind::WouldBlock
                    || e.kind() == std::io::ErrorKind::TimedOut =>
            {
                return Err("response write deadline exceeded".to_string());
            }
            Err(e) => return Err(format!("response write failed: {}", e)),
        }
    }
    let _ = stream.flush();
    Ok(())
}

/// Best-effort busy reply that never blocks the accept loop.
fn try_write_response(stream: &mut std::net::TcpStream, response: &AgentResponse) {
    use std::io::Write;

    let mut payload = match serde_json::to_vec(response) {
        Ok(payload) => payload,
        Err(_) => return,
    };
    payload.push(b'\n');
    if stream.set_nonblocking(true).is_err() {
        return;
    }
    let _ = stream.write(&payload);
}

fn dispatch_and_wait(
    request: AgentRequest,
    tx: &mpsc::SyncSender<PendingCommand>,
) -> AgentResponse {
    if !RUNNING.load(Ordering::SeqCst) {
        return AgentResponse::error("agent is shutting down");
    }

    let budget = CLIENT_IO_TIMEOUT;
    let deadline = std::time::Instant::now() + budget;

    let (reply_tx, reply_rx) = mpsc::channel();
    let command = match request {
        AgentRequest::Run {
            target,
            params,
            working_dir,
        } => AppCommand::Run {
            target,
            params,
            working_dir,
            reply: reply_tx,
        },
        AgentRequest::Open { file } => AppCommand::Open {
            file,
            reply: reply_tx,
        },
        AgentRequest::GetMappedDrives => AppCommand::GetMappedDrives { reply: reply_tx },
        AgentRequest::Quit => AppCommand::Quit { reply: reply_tx },
        AgentRequest::Ping | AgentRequest::Status | AgentRequest::ListApps { .. } => {
            return AgentResponse::error("request is not serial-dispatched");
        }
    };

    // Never block a handler on a full or dead queue: a stalled dispatcher must
    // not be able to pin all eight handler slots on enqueue.
    match tx.try_send(PendingCommand { deadline, command }) {
        Ok(()) => {}
        Err(mpsc::TrySendError::Full(_)) => return AgentResponse::error("agent is busy"),
        Err(mpsc::TrySendError::Disconnected(_)) => {
            return AgentResponse::error("agent dispatcher is not available");
        }
    }

    match reply_rx.recv_timeout(budget) {
        Ok(response) => response,
        Err(mpsc::RecvTimeoutError::Timeout) => {
            AgentResponse::error("command is still executing; outcome unknown")
        }
        Err(mpsc::RecvTimeoutError::Disconnected) => {
            AgentResponse::error("agent dispatcher stopped before replying; outcome unknown")
        }
    }
}

fn handle_tcp_client(
    stream: &mut std::net::TcpStream,
    dispatch_tx: &mpsc::SyncSender<PendingCommand>,
    token: Option<&auth::Token>,
) {
    let line = match read_request_frame(stream) {
        Ok(line) => line,
        Err(e) => {
            if let Err(write_err) =
                write_response(stream, &AgentResponse::error(e), CLIENT_IO_TIMEOUT)
            {
                log::warn!("Failed to send error response: {}", write_err);
            }
            return;
        }
    };

    let mut request: serde_json::Value = match serde_json::from_str(line.trim()) {
        Ok(request) => request,
        Err(e) => {
            log::warn!("Rejected malformed TCP request: {}", e);
            if let Err(write_err) = write_response(
                stream,
                &AgentResponse::error(format!("Invalid request: {}", e)),
                CLIENT_IO_TIMEOUT,
            ) {
                log::warn!("Failed to send error response: {}", write_err);
            }
            return;
        }
    };
    let action = auth::action(&request).to_string();
    if !auth::authorize(&request, token) {
        log::warn!("Rejected unauthenticated TCP request (action: {})", action);
        if let Err(write_err) = write_response(
            stream,
            &AgentResponse::error("authentication required"),
            CLIENT_IO_TIMEOUT,
        ) {
            log::warn!("Failed to send error response: {}", write_err);
        }
        return;
    }
    auth::remove_token_field(&mut request);
    log::info!("Incoming TCP request (action: {})", action);

    let (response, write_budget) = match serde_json::from_value::<AgentRequest>(request) {
        Ok(AgentRequest::Ping) => (AgentResponse::ok_with_message("pong"), CLIENT_IO_TIMEOUT),
        Ok(AgentRequest::Status) => {
            let (session_id, session_active) = session::current_active();
            (
                AgentResponse {
                    session_id,
                    session_active,
                    ..AgentResponse::ok_with_message("interactive endpoint ready")
                },
                CLIENT_IO_TIMEOUT,
            )
        }
        Ok(AgentRequest::ListApps { with_icons }) => {
            if LIST_APPS_RUNNING
                .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                .is_err()
            {
                (
                    AgentResponse::error("list_apps scan already in progress"),
                    CLIENT_IO_TIMEOUT,
                )
            } else {
                let _scan_guard = ListAppsScanGuard;
                (
                    AgentResponse {
                        apps: Some(scan_shortcuts(with_icons)),
                        ..AgentResponse::ok()
                    },
                    LIST_APPS_WRITE_TIMEOUT,
                )
            }
        }
        Ok(request) => (dispatch_and_wait(request, dispatch_tx), CLIENT_IO_TIMEOUT),
        Err(e) => (
            AgentResponse::error(format!("Invalid request: {}", e)),
            CLIENT_IO_TIMEOUT,
        ),
    };

    log::info!("Outgoing TCP response (status: {})", response.status);
    if let Err(write_err) = write_response(stream, &response, write_budget) {
        log::warn!("Failed to send response: {}", write_err);
    }
}

fn run_daemon(bind_addr: std::net::SocketAddr, token: Option<Arc<auth::Token>>) {
    unsafe {
        let chwnd = GetConsoleWindow();
        if !chwnd.is_null() {
            ShowWindow(chwnd, 0); // SW_HIDE
        }
        let class_name = to_wide("RemoteAppLauncherClass");
        let window_name = to_wide("RemoteApp Launcher Daemon");

        let wc = WNDCLASSW {
            style: 0,
            lpfnWndProc: Some(wnd_proc),
            cbClsExtra: 0,
            cbWndExtra: 0,
            hInstance: GetModuleHandleW(ptr::null()),
            hIcon: LoadIconW(ptr::null_mut(), IDI_APPLICATION),
            hCursor: LoadCursorW(ptr::null_mut(), IDC_ARROW),
            hbrBackground: ptr::null_mut(),
            lpszMenuName: ptr::null(),
            lpszClassName: class_name.as_ptr(),
        };

        RegisterClassW(&wc);

        let hwnd = CreateWindowExW(
            0,
            class_name.as_ptr(),
            window_name.as_ptr(),
            0,
            0,
            0,
            0,
            0,
            -3_isize as HWND, // HWND_MESSAGE (message-only window)
            ptr::null_mut(),
            wc.hInstance,
            ptr::null_mut(),
        );

        if hwnd.is_null() {
            eprintln!("Failed to create daemon message window");
            return;
        }

        if WTSRegisterSessionNotification(hwnd, NOTIFY_FOR_THIS_SESSION) == 0 {
            log::warn!(
                "WTSRegisterSessionNotification failed (Win32 error {}); keeping RDP keepalive active",
                GetLastError()
            );
        }

        let mut nid: NOTIFYICONDATAW = std::mem::zeroed();
        nid.cbSize = std::mem::size_of::<NOTIFYICONDATAW>() as u32;
        nid.hWnd = hwnd;
        nid.uID = 1;
        nid.uFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP;
        nid.uCallbackMessage = WM_TRAYICON;
        nid.hIcon = LoadIconW(ptr::null_mut(), IDI_APPLICATION);

        let tip = to_wide("RemoteApp 保活中 (FreeRDP)");
        let len = tip.len().min(nid.szTip.len());
        nid.szTip[..len].copy_from_slice(&tip[..len]);

        Shell_NotifyIconW(NIM_ADD, &nid);

        SetThreadExecutionState(
            ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_AWAYMODE_REQUIRED | ES_DISPLAY_REQUIRED,
        );
        SetTimer(hwnd, TIMER_KEEPALIVE_ID, 60_000, None);

        // Serial application dispatcher; owns the raw HWND as an integer so it
        // can request shutdown without sharing window state across threads.
        let (dispatch_tx, dispatch_rx) = mpsc::sync_channel::<PendingCommand>(MAX_PENDING_COMMANDS);
        let dispatch_hwnd = hwnd as usize;
        std::thread::spawn(move || run_dispatcher(dispatch_rx, dispatch_hwnd as HWND));

        // The listener thread needs the window handle only to request shutdown
        // when the initial bind fails.
        let listener_hwnd = hwnd as usize;
        // TCP server listener thread bound strictly to the address commanded by the host launcher
        std::thread::spawn(move || {
            match std::net::TcpListener::bind(bind_addr) {
                Ok(listener) => {
                    log::info!("TCP Agent successfully listening on {}", bind_addr);
                    for stream in listener.incoming() {
                        if !RUNNING.load(Ordering::SeqCst) {
                            break;
                        }
                        let mut stream = match stream {
                            Ok(stream) => stream,
                            Err(e) => {
                                log::warn!("TCP accept failed: {}", e);
                                continue;
                            }
                        };

                        if ACTIVE_HANDLERS.fetch_add(1, Ordering::SeqCst) >= MAX_ACTIVE_HANDLERS {
                            ACTIVE_HANDLERS.fetch_sub(1, Ordering::SeqCst);
                            try_write_response(&mut stream, &AgentResponse::error("agent is busy"));
                            continue;
                        }

                        let dispatch_tx = dispatch_tx.clone();
                        let token = token.clone();
                        std::thread::spawn(move || {
                            let _guard = ActiveHandlerGuard;
                            handle_tcp_client(&mut stream, &dispatch_tx, token.as_deref());
                        });
                    }
                }
                Err(e) => {
                    log::error!(
                        "CRITICAL ERROR: Failed to bind TCP listener on {}: {}",
                        bind_addr,
                        e
                    );
                    eprintln!("Failed to bind TCP listener on {}: {}", bind_addr, e);
                    // A failed bind must not leave the daemon, keepalive and
                    // singleton mutex alive with no RPC endpoint. Ask the main
                    // window to close so the process exits and the mutex is
                    // relinquished for a later retry.
                    RUNNING.store(false, Ordering::SeqCst);
                    PostMessageW(listener_hwnd as HWND, WM_CLOSE, 0, 0);
                }
            }
        });

        let mut msg: MSG = std::mem::zeroed();
        while GetMessageW(&mut msg, ptr::null_mut(), 0, 0) > 0 {
            TranslateMessage(&msg);
            DispatchMessageW(&msg);
        }

        Shell_NotifyIconW(NIM_DELETE, &nid);
    }
}

/// A UWP/AUMID target is only treated as an app when it is path-free
/// (`Package!AppId`). A real path such as `C:\report!.xlsx` also contains `!`
/// and must go through the normal ShellExecute path instead.
fn is_path_free_aumid(target: &str) -> bool {
    target.contains('!') && !target.contains('/') && !target.contains('\\') && !target.contains(':')
}

fn launch_application(
    target: &str,
    params_opt: Option<&str>,
    working_dir_opt: Option<&str>,
) -> Result<(), String> {
    unsafe {
        let clean_target = target.trim().trim_matches('"');
        if is_path_free_aumid(clean_target) {
            if params_opt.map(|p| !p.trim().is_empty()).unwrap_or(false) {
                return Err(format!(
                    "unsupported: cannot pass file parameters to UWP app '{}'; explorer.exe cannot forward them",
                    clean_target
                ));
            }

            let explorer = to_wide("explorer.exe");
            let param = to_wide(&format!("shell:AppsFolder\\{}", clean_target));
            let open_verb = to_wide("open");

            let mut sei: SHELLEXECUTEINFOW = std::mem::zeroed();
            sei.cbSize = std::mem::size_of::<SHELLEXECUTEINFOW>() as u32;
            sei.fMask = SEE_MASK_DOENVSUBST | SEE_MASK_FLAG_NO_UI;
            sei.lpVerb = open_verb.as_ptr();
            sei.lpFile = explorer.as_ptr();
            sei.lpParameters = param.as_ptr();
            sei.nShow = SW_SHOWNORMAL;

            if ShellExecuteExW(&mut sei) == 0 {
                let code = GetLastError();
                return Err(format!(
                    "ShellExecuteExW failed to activate UWP app '{}' (Win32 error {})",
                    clean_target, code
                ));
            }
            return Ok(());
        }

        let wide_file = to_wide(clean_target);
        let wide_params = params_opt.map(to_wide);
        let open_verb = to_wide("open");

        // Compute working directory: explicit > parent directory > none
        let computed_dir = working_dir_opt.map(|s| s.to_string()).or_else(|| {
            let p = Path::new(clean_target);
            if p.is_file() {
                p.parent()
                    .and_then(|parent| parent.to_str())
                    .map(|s| s.to_string())
            } else {
                None
            }
        });
        let wide_dir = computed_dir.as_deref().map(to_wide);

        let mut sei: SHELLEXECUTEINFOW = std::mem::zeroed();
        sei.cbSize = std::mem::size_of::<SHELLEXECUTEINFOW>() as u32;
        sei.fMask = SEE_MASK_DOENVSUBST | SEE_MASK_FLAG_NO_UI;
        sei.lpVerb = open_verb.as_ptr();
        sei.lpFile = wide_file.as_ptr();
        if let Some(ref p) = wide_params {
            sei.lpParameters = p.as_ptr();
        }
        if let Some(ref d) = wide_dir {
            sei.lpDirectory = d.as_ptr();
        }
        sei.nShow = SW_SHOWNORMAL;

        if ShellExecuteExW(&mut sei) == 0 {
            let code = GetLastError();
            return Err(format!(
                "ShellExecuteExW failed to launch '{}' (Win32 error {})",
                clean_target, code
            ));
        }
        Ok(())
    }
}

fn resolve_target_and_params(args: &[String]) -> (String, Option<String>) {
    let raw_joined = args.join(" ");
    let clean_joined = raw_joined.trim().trim_matches('"');

    // 1. If the entire string is an existing file or directory on disk, it's a single target
    if Path::new(clean_joined).exists() {
        return (clean_joined.to_string(), None);
    }

    // 2. Try to find an existing executable prefix among arguments
    for i in (1..args.len()).rev() {
        let prefix = args[..i].join(" ");
        let clean_prefix = prefix.trim().trim_matches('"');
        if Path::new(clean_prefix).is_file() {
            let params = args[i..].join(" ");
            let clean_params = params.trim();
            return (
                clean_prefix.to_string(),
                if clean_params.is_empty() {
                    None
                } else {
                    Some(clean_params.to_string())
                },
            );
        }
    }

    // 3. If first argument ends with .exe, assume first argument is binary and rest are params
    if let Some(first) = args.first() {
        let clean_first = first.trim().trim_matches('"');
        if clean_first.to_lowercase().ends_with(".exe")
            || clean_first.to_lowercase().ends_with(".cmd")
            || clean_first.to_lowercase().ends_with(".bat")
        {
            let params = args[1..].join(" ");
            let clean_params = params.trim();
            return (
                clean_first.to_string(),
                if clean_params.is_empty() {
                    None
                } else {
                    Some(clean_params.to_string())
                },
            );
        }
    }

    // 4. Default fallback: treat entire string as target
    (clean_joined.to_string(), None)
}

fn send_tcp_request(args: &[String], req: &AgentRequest) -> Result<serde_json::Value, String> {
    use std::io::{BufRead, BufReader, Write};
    let addr = parse_bind_addr(args, DEFAULT_TCP_PORT)?;
    let token = auth::token_file_arg(args)
        .map_err(|e| e.to_string())?
        .map(|path| auth::Token::load(&path).map_err(|e| e.to_string()))
        .transpose()?;
    let mut stream = std::net::TcpStream::connect_timeout(&addr, std::time::Duration::from_secs(2))
        .map_err(|e| format!("Daemon not reachable: {}", e))?;
    stream
        .set_read_timeout(Some(std::time::Duration::from_secs(60)))
        .map_err(|e| format!("failed to set read timeout: {}", e))?;
    stream
        .set_write_timeout(Some(std::time::Duration::from_secs(5)))
        .map_err(|e| format!("failed to set write timeout: {}", e))?;
    let mut body = serde_json::to_value(req).map_err(|e| e.to_string())?;
    if let Some(token) = &token {
        if let serde_json::Value::Object(fields) = &mut body {
            fields.insert(
                "token".to_string(),
                serde_json::Value::String(token.value().to_string()),
            );
        }
    }
    let mut json = serde_json::to_string(&body).map_err(|e| e.to_string())?;
    json.push('\n');
    stream
        .write_all(json.as_bytes())
        .map_err(|e| e.to_string())?;
    stream.flush().map_err(|e| e.to_string())?;

    let mut reader = BufReader::new(stream);
    let mut line = String::new();
    reader.read_line(&mut line).map_err(|e| e.to_string())?;
    let response: serde_json::Value = serde_json::from_str(&line).map_err(|e| e.to_string())?;
    if response["status"] != "ok" {
        return Err(response["message"]
            .as_str()
            .unwrap_or("agent returned an error")
            .to_string());
    }
    Ok(response)
}

fn main() {
    unsafe {
        AttachConsole(0xFFFFFFFF);
        SetConsoleOutputCP(65001);
    }
    let args: Vec<String> = std::env::args().collect();
    let subcommand = args.get(1).map(|s| s.as_str()).unwrap_or("daemon");

    if let Some(index) = args.iter().position(|arg| arg == "--log-dir") {
        let directory = args.get(index + 1).unwrap_or_else(|| {
            eprintln!("--log-dir requires a directory");
            std::process::exit(2)
        });
        if let Err(error) = fs::create_dir_all(directory) {
            eprintln!("{error}");
            std::process::exit(2);
        }
        std::env::set_var("RDP_LAUNCHER_LOG_DIR", directory);
    }
    let _logger = init_logger();
    if subcommand == "session-probe" {
        let result = args
            .get(2)
            .ok_or_else(|| std::io::Error::other("missing probe output path"))
            .and_then(|path| session::write_probe(std::path::Path::new(path)));
        if let Err(error) = result {
            eprintln!("{error}");
            std::process::exit(1);
        }
        return;
    }

    // Do not log the full argument vector: it can contain operator paths.
    log::info!("Process started: subcommand='{}'", subcommand);

    // Enforce strict singleton for the daemon instance in this session
    if matches!(subcommand, "daemon" | "interactive") {
        unsafe {
            let bind = parse_bind_addr(&args, DEFAULT_TCP_PORT).unwrap_or_else(|error| {
                eprintln!("{error}");
                std::process::exit(2)
            });
            let mutex_name = to_wide(&format!("Local\\RdpLauncher_Interactive_{bind}"));
            let h_mutex = CreateMutexW(ptr::null_mut(), FALSE, mutex_name.as_ptr());
            if h_mutex.is_null() || GetLastError() == ERROR_ALREADY_EXISTS {
                log::info!(
                    "Singleton mutex check: another daemon instance is already active. Exiting."
                );
                std::process::exit(0);
            }
        }
    }

    match subcommand {
        "list-apps" => {
            let with_icons = args.iter().any(|a| a == "--with-icons");
            let apps = scan_shortcuts(with_icons);
            if let Ok(json) = serde_json::to_string_pretty(&apps) {
                print_to_stdout(&json);
                print_to_stdout("\r\n");
            }
        }
        "check-ssh" => {
            if let Some(count) = ssh::count() {
                print_to_stdout(&format!("Active SSH connections: {}\r\n", count));
            } else {
                print_to_stdout("Failed to query active SSH connections\r\n");
            }
            std::process::exit(0);
        }
        "ping" | "status" | "apps" | "mounts" | "open" | "run" | "stop" | "quit" => {
            let mut positional = Vec::new();
            let mut i = 2;
            while i < args.len() {
                if args[i] == "--bind" || args[i] == "--log-dir" || args[i] == "--auth-token-file" {
                    i += 2;
                } else {
                    positional.push(args[i].clone());
                    i += 1;
                }
            }
            let request = match subcommand {
                "ping" => AgentRequest::Ping,
                "status" => AgentRequest::Status,
                "apps" => AgentRequest::ListApps { with_icons: false },
                "mounts" => AgentRequest::GetMappedDrives,
                "stop" | "quit" => AgentRequest::Quit,
                "open" if positional.len() == 1 => AgentRequest::Open {
                    file: positional[0].clone(),
                },
                "run" if !positional.is_empty() => {
                    let (target, params) = resolve_target_and_params(&positional);
                    AgentRequest::Run {
                        target,
                        params,
                        working_dir: None,
                    }
                }
                _ => {
                    eprintln!("open requires one file; run requires a target");
                    std::process::exit(2);
                }
            };
            match send_tcp_request(&args, &request) {
                Ok(response) => {
                    if subcommand == "ping" {
                        print_to_stdout("PONG\r\n");
                    } else {
                        print_to_stdout(&format!("{}\r\n", response));
                    }
                }
                Err(error) => {
                    eprintln!("{error}");
                    std::process::exit(1);
                }
            }
        }
        "help" | "--help" | "-h" => {
            print_to_stdout("remoteapp-launcher backend|interactive [--bind IP:PORT] [--log-dir DIR] [--auth-token-file PATH]\r\nremoteapp-launcher ping|status|apps|mounts|quit [--bind IP:PORT] [--auth-token-file PATH]\r\nremoteapp-launcher run TARGET [ARGS...] [--bind IP:PORT] [--auth-token-file PATH]\r\nremoteapp-launcher open FILE [--bind IP:PORT] [--auth-token-file PATH]\r\nremoteapp-launcher list-apps [--with-icons] | check-ssh\r\n");
        }
        "backend" => {
            let result = parse_bind_addr(&args, 49153)
                .map_err(std::io::Error::other)
                .and_then(|addr| backend::run(addr, &args));
            if let Err(error) = result {
                log::error!("Background stopped: {error}");
                eprintln!("{error}");
                std::process::exit(1);
            }
        }
        "daemon" | "interactive" => start_daemon(&args),
        _ => {
            eprintln!("Unknown command: {subcommand}");
            std::process::exit(2);
        }
    }
}

fn print_to_stdout(s: &str) {
    unsafe {
        let stdout = GetStdHandle(STD_OUTPUT_HANDLE);
        if !stdout.is_null() && stdout != INVALID_HANDLE_VALUE {
            let bytes = s.as_bytes();
            let mut written = 0u32;
            WriteFile(
                stdout,
                bytes.as_ptr(),
                bytes.len() as u32,
                &mut written,
                ptr::null_mut(),
            );
        }
    }
}
