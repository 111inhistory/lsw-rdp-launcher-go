//! Runs only in the selected user's WinSta0 desktop. Unknown tray state stays
//! null: RemoteApp shells and unsupported shell versions must never look empty.
use crate::protocol::Ui;
use std::{
    collections::HashSet,
    ptr,
    sync::{
        atomic::{AtomicU32, Ordering},
        Mutex,
    },
};
use windows_sys::{
    core::{GUID, HRESULT},
    Win32::{
        Foundation::*,
        System::{Com::*, Threading::*},
        UI::WindowsAndMessaging::*,
    },
};

pub fn observe() -> Ui {
    unsafe {
        let mut count = 0u32;
        let ok = EnumWindows(Some(window), (&mut count as *mut u32) as isize) != 0;
        Ui {
            windows: if ok { Some(count) } else { None },
            tray_icons: tray_icons(),
        }
    }
}
unsafe extern "system" fn window(hwnd: HWND, data: LPARAM) -> BOOL {
    if IsWindowVisible(hwnd) == 0 || hwnd == GetShellWindow() || hwnd == GetDesktopWindow() {
        return 1;
    }
    let mut class = [0u16; 256];
    let n = GetClassNameW(hwnd, class.as_mut_ptr(), class.len() as i32);
    let class = String::from_utf16_lossy(&class[..n.max(0) as usize]);
    if matches!(
        class.as_str(),
        "Shell_TrayWnd"
            | "Shell_SecondaryTrayWnd"
            | "Progman"
            | "WorkerW"
            | "RemoteAppLauncherClass"
    ) {
        return 1;
    }
    let mut pid = 0;
    GetWindowThreadProcessId(hwnd, &mut pid);
    if pid == GetCurrentProcessId() || own_agent(pid) {
        return 1;
    }
    // Owned tool windows still count: hiding a main window does not make its
    // dialogs or other user-visible work safe to terminate.
    *(data as *mut u32) += 1;
    1
}
unsafe fn image(pid: u32) -> Option<String> {
    let h = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
    if h.is_null() {
        return None;
    }
    let mut buf = [0u16; 32768];
    let mut n = buf.len() as u32;
    let ok = QueryFullProcessImageNameW(h, 0, buf.as_mut_ptr(), &mut n);
    CloseHandle(h);
    if ok == 0 {
        None
    } else {
        Some(String::from_utf16_lossy(&buf[..n as usize]))
    }
}
unsafe fn own_agent(pid: u32) -> bool {
    match (image(pid), std::env::current_exe().ok()) {
        (Some(actual), Some(ours)) => actual.eq_ignore_ascii_case(&ours.to_string_lossy()),
        _ => false,
    }
}
// WORKAROUND: Explorer exposes this private Win8+ notification interface, not
// a supported complete tray snapshot API. Only the known prefix
// of NOTIFYITEM is read, and no preferences or remote process memory are changed.
// Shell capability absence is deliberately not treated as an empty tray.
// Replace this adapter if a supported snapshot API becomes available. A
// successful registration reports the icons it observed, including zero.
const CLSID_TRAY: GUID = GUID::from_u128(0x25dead04_1eac_4911_9e3a_ad0a4ab560fd);
const IID_TRAY: GUID = GUID::from_u128(0xd133ce13_3537_48ba_93a7_afcd5d2053b4);
const IID_CB: GUID = GUID::from_u128(0xd782ccba_afb0_43f1_94db_fda3779eaccb);
const IID_UNKNOWN: GUID = GUID::from_u128(0x00000000_0000_0000_c000_000000000046);
#[repr(C)]
struct Item {
    exe: *const u16,
    tip: *const u16,
    icon: HICON,
    hwnd: HWND,
    preference: u32,
    id: u32,
}
#[repr(C)]
struct Tray {
    vtable: *const TrayVtable,
}
#[repr(C)]
struct TrayVtable {
    query:
        unsafe extern "system" fn(*mut Tray, *const GUID, *mut *mut core::ffi::c_void) -> HRESULT,
    add: unsafe extern "system" fn(*mut Tray) -> u32,
    release: unsafe extern "system" fn(*mut Tray) -> u32,
    register: unsafe extern "system" fn(*mut Tray, *mut Callback, *mut u32) -> HRESULT,
    unregister: unsafe extern "system" fn(*mut Tray, *mut u32) -> HRESULT,
}
#[repr(C)]
struct Callback {
    vtable: *const CallbackVtable,
    refs: AtomicU32,
    icons: Mutex<HashSet<(u32, isize, u32)>>,
}
#[repr(C)]
struct CallbackVtable {
    query: unsafe extern "system" fn(
        *mut Callback,
        *const GUID,
        *mut *mut core::ffi::c_void,
    ) -> HRESULT,
    add: unsafe extern "system" fn(*mut Callback) -> u32,
    release: unsafe extern "system" fn(*mut Callback) -> u32,
    notify: unsafe extern "system" fn(*mut Callback, u32, *const Item) -> HRESULT,
}
unsafe extern "system" fn query(
    this: *mut Callback,
    iid: *const GUID,
    out: *mut *mut core::ffi::c_void,
) -> HRESULT {
    if out.is_null() || iid.is_null() {
        return 0x80004003u32 as i32;
    }
    *out = ptr::null_mut();
    let same = |other: &GUID| {
        (*iid).data1 == other.data1
            && (*iid).data2 == other.data2
            && (*iid).data3 == other.data3
            && (*iid).data4 == other.data4
    };
    if same(&IID_CB) || same(&IID_UNKNOWN) {
        *out = this.cast();
        add(this);
        0
    } else {
        0x80004002u32 as i32
    }
}
unsafe extern "system" fn add(this: *mut Callback) -> u32 {
    (*this).refs.fetch_add(1, Ordering::Relaxed) + 1
}
unsafe extern "system" fn release(this: *mut Callback) -> u32 {
    let n = (*this).refs.fetch_sub(1, Ordering::AcqRel) - 1;
    if n == 0 {
        drop(Box::from_raw(this))
    }
    n
}
unsafe extern "system" fn notify(this: *mut Callback, event: u32, item: *const Item) -> HRESULT {
    if item.is_null() {
        return 0;
    }
    let item = &*item;
    let mut pid = 0;
    GetWindowThreadProcessId(item.hwnd, &mut pid);
    let key = (pid, item.hwnd as isize, item.id);
    if let Ok(mut icons) = (*this).icons.lock() {
        if event == 2 {
            icons.remove(&key);
        } else if IsWindow(item.hwnd) != 0 && !own_agent(pid) {
            // Built-in shell indicators are not user-launched notification icons.
            if !image(pid)
                .map(|p| p.to_ascii_lowercase().ends_with("\\explorer.exe"))
                .unwrap_or(false)
            {
                icons.insert(key);
            }
        }
    }
    0
}
static CALLBACK: CallbackVtable = CallbackVtable {
    query,
    add,
    release,
    notify,
};
unsafe fn tray_icons() -> Option<u32> {
    let name: Vec<u16> = "Shell_TrayWnd\0".encode_utf16().collect();
    if FindWindowW(name.as_ptr(), ptr::null()).is_null() {
        return None;
    }
    let initialized = CoInitializeEx(ptr::null(), COINIT_APARTMENTTHREADED as u32);
    if initialized < 0 {
        return None;
    }
    let mut tray: *mut Tray = ptr::null_mut();
    let hr = CoCreateInstance(
        &CLSID_TRAY,
        ptr::null_mut(),
        CLSCTX_LOCAL_SERVER,
        &IID_TRAY,
        (&mut tray as *mut *mut Tray).cast(),
    );
    if hr < 0 || tray.is_null() {
        CoUninitialize();
        return None;
    }
    let cb = Box::into_raw(Box::new(Callback {
        vtable: &CALLBACK,
        refs: AtomicU32::new(1),
        icons: Mutex::new(HashSet::new()),
    }));
    let mut cookie = 0;
    let registered = ((*(*tray).vtable).register)(tray, cb, &mut cookie);
    let result = if registered >= 0 {
        let unregistered = ((*(*tray).vtable).unregister)(tray, &mut cookie);
        if unregistered >= 0 {
            // Registration succeeded, so the callbacks seen while it was held
            // are the observation; an empty set is a real zero. UI counts are
            // activity observations, never authorization for logoff.
            (*cb).icons.lock().ok().map(|icons| icons.len() as u32)
        } else {
            None
        }
    } else {
        None
    };
    release(cb);
    ((*(*tray).vtable).release)(tray);
    CoUninitialize();
    result
}
