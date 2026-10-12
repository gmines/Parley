mod api;
mod commands;
mod config;
mod error;
mod secrets;
mod session;

use tauri::Manager;

pub fn run() {
    tauri::Builder::default()
        .setup(|app| {
            let config_path = app.path().app_config_dir()?.join("config.json");
            app.manage(commands::AppState {
                session: session::SessionManager::new(Box::new(secrets::Keyring)),
                config: config::ConfigStore::new(config_path),
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
        ])
        .run(tauri::generate_context!())
        .expect("error while running Parley");
}
