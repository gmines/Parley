//! The main window's lifecycle. Closing the window hides it and Parley keeps
//! running, so calls and messages still arrive (unless the user turned
//! "Keep Parley running" off). It comes back from the Dock (macOS), the tray
//! icon (Windows, Linux), launching Parley again, or an incoming call.
//! Quitting is Parley → Quit (⌘Q) on macOS and the tray's "Quit Parley"
//! elsewhere.

use std::sync::atomic::{AtomicBool, Ordering};

use serde::Serialize;
use tauri::{AppHandle, Manager, State, Window, WindowEvent};

use crate::commands::AppState;

/// Whether a hidden window can be brought back: always on macOS (the Dock),
/// elsewhere only once the tray icon exists.
static CAN_HIDE: AtomicBool = AtomicBool::new(cfg!(target_os = "macos"));

pub fn show_main(app: &AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.unminimize();
        let _ = w.show();
        let _ = w.set_focus();
    }
}

/// An incoming call: bring a hidden or minimized window back so it can ring,
/// without taking focus from what the user is typing in.
pub fn incoming_call(app: &AppHandle) {
    let Some(w) = app.get_webview_window("main") else { return };
    let hidden = !w.is_visible().unwrap_or(true) || w.is_minimized().unwrap_or(false);
    if hidden {
        let _ = w.unminimize();
        let _ = w.show();
    }
    let _ = w.request_user_attention(Some(tauri::UserAttentionType::Critical));
}

/// Brings the window back and asks for attention, like an incoming call:
/// about to leave a call the user seems to have walked away from.
#[tauri::command]
pub fn request_attention(app: AppHandle) {
    incoming_call(&app);
}

fn keep_running(app: &AppHandle) -> bool {
    CAN_HIDE.load(Ordering::Relaxed) && app.state::<AppState>().config.load().run_in_background.unwrap_or(true)
}

pub fn on_window_event(window: &Window, event: &WindowEvent) {
    if let WindowEvent::CloseRequested { api, .. } = event {
        if window.label() == "main" && keep_running(window.app_handle()) {
            api.prevent_close();
            let _ = window.hide();
        }
    }
}

/// Adds the tray icon (Windows, Linux). On Linux this needs an AppIndicator
/// library; without one the app still runs, and closing the window quits.
#[cfg(not(target_os = "macos"))]
pub fn setup_tray(app: &tauri::App) {
    use tauri::menu::{Menu, MenuItem};
    use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};

    let build = || -> tauri::Result<()> {
        let open = MenuItem::with_id(app, "open", "Open Parley", true, None::<&str>)?;
        let quit = MenuItem::with_id(app, "quit", "Quit Parley", true, None::<&str>)?;
        let mut tray = TrayIconBuilder::with_id("main")
            .tooltip("Parley")
            .menu(&Menu::with_items(app, &[&open, &quit])?)
            .show_menu_on_left_click(false)
            .on_menu_event(|app, e| match e.id.as_ref() {
                "open" => show_main(app),
                "quit" => app.exit(0),
                _ => {}
            })
            .on_tray_icon_event(|tray, e| {
                if let TrayIconEvent::Click { button: MouseButton::Left, button_state: MouseButtonState::Up, .. } = e {
                    show_main(tray.app_handle());
                }
            });
        if let Some(icon) = app.default_window_icon() {
            tray = tray.icon(icon.clone());
        }
        tray.build(app)?;
        Ok(())
    };
    // The Linux tray loads libayatana-appindicator at runtime and panics if
    // it isn't installed.
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(build)) {
        Ok(Ok(())) => CAN_HIDE.store(true, Ordering::Relaxed),
        Ok(Err(e)) => log::warn!("no tray icon, closing the window will quit: {e}"),
        Err(_) => log::warn!("no tray icon (AppIndicator library missing?), closing the window will quit"),
    }
}

#[derive(Serialize)]
pub struct BackgroundSetting {
    enabled: bool,
    /// False when there's no way back to a hidden window (no tray icon).
    available: bool,
}

#[tauri::command]
pub fn background_setting(state: State<'_, AppState>) -> BackgroundSetting {
    BackgroundSetting {
        enabled: state.config.load().run_in_background.unwrap_or(true),
        available: CAN_HIDE.load(Ordering::Relaxed),
    }
}

#[tauri::command]
pub fn set_background(state: State<'_, AppState>, enabled: bool) {
    state.config.update(|c| c.run_in_background = Some(enabled));
}
