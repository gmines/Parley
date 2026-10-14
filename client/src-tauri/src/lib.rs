mod api;
mod commands;
mod config;
mod error;
mod realtime;
mod secrets;
mod session;
mod voice;
pub mod voice_engine;

use tauri::Manager;

pub fn run() {
    tauri::Builder::default()
        .setup(|app| {
            let config_path = app.path().app_config_dir()?.join("config.json");
            app.manage(commands::AppState {
                session: session::SessionManager::new(Box::new(secrets::Keyring)),
                config: config::ConfigStore::new(config_path),
                realtime: realtime::Realtime::default(),
                voice: voice::Voice::default(),
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            commands::saved_server,
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
            voice::voice_set_deaf,
            voice::voice_status,
            voice::voice_stats,
            voice::voice_devices,
            voice::voice_set_devices,
            voice::voice_decline,
            voice::voice_calls,
            commands::app_info,
            commands::server_meta,
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
        .run(|_app, event| {
            if let tauri::RunEvent::Exit = event {
                voice::shutdown();
            }
        });
}
