//! Tauri commands invoked by the UI (see src/api.ts for the TypeScript side).

use reqwest::Method;
use serde::Serialize;
use serde_json::{json, Value};
use tauri::{AppHandle, State};
use url::Url;

use crate::api::{Blocked, BlockedList, Features, Friend, FriendsList, Requests, SendResult, User};
use crate::config::ConfigStore;
use crate::error::{AppError, Result};
use crate::realtime::Realtime;
use crate::session::{normalize_server, SessionManager};

pub struct AppState {
    pub session: SessionManager,
    pub config: ConfigStore,
    pub realtime: Realtime,
    pub voice: crate::voice::Voice,
}

/// Path segments come from the UI, so only accept UUID-shaped IDs.
fn id(s: &str) -> Result<&str> {
    let ok = s.len() == 36 && s.chars().all(|c| c.is_ascii_hexdigit() || c == '-');
    ok.then_some(s)
        .ok_or_else(|| AppError::Internal("invalid id".into()))
}

#[tauri::command]
pub fn saved_server(state: State<'_, AppState>) -> Option<String> {
    state.config.load().server_url
}

/// What the login form starts with: the last server, username and "Stay
/// signed in" choice.
#[derive(Serialize)]
pub struct SavedLogin {
    server: Option<String>,
    username: Option<String>,
    stay_signed_in: bool,
}

#[tauri::command]
pub fn saved_login(state: State<'_, AppState>) -> SavedLogin {
    let c = state.config.load();
    SavedLogin { server: c.server_url, username: c.username, stay_signed_in: c.stay_signed_in.unwrap_or(true) }
}

#[tauri::command]
pub async fn restore_session(state: State<'_, AppState>) -> Result<Option<User>> {
    let Some(server) = state.config.load().server_url else {
        return Ok(None);
    };
    state.session.restore(&normalize_server(&server)?).await
}

/// Requests a new account; the server sends an activation link.
#[tauri::command]
pub async fn signup(
    state: State<'_, AppState>,
    server: String,
    username: String,
    display_name: String,
    email: Option<String>,
    phone: Option<String>,
) -> Result<()> {
    let server = normalize_server(&server)?;
    let body = json!({ "username": username, "display_name": display_name, "email": email, "phone": phone });
    state.session.public_post(&server, "v1/auth/signup", &body).await?;
    state.config.update(|c| c.server_url = Some(server.to_string()));
    Ok(())
}

#[derive(Serialize)]
pub struct LoginOutcome {
    user: Option<User>,
    mfa_required: bool,
}

#[tauri::command]
pub async fn login(
    state: State<'_, AppState>,
    server: String,
    username: String,
    password: String,
    stay_signed_in: Option<bool>,
) -> Result<LoginOutcome> {
    let server = normalize_server(&server)?;
    let stay = stay_signed_in.unwrap_or(true);
    state.session.set_persist(stay);
    let user = state.session.login(&server, &username, &password).await?;
    if let Some(u) = &user {
        remember_login(&state, &server, u, stay);
    }
    Ok(LoginOutcome { mfa_required: user.is_none(), user })
}

/// The second step of a login: an authenticator or recovery code.
#[tauri::command]
pub async fn mfa_login(state: State<'_, AppState>, code: String, stay_signed_in: Option<bool>) -> Result<User> {
    let (server, user) = state.session.mfa_login(&code).await?;
    remember_login(&state, &server, &user, stay_signed_in.unwrap_or(true));
    Ok(user)
}

#[derive(Serialize)]
pub struct MfaSetup {
    secret: String,
    /// QR code as rows of `size` modules, dark = true.
    qr: Vec<bool>,
    size: usize,
}

#[tauri::command]
pub async fn mfa_enroll_start(state: State<'_, AppState>) -> Result<MfaSetup> {
    let s = state.session.mfa_enroll_start().await?;
    let code = qrcode::QrCode::new(s.otpauth_uri.as_bytes()).map_err(|e| AppError::Internal(e.to_string()))?;
    Ok(MfaSetup {
        secret: s.secret,
        size: code.width(),
        qr: code.to_colors().into_iter().map(|c| c == qrcode::Color::Dark).collect(),
    })
}

#[derive(Serialize)]
pub struct MfaConfirmed {
    user: User,
    recovery_codes: Vec<String>,
}

#[tauri::command]
pub async fn mfa_enroll_confirm(state: State<'_, AppState>, code: String) -> Result<MfaConfirmed> {
    let (user, recovery_codes) = state.session.mfa_enroll_confirm(&code).await?;
    Ok(MfaConfirmed { user, recovery_codes })
}

#[tauri::command]
pub async fn mfa_reset(state: State<'_, AppState>, password: String, code: String) -> Result<User> {
    state.session.mfa_reset(&password, &code).await
}

fn remember_login(state: &AppState, server: &Url, user: &User, stay: bool) {
    state.config.update(|c| {
        c.server_url = Some(server.to_string());
        c.username = Some(user.username.clone());
        c.stay_signed_in = Some(stay);
    });
}

#[tauri::command]
pub async fn logout(state: State<'_, AppState>) -> Result<()> {
    crate::voice::voice_leave(state.clone());
    state.realtime.stop().await;
    state.session.logout().await;
    Ok(())
}

#[tauri::command]
pub async fn current_user(state: State<'_, AppState>) -> Result<Option<User>> {
    Ok(state.session.current_user().await)
}

/// "email" or "phone" from the UI, as used in /v1/users/me/{contact}.
fn contact(kind: &str) -> Result<&str> {
    matches!(kind, "email" | "phone")
        .then_some(kind)
        .ok_or_else(|| AppError::Internal("invalid contact kind".into()))
}

/// Which recovery options `server` offers. Works before logging in.
#[tauri::command]
pub async fn server_features(state: State<'_, AppState>, server: String) -> Result<Features> {
    state
        .session
        .public_get(&normalize_server(&server)?, "v1/auth/features")
        .await
}

/// `identifier` is a username, email address or phone number.
#[tauri::command]
pub async fn request_password_reset(
    state: State<'_, AppState>,
    server: String,
    identifier: String,
) -> Result<()> {
    let body = json!({ "identifier": identifier });
    state
        .session
        .public_post(
            &normalize_server(&server)?,
            "v1/auth/password-reset/request",
            &body,
        )
        .await
}

/// Fetches the signed-in user from the server (refreshes the cached copy).
#[tauri::command]
pub async fn fetch_me(state: State<'_, AppState>) -> Result<User> {
    let user: User = state.session.call(Method::GET, "v1/users/me", None).await?;
    state.session.set_user(user.clone()).await;
    Ok(user)
}

#[tauri::command]
pub async fn change_password(
    state: State<'_, AppState>,
    current_password: String,
    new_password: String,
) -> Result<User> {
    state
        .session
        .change_password(&current_password, &new_password)
        .await
}

/// Sets an email/phone (unverified) and has the server send it a code.
#[tauri::command]
pub async fn set_contact(
    state: State<'_, AppState>,
    kind: String,
    value: String,
    password: String,
) -> Result<()> {
    let path = format!("v1/users/me/{}", contact(&kind)?);
    let body = json!({ "value": value, "password": password });
    state
        .session
        .call_empty(Method::PUT, &path, Some(body))
        .await
}

#[tauri::command]
pub async fn verify_contact(
    state: State<'_, AppState>,
    kind: String,
    code: String,
) -> Result<User> {
    let path = format!("v1/users/me/{}/verify", contact(&kind)?);
    let user: User = state
        .session
        .call(Method::POST, &path, Some(json!({ "code": code })))
        .await?;
    state.session.set_user(user.clone()).await;
    Ok(user)
}

#[tauri::command]
pub async fn remove_contact(
    state: State<'_, AppState>,
    kind: String,
    password: String,
) -> Result<User> {
    let path = format!("v1/users/me/{}", contact(&kind)?);
    let user: User = state
        .session
        .call(Method::DELETE, &path, Some(json!({ "password": password })))
        .await?;
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
    state
        .session
        .call(Method::GET, "v1/friends/requests", None)
        .await
}

#[tauri::command]
pub async fn send_friend_request(
    state: State<'_, AppState>,
    username: String,
) -> Result<SendResult> {
    let body = json!({ "username": username });
    state
        .session
        .call(Method::POST, "v1/friends/requests", Some(body))
        .await
}

#[tauri::command]
pub async fn accept_friend_request(
    state: State<'_, AppState>,
    request_id: String,
) -> Result<Friend> {
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

// Realtime -----------------------------------------------------------------------

/// Opens (or reopens) the live event connection for the signed-in user.
#[tauri::command]
pub async fn realtime_start(app: AppHandle, state: State<'_, AppState>) -> Result<()> {
    state.realtime.start(app).await;
    Ok(())
}

// Conversations and messages ------------------------------------------------------
// Responses pass through unchanged; src/api.ts holds their types.

#[tauri::command]
pub async fn list_conversations(state: State<'_, AppState>) -> Result<Value> {
    let mut v: Value = state
        .session
        .call(Method::GET, "v1/conversations", None)
        .await?;
    Ok(v["conversations"].take())
}

#[tauri::command]
pub async fn get_conversation(
    state: State<'_, AppState>,
    conversation_id: String,
) -> Result<Value> {
    let path = format!("v1/conversations/{}", id(&conversation_id)?);
    state.session.call(Method::GET, &path, None).await
}

/// Adds friends to a group (the server skips anyone already in it).
#[tauri::command]
pub async fn add_group_members(
    state: State<'_, AppState>,
    conversation_id: String,
    user_ids: Vec<String>,
) -> Result<Value> {
    let ids = user_ids.iter().map(|u| id(u)).collect::<Result<Vec<_>>>()?;
    let path = format!("v1/conversations/{}/members", id(&conversation_id)?);
    state.session.call(Method::POST, &path, Some(json!({ "user_ids": ids }))).await
}

#[tauri::command]
pub async fn leave_group(state: State<'_, AppState>, conversation_id: String) -> Result<()> {
    let path = format!("v1/conversations/{}/members/me", id(&conversation_id)?);
    state.session.call_empty(Method::DELETE, &path, None).await
}

#[tauri::command]
pub async fn open_dm(state: State<'_, AppState>, user_id: String) -> Result<Value> {
    let body = json!({ "user_id": id(&user_id)? });
    state
        .session
        .call(Method::POST, "v1/conversations/dm", Some(body))
        .await
}

#[tauri::command]
pub async fn create_group(
    state: State<'_, AppState>,
    user_ids: Vec<String>,
    name: String,
) -> Result<Value> {
    let ids = user_ids.iter().map(|u| id(u)).collect::<Result<Vec<_>>>()?;
    let body = json!({ "user_ids": ids, "name": name });
    state
        .session
        .call(Method::POST, "v1/conversations/group", Some(body))
        .await
}

/// Messages in ascending order: the newest page, or those before/after a message id.
#[tauri::command]
pub async fn list_messages(
    state: State<'_, AppState>,
    conversation_id: String,
    before: Option<String>,
    after: Option<String>,
) -> Result<Value> {
    let mut path = format!(
        "v1/conversations/{}/messages?limit=50",
        id(&conversation_id)?
    );
    if let Some(b) = before.as_deref() {
        path.push_str(&format!("&before={}", id(b)?));
    }
    if let Some(a) = after.as_deref() {
        path.push_str(&format!("&after={}", id(a)?));
    }
    let mut v: Value = state.session.call(Method::GET, &path, None).await?;
    Ok(v["messages"].take())
}

#[tauri::command]
pub async fn send_message(
    state: State<'_, AppState>,
    conversation_id: String,
    content: String,
    nonce: String,
) -> Result<Value> {
    let path = format!("v1/conversations/{}/messages", id(&conversation_id)?);
    state
        .session
        .call(
            Method::POST,
            &path,
            Some(json!({ "content": content, "nonce": nonce })),
        )
        .await
}

#[tauri::command]
pub async fn mark_read(
    state: State<'_, AppState>,
    conversation_id: String,
    message_id: String,
) -> Result<()> {
    let path = format!("v1/conversations/{}/read", id(&conversation_id)?);
    state
        .session
        .call_empty(
            Method::POST,
            &path,
            Some(json!({ "message_id": id(&message_id)? })),
        )
        .await
}

// About ------------------------------------------------------------------------

/// Identifies this build, for the About page and bug reports.
#[derive(serde::Serialize)]
pub struct AppInfo {
    version: &'static str,
    commit: &'static str,
    os: &'static str,
    arch: &'static str,
    tauri_version: &'static str,
    webview_version: Option<String>,
}

#[tauri::command]
pub fn app_info() -> AppInfo {
    AppInfo {
        version: env!("CARGO_PKG_VERSION"),
        commit: env!("PARLEY_COMMIT"),
        os: std::env::consts::OS,
        arch: std::env::consts::ARCH,
        tauri_version: tauri::VERSION,
        webview_version: tauri::webview_version().ok(),
    }
}

/// Server metadata (GET /v1/meta): its version, realtime URL, features.
#[tauri::command]
pub async fn server_meta(state: State<'_, AppState>, server: String) -> Result<Value> {
    state.session.public_get(&normalize_server(&server)?, "v1/meta").await
}

/// Opens the folder with the app's log files.
#[tauri::command]
pub fn open_logs(app: AppHandle) -> Result<()> {
    use tauri::Manager;
    use tauri_plugin_opener::OpenerExt;
    let dir = app.path().app_log_dir().map_err(|e| AppError::Internal(e.to_string()))?;
    std::fs::create_dir_all(&dir).map_err(|e| AppError::Internal(e.to_string()))?;
    app.opener()
        .open_path(dir.to_string_lossy(), None::<&str>)
        .map_err(|e| AppError::Internal(e.to_string()))
}
