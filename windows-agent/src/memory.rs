//! Sample physical memory once per second; notify only on changes.
//! Thresholds and resizing belong to the host.

use serde::Serialize;
use windows_sys::Win32::System::SystemInformation::{GlobalMemoryStatusEx, MEMORYSTATUSEX};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
pub struct Usage {
    pub total_bytes: u64,
    pub available_bytes: u64,
}

pub fn read() -> Option<Usage> {
    let mut status: MEMORYSTATUSEX = unsafe { std::mem::zeroed() };
    status.dwLength = std::mem::size_of::<MEMORYSTATUSEX>() as u32;
    if unsafe { GlobalMemoryStatusEx(&mut status) } == 0
        || status.ullTotalPhys == 0
        || status.ullAvailPhys > status.ullTotalPhys
    {
        return None;
    }
    Some(Usage {
        total_bytes: status.ullTotalPhys,
        available_bytes: status.ullAvailPhys,
    })
}

pub fn changed(previous: Option<Usage>, current: Option<Usage>) -> bool {
    match (previous, current) {
        (Some(a), Some(b)) => {
            a.total_bytes != b.total_bytes || percent_bounds(a) != percent_bounds(b)
        }
        (None, None) => false,
        _ => true,
    }
}

// Floor detects landing on an integer from below; ceil detects it from above.
// These bounds do not contain or duplicate the host's thresholds.
fn percent_bounds(usage: Usage) -> (u64, u64) {
    let scaled = (usage.total_bytes - usage.available_bytes) * 100;
    (
        scaled / usage.total_bytes,
        scaled.div_ceil(usage.total_bytes),
    )
}
