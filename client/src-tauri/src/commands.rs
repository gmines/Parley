//! Tauri commands invoked by the UI (see src/api.ts for the TypeScript side).

use reqwest::Method;
use serde_json::json;
use tauri::State;

use crate::api::{Blocked, BlockedList, Features, Friend, FriendsList, Requests, SendResult, User};
use crate::config::ConfigStore;
use crate::error::{AppError, Result};
use crate::session::{normalize_server, Registration, SessionManager};

pub struct AppState {
    pub session: SessionManager,
    pub config: ConfigStore,
}

/// Path segments come from the UI, so only accept UUID-shaped IDs.
fn id(s: &str) -> Result<&str> {
    let ok = s.len() == 36 && s.chars().all(|c| c.is_ascii_hexdigit() || c == '-');
    ok.then_some(s).ok_or_else(|| AppError::Internal("invalid id".into()))
}

#[tauri::command]
pub fn saved_server(state: State<'_, AppState>) -> Option<String> {
    state.config.load().server_url
}

#[tauri::command]
pub async fn restore_session(state: State<'_, AppState>) -> Result<Option<User>> {
    let Some(server) = state.config.load().server_url else {
        return Ok(None);
    };
    state.session.restore(&normalize_server(&server)?).await
}

#[tauri::command]
#[allow(clippy::too_many_arguments)]
pub async fn register(
    state: State<'_, AppState>,
    server: String,
    username: String,
    password: String,
    display_name: String,
    email: Option<String>,
    phone: Option<String>,
) -> Result<User> {
    let server = normalize_server(&server)?;
    let reg = Registration {
        username: &username,
        password: &password,
        display_name: &display_name,
        email: email.as_deref().unwrap_or(""),
        phone: phone.as_deref().unwrap_or(""),
    };
    let user = state.session.register(&server, &reg).await?;
    state.config.update(|c| c.server_url = Some(server.to_string()));
    Ok(user)
}

#[tauri::command]
pub async fn login(state: State<'_, AppState>, server: String, username: String, password: String) -> Result<User> {
    let server = normalize_server(&server)?;
    let user = state.session.login(&server, &username, &password).await?;
    state.config.update(|c| c.server_url = Some(server.to_string()));
    Ok(user)
}

#[tauri::command]
pub async fn logout(state: State<'_, AppState>) -> Result<()> {
    state.session.logout().await;
    Ok(())
}

#[tauri::command]
pub async fn current_user(state: State<'_, AppState>) -> Result<Option<User>> {
    Ok(state.session.current_user().await)
}

/// "email" or "phone" from the UI, as used in /v1/users/me/{contact}.
fn contact(kind: &str) -> Result<&str> {
    matches!(kind, "email" | "phone").then_some(kind).ok_or_else(|| AppError::Internal("invalid contact kind".into()))
}

/// Which recovery options `server` offers. Works before logging in.
#[tauri::command]
pub async fn server_features(state: State<'_, AppState>, server: String) -> Result<Features> {
    state.session.public_get(&normalize_server(&server)?, "v1/auth/features").await
}

/// `channel` is "email" or "sms".
#[tauri::command]
pub async fn request_password_reset(state: State<'_, AppState>, server: String, username: String, channel: String) -> Result<()> {
    let body = json!({ "username": username, "channel": channel });
    state.session.public_post(&normalize_server(&server)?, "v1/auth/password-reset/request", &body).await
}

#[tauri::command]
pub async fn confirm_password_reset(
    state: State<'_, AppState>,
    server: String,
    username: String,
    code: String,
    new_password: String,
) -> Result<()> {
    let body = json!({ "username": username, "code": code, "new_password": new_password });
    state.session.public_post(&normalize_server(&server)?, "v1/auth/password-reset/confirm", &body).await
}

/// Fetches the signed-in user from the server (refreshes the cached copy).
#[tauri::command]
pub async fn fetch_me(state: State<'_, AppState>) -> Result<User> {
    let user: User = state.session.call(Method::GET, "v1/users/me", None).await?;
    state.session.set_user(user.clone()).await;
    Ok(user)
}

#[tauri::command]
pub async fn change_password(state: State<'_, AppState>, current_password: String, new_password: String) -> Result<User> {
    state.session.change_password(&current_password, &new_password).await
}

/// Sets an email/phone (unverified) and has the server send it a code.
#[tauri::command]
pub async fn set_contact(state: State<'_, AppState>, kind: String, value: String, password: String) -> Result<()> {
    let path = format!("v1/users/me/{}", contact(&kind)?);
    let body = json!({ "value": value, "password": password });
    state.session.call_empty(Method::PUT, &path, Some(body)).await
}

#[tauri::command]
pub async fn verify_contact(state: State<'_, AppState>, kind: String, code: String) -> Result<User> {
    let path = format!("v1/users/me/{}/verify", contact(&kind)?);
    let user: User = state.session.call(Method::POST, &path, Some(json!({ "code": code }))).await?;
    state.session.set_user(user.clone()).await;
    Ok(user)
}

#[tauri::command]
pub async fn remove_contact(state: State<'_, AppState>, kind: String, password: String) -> Result<User> {
    let path = format!("v1/users/me/{}", contact(&kind)?);
    let user: User = state.session.call(Method::DELETE, &path, Some(json!({ "password": password }))).await?;
    state.session.set_user(user.clone()).await;
    Ok(user)
}

#[tauri::command]
pub async fn list_friends(state: State<'_, AppState>) -> Result<Vec<Friend>> {
    let list: FriendsList = state.session.call(Method::GET, "v1/friends", None).await?;
    Ok(list.friends)
}

#[tauri::command]
pub async fn remove_friend(state: State<'_, AppState>, user_id: String) -> Result<()> {
    let path = format!("v1/friends/{}", id(&user_id)?);
    state.session.call_empty(Method::DELETE, &path, None).await
}

#[tauri::command]
pub async fn list_requests(state: State<'_, AppState>) -> Result<Requests> {
    state.session.call(Method::GET, "v1/friends/requests", None).await
}

#[tauri::command]
pub async fn send_friend_request(state: State<'_, AppState>, username: String) -> Result<SendResult> {
    let body = json!({ "username": username });
    state.session.call(Method::POST, "v1/friends/requests", Some(body)).await
}

#[tauri::command]
pub async fn accept_friend_request(state: State<'_, AppState>, request_id: String) -> Result<Friend> {
    let path = format!("v1/friends/requests/{}/accept", id(&request_id)?);
    state.session.call(Method::POST, &path, None).await
}

#[tauri::command]
pub async fn delete_friend_request(state: State<'_, AppState>, request_id: String) -> Result<()> {
    let path = format!("v1/friends/requests/{}", id(&request_id)?);
    state.session.call_empty(Method::DELETE, &path, None).await
}

#[tauri::command]
pub async fn list_blocked(state: State<'_, AppState>) -> Result<Vec<Blocked>> {
    let list: BlockedList = state.session.call(Method::GET, "v1/blocks", None).await?;
    Ok(list.blocked)
}

#[tauri::command]
pub async fn block_user(state: State<'_, AppState>, user_id: String) -> Result<()> {
    let path = format!("v1/blocks/{}", id(&user_id)?);
    state.session.call_empty(Method::PUT, &path, None).await
}

#[tauri::command]
pub async fn unblock_user(state: State<'_, AppState>, user_id: String) -> Result<()> {
    let path = format!("v1/blocks/{}", id(&user_id)?);
    state.session.call_empty(Method::DELETE, &path, None).await
}
