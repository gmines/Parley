//! Voice calls in the app: gets join credentials from voice-control, drives
//! the engine (voice_engine.rs), and forwards engine events to the UI as
//! `voice-event` ({"type": "state", ...} or {"type": "media", ...}).

use std::sync::Mutex;

use reqwest::Method;
use serde::Serialize;
use serde_json::{json, Value};
use tauri::{AppHandle, Emitter, State};
use url::Url;

use crate::commands::AppState;
use crate::error::{AppError, Result};
use crate::voice_engine::{CallState, Device, Engine, EngineEvent, SipCredentials, Stats};

#[derive(Default)]
pub struct Voice {
    state: Mutex<VoiceState>,
}

#[derive(Default, Clone, Serialize)]
pub struct VoiceState {
    /// Conversation whose call we're in (or joining).
    conversation: Option<String>,
    muted: bool,
    deafened: bool,
}

fn internal(e: impl std::fmt::Display) -> AppError {
    AppError::Internal(e.to_string())
}

/// The engine, started on first use. Its events go to the UI.
fn engine(app: &AppHandle) -> Result<&'static Engine> {
    if let Some(e) = Engine::get() {
        return Ok(e);
    }
    let app = app.clone();
    Engine::start(false, 3, move |ev| match ev {
        EngineEvent::Log { level, text } => match level {
            0 | 1 => log::error!("voice: {text}"),
            2 => log::warn!("voice: {text}"),
            _ => log::debug!("voice: {text}"),
        },
        EngineEvent::Call { state, status, reason } => {
            if state == CallState::Ended {
                if let Some(v) = tauri::Manager::try_state::<AppState>(&app) {
                    v.voice.state.lock().unwrap().conversation = None;
                }
            }
            let _ = app.emit("voice-event", json!({ "type": "state", "state": state, "status": status, "reason": reason }));
        }
        EngineEvent::Media { active } => {
            let _ = app.emit("voice-event", json!({ "type": "media", "active": active }));
        }
    })
    .map_err(AppError::Internal)
}

/// voice-control's base URL: advertised by the server (GET /v1/meta
/// voice_url), or the server itself (single ingress).
async fn voice_base(state: &AppState) -> Result<Url> {
    let server = state.session.server().await?;
    let meta: Value = state.session.public_get(&server, "v1/meta").await?;
    match meta["voice_url"].as_str().filter(|u| !u.is_empty()) {
        Some(u) => {
            let mut url = Url::parse(u).map_err(|e| internal(format!("server sent a bad voice_url: {e}")))?;
            if !url.path().ends_with('/') {
                let p = format!("{}/", url.path());
                url.set_path(&p);
            }
            Ok(url)
        }
        None => Ok(server),
    }
}

fn conv_id(s: &str) -> Result<&str> {
    let ok = s.len() == 36 && s.chars().all(|c| c.is_ascii_hexdigit() || c == '-');
    ok.then_some(s).ok_or_else(|| internal("invalid conversation id"))
}

/// Joins the conversation's call (starting it if nobody's in it yet).
/// Returns the call's state before we joined (null if we're starting it).
#[tauri::command]
pub async fn voice_join(app: AppHandle, state: State<'_, AppState>, conversation_id: String) -> Result<Value> {
    let conv = conv_id(&conversation_id)?;
    if let Some(current) = state.voice.state.lock().unwrap().conversation.clone() {
        return Err(AppError::Api {
            code: "already_in_call".into(),
            message: if current == conv { "You're already in this call.".into() } else { "Leave your current call first.".into() },
        });
    }
    let user = state.session.current_user().await.ok_or(AppError::NotLoggedIn)?;
    let base = voice_base(&state).await?;
    let eng = engine(&app)?;
    // The server hands out a TLS proxy when we can use one.
    let body = json!({ "transports": eng.transports() });
    let resp: Value = state.session.call_at(&base, Method::POST, &format!("v1/calls/{conv}/join"), Some(body)).await?;
    let creds: SipCredentials = serde_json::from_value(resp["sip"].clone()).map_err(internal)?;

    {
        let s = state.voice.state.lock().unwrap();
        eng.set_mute(s.muted || s.deafened);
        eng.set_deaf(s.deafened);
    }
    eng.join(&creds, &user.id).map_err(internal)?;
    state.voice.state.lock().unwrap().conversation = Some(conv.to_string());
    Ok(resp["call"].clone())
}

#[tauri::command]
pub fn voice_leave(state: State<'_, AppState>) {
    if let Some(e) = Engine::get() {
        e.leave();
    }
    state.voice.state.lock().unwrap().conversation = None;
}

/// Mute: stop sending the microphone.
#[tauri::command]
pub fn voice_set_mute(state: State<'_, AppState>, muted: bool) -> VoiceState {
    let mut s = state.voice.state.lock().unwrap();
    s.muted = muted;
    if !muted {
        s.deafened = false; // unmuting also undeafens, like most chat apps
    }
    apply(&s);
    s.clone()
}

/// Deafen: stop playing others' audio (and mute, since you can't hear replies).
#[tauri::command]
pub fn voice_set_deaf(state: State<'_, AppState>, deafened: bool) -> VoiceState {
    let mut s = state.voice.state.lock().unwrap();
    s.deafened = deafened;
    apply(&s);
    s.clone()
}

fn apply(s: &VoiceState) {
    if let Some(e) = Engine::get() {
        e.set_mute(s.muted || s.deafened);
        e.set_deaf(s.deafened);
    }
}

#[tauri::command]
pub fn voice_status(state: State<'_, AppState>) -> VoiceState {
    state.voice.state.lock().unwrap().clone()
}

#[tauri::command]
pub fn voice_stats() -> Option<Stats> {
    Engine::get().and_then(|e| e.stats())
}

#[derive(Serialize)]
pub struct Devices {
    devices: Vec<Device>,
}

#[tauri::command]
pub fn voice_devices(app: AppHandle) -> Result<Devices> {
    Ok(Devices { devices: engine(&app)?.devices() })
}

/// Selects audio devices by index (from voice_devices); null = system default.
#[tauri::command]
pub fn voice_set_devices(app: AppHandle, capture: Option<i32>, playback: Option<i32>) -> Result<()> {
    engine(&app)?.set_devices(capture, playback).map_err(internal)
}

/// Declines a ringing call (the others' clients stop ringing for us).
#[tauri::command]
pub async fn voice_decline(state: State<'_, AppState>, conversation_id: String) -> Result<()> {
    let conv = conv_id(&conversation_id)?;
    let base = voice_base(&state).await?;
    state.session.call_empty_at(&base, Method::POST, &format!("v1/calls/{conv}/decline"), None).await
}

/// Calls in progress in your conversations (for resync after reconnecting).
#[tauri::command]
pub async fn voice_calls(state: State<'_, AppState>) -> Result<Value> {
    let base = voice_base(&state).await?;
    let mut v: Value = state.session.call_at(&base, Method::GET, "v1/calls", None).await?;
    Ok(v["calls"].take())
}

/// Hangs up on app exit so others see us leave immediately.
pub fn shutdown() {
    if let Some(e) = Engine::get() {
        e.leave();
        std::thread::sleep(std::time::Duration::from_millis(300));
        e.shutdown();
    }
}
