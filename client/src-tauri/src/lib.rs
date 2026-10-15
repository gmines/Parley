mod api;
mod commands;
mod config;
mod error;
mod realtime;
mod secrets;
mod session;
mod voice;
pub mod voice_engine;
mod window;

use tauri::Manager;
use tauri_plugin_log::{RotationStrategy, Target, TargetKind, TimezoneStrategy};

pub fn run() {
    tauri::Builder::default()
        // Launching Parley again shows the running one (its window may be
        // hidden in the background). Must be the first plugin.
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| window::show_main(app)))
        // Logs go to the OS log folder (About → Open logs folder), so testers
        // can send them with a bug report.
        .plugin(
            tauri_plugin_log::Builder::new()
                .targets([
                    Target::new(TargetKind::Stdout),
                    Target::new(TargetKind::LogDir { file_name: Some("parley".into()) }),
                ])
                .level(log::LevelFilter::Info)
                .level_for("tao", log::LevelFilter::Warn)
                .level_for("hyper_util", log::LevelFilter::Warn)
                .level_for("reqwest", log::LevelFilter::Warn)
                .level_for("tungstenite", log::LevelFilter::Warn)
                .level_for("tokio_tungstenite", log::LevelFilter::Warn)
                .max_file_size(2_000_000)
                .rotation_strategy(RotationStrategy::KeepOne)
                .timezone_strategy(TimezoneStrategy::UseLocal)
                .build(),
        )
        .plugin(tauri_plugin_opener::init())
        .setup(|app| {
            log::info!(
                "Parley {} ({}) on {} {}",
                env!("CARGO_PKG_VERSION"),
                env!("PARLEY_COMMIT"),
                std::env::consts::OS,
                std::env::consts::ARCH
            );
            let config_path = app.path().app_config_dir()?.join("config.json");
            app.manage(commands::AppState {
                session: session::SessionManager::new(Box::new(secrets::Keyring)),
                config: config::ConfigStore::new(config_path),
                realtime: realtime::Realtime::default(),
                voice: voice::Voice::default(),
            });
            #[cfg(not(target_os = "macos"))]
            window::setup_tray(app);
            Ok(())
        })
        .on_window_event(window::on_window_event)
        .invoke_handler(tauri::generate_handler![
            commands::saved_server,
            commands::saved_login,
            commands::restore_session,
            commands::register,
            commands::login,
            commands::logout,
            commands::current_user,
            commands::server_features,
            commands::request_password_reset,
            commands::confirm_password_reset,
            commands::fetch_me,
            commands::change_password,
            commands::set_contact,
            commands::verify_contact,
            commands::remove_contact,
            commands::list_friends,
            commands::remove_friend,
            commands::list_requests,
            commands::send_friend_request,
            commands::accept_friend_request,
            commands::delete_friend_request,
            commands::list_blocked,
            commands::block_user,
            commands::unblock_user,
            commands::realtime_start,
            voice::voice_join,
            voice::voice_leave,
            voice::voice_set_mute,
            voice::voice_tone,
            voice::voice_audio_test,
            voice::voice_mic_level,
            voice::voice_set_deaf,
            voice::voice_status,
            voice::voice_stats,
            voice::voice_devices,
            voice::voice_set_devices,
            voice::voice_decline,
            voice::voice_calls,
            commands::app_info,
            commands::server_meta,
            commands::open_logs,
            window::background_setting,
            window::set_background,
            commands::list_conversations,
            commands::get_conversation,
            commands::open_dm,
            commands::create_group,
            commands::add_group_members,
            commands::leave_group,
            commands::list_messages,
            commands::send_message,
            commands::mark_read,
        ])
        .build(tauri::generate_context!())
        .expect("error while building Parley")
        .run(|_app, event| match event {
            tauri::RunEvent::Exit => voice::shutdown(),
            // Dock icon clicked while the window is hidden.
            #[cfg(target_os = "macos")]
            tauri::RunEvent::Reopen { has_visible_windows: false, .. } => window::show_main(_app),
            _ => {}
        });
}
