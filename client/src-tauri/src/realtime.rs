//! Live events from the realtime service. While signed in, a background task
//! keeps one WebSocket open (reconnecting with backoff) and forwards each
//! event to the UI as a `realtime-event`; connection changes are sent as
//! `realtime-status`. Events missed while disconnected are not replayed: on
//! every (re)connect the UI gets a `ready` event and resyncs over REST.

use std::time::Duration;

use futures_util::StreamExt;
use serde::Serialize;
use tauri::{AppHandle, Emitter, Manager};
use tokio::sync::Mutex;
use tokio_tungstenite::tungstenite::{client::IntoClientRequest, http::HeaderValue, Message};
use url::Url;

use crate::commands::AppState;
use crate::error::{AppError, Result};
use crate::session::SessionManager;

const MAX_BACKOFF: Duration = Duration::from_secs(30);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);
/// The server pings every 25s; this much silence means the connection is dead
/// (e.g. the laptop slept or a NAT mapping expired).
const IDLE_TIMEOUT: Duration = Duration::from_secs(60);

#[derive(Default)]
pub struct Realtime {
    task: Mutex<Option<tauri::async_runtime::JoinHandle<()>>>,
}

#[derive(Serialize, Clone)]
struct Status {
    /// "connecting", "connected", "disconnected", "signed_out" or "mfa_required"
    state: &'static str,
}

impl Realtime {
    /// Starts (or restarts) the connection loop.
    pub async fn start(&self, app: AppHandle) {
        let mut task = self.task.lock().await;
        if let Some(t) = task.take() {
            t.abort();
        }
        *task = Some(tauri::async_runtime::spawn(run(app)));
    }

    pub async fn stop(&self) {
        if let Some(t) = self.task.lock().await.take() {
            t.abort();
        }
    }
}

fn status(app: &AppHandle, state: &'static str) {
    let _ = app.emit("realtime-status", Status { state });
}

async fn run(app: AppHandle) {
    let mut backoff = Duration::from_secs(1);
    loop {
        status(&app, "connecting");
        let session = &app.state::<AppState>().session;
        let me = session.current_user().await.map(|u| u.id);
        let on_connected = || status(&app, "connected");
        let on_event = |event: serde_json::Value| {
            // Someone's calling: show the window if it's in the background.
            if event["type"] == "call.started" && event["data"]["started_by"].as_str() != me.as_deref() {
                crate::window::incoming_call(&app);
            }
            let _ = app.emit("realtime-event", event);
        };
        match connect_once(session, on_connected, on_event).await {
            Ok(()) => backoff = Duration::from_secs(1), // was connected; retry promptly
            Err(AppError::NotLoggedIn | AppError::SessionExpired) => {
                status(&app, "signed_out");
                return;
            }
            Err(AppError::Api { code, .. }) if code == "mfa_setup_required" => {
                status(&app, "mfa_required");
                return;
            }
            Err(e) => log::warn!("realtime connection failed: {e}"),
        }
        status(&app, "disconnected");
        tokio::time::sleep(backoff + jitter()).await;
        backoff = (backoff * 2).min(MAX_BACKOFF);
    }
}

/// One connection: returns when it closes. Kept free of Tauri types so it
/// can be tested against a live server.
async fn connect_once(
    session: &SessionManager,
    on_connected: impl FnOnce(),
    on_event: impl Fn(serde_json::Value),
) -> Result<()> {
    let (server, token) = session.bearer().await?;
    let url = realtime_url(session, &server).await?;
    let mut req = url
        .as_str()
        .into_client_request()
        .map_err(|e| AppError::Internal(format!("realtime url: {e}")))?;
    req.headers_mut().insert(
        "Authorization",
        HeaderValue::from_str(&format!("Bearer {token}"))
            .map_err(|e| AppError::Internal(e.to_string()))?,
    );
    let (mut ws, _) = tokio::time::timeout(CONNECT_TIMEOUT, tokio_tungstenite::connect_async(req))
        .await
        .map_err(|_| AppError::Network("realtime connect timed out".into()))?
        .map_err(|e| AppError::Network(e.to_string()))?;
    on_connected();

    loop {
        let next = tokio::time::timeout(IDLE_TIMEOUT, ws.next())
            .await
            .map_err(|_| AppError::Network("realtime connection went silent".into()))?;
        match next {
            None | Some(Ok(Message::Close(_))) => return Ok(()),
            Some(Err(e)) => return Err(AppError::Network(e.to_string())),
            Some(Ok(Message::Text(text))) => match serde_json::from_str::<serde_json::Value>(&text)
            {
                Ok(event) => on_event(event),
                Err(e) => log::warn!("bad realtime event: {e}"),
            },
            // Pings are answered by tungstenite while reading.
            Some(Ok(_)) => {}
        }
    }
}

/// The realtime URL advertised by the server (GET /v1/meta), or, if it
/// advertises none, `<server>/v1/realtime` with a ws(s) scheme: the layout
/// behind a single ingress.
async fn realtime_url(session: &SessionManager, server: &Url) -> Result<Url> {
    let meta: serde_json::Value = session.public_get(server, "v1/meta").await?;
    if let Some(u) = meta["realtime_url"].as_str().filter(|u| !u.is_empty()) {
        return Url::parse(u)
            .map_err(|e| AppError::Internal(format!("server sent a bad realtime_url: {e}")));
    }
    let mut url = server
        .join("v1/realtime")
        .map_err(|e| AppError::Internal(e.to_string()))?;
    let scheme = if server.scheme() == "https" {
        "wss"
    } else {
        "ws"
    };
    url.set_scheme(scheme)
        .map_err(|_| AppError::Internal("cannot build realtime url".into()))?;
    Ok(url)
}

fn jitter() -> Duration {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .subsec_nanos();
    Duration::from_millis(u64::from(nanos % 1000))
}
