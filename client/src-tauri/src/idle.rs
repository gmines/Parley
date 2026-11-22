//! How long since the user last touched the computer, for leaving calls
//! they've walked away from (#15). Each OS answers differently:
//!
//! - macOS and Windows: time since the last keyboard or mouse input.
//! - Linux on GNOME: the same, from Mutter's idle monitor.
//! - Other Linux desktops (KDE on Wayland has no idle-time API): only
//!   whether the screen is locked or the screensaver is on.

use serde::Serialize;

#[derive(Debug, Default, Serialize)]
pub struct Idle {
    /// Seconds since the last input, when the OS tells.
    seconds: Option<u64>,
    /// The screen is locked or the screensaver is running.
    locked: bool,
}

#[tauri::command]
pub async fn system_idle() -> Idle {
    tauri::async_runtime::spawn_blocking(query).await.unwrap_or_default()
}

#[cfg(target_os = "macos")]
fn query() -> Idle {
    #[link(name = "CoreGraphics", kind = "framework")]
    extern "C" {
        fn CGEventSourceSecondsSinceLastEventType(source: i32, event_type: u32) -> f64;
    }
    // kCGEventSourceStateCombinedSessionState, kCGAnyInputEventType
    let secs = unsafe { CGEventSourceSecondsSinceLastEventType(0, u32::MAX) };
    Idle { seconds: secs.is_finite().then(|| secs.max(0.0) as u64), locked: false }
}

#[cfg(target_os = "windows")]
fn query() -> Idle {
    #[repr(C)]
    struct LastInputInfo {
        cb_size: u32,
        dw_time: u32,
    }
    #[link(name = "user32")]
    extern "system" {
        fn GetLastInputInfo(plii: *mut LastInputInfo) -> i32;
    }
    #[link(name = "kernel32")]
    extern "system" {
        fn GetTickCount() -> u32;
    }
    let mut info = LastInputInfo { cb_size: std::mem::size_of::<LastInputInfo>() as u32, dw_time: 0 };
    if unsafe { GetLastInputInfo(&mut info) } == 0 {
        return Idle::default();
    }
    // Both are milliseconds since boot, wrapping every 49.7 days.
    let ms = unsafe { GetTickCount() }.wrapping_sub(info.dw_time);
    Idle { seconds: Some(u64::from(ms) / 1000), locked: false }
}

#[cfg(target_os = "linux")]
fn query() -> Idle {
    use dbus::blocking::Connection;
    use std::time::Duration;

    let Ok(conn) = Connection::new_session() else { return Idle::default() };
    let timeout = Duration::from_millis(500);
    let seconds = conn
        .with_proxy("org.gnome.Mutter.IdleMonitor", "/org/gnome/Mutter/IdleMonitor/Core", timeout)
        .method_call("org.gnome.Mutter.IdleMonitor", "GetIdletime", ())
        .ok()
        .map(|(ms,): (u64,)| ms / 1000);
    let locked = conn
        .with_proxy("org.freedesktop.ScreenSaver", "/org/freedesktop/ScreenSaver", timeout)
        .method_call("org.freedesktop.ScreenSaver", "GetActive", ())
        .map(|(active,): (bool,)| active)
        .unwrap_or(false);
    Idle { seconds, locked }
}

#[cfg(not(any(target_os = "macos", target_os = "windows", target_os = "linux")))]
fn query() -> Idle {
    Idle::default()
}
