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
    /// "connecting", "connected", "disconnected" or "signed_out"
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
        let on_connected = || status(&app, "connected");
        let on_event = |event: serde_json::Value| {
            let _ = app.emit("realtime-event", event);
        };
        match connect_once(session, on_connected, on_event).await {
            Ok(()) => backoff = Duration::from_secs(1), // was connected; retry promptly
            Err(AppError::NotLoggedIn | AppError::SessionExpired) => {
                status(&app, "signed_out");
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::tests::{live_server, reg, unique, MemoryStore};
    use std::sync::{Arc, Mutex as StdMutex};

    /// Against a live api + realtime (PARLEY_TEST_API_URL): connects with the
    /// advertised URL and receives a message sent by another user.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn receives_messages_live() {
        let Some(server) = live_server() else {
            eprintln!("PARLEY_TEST_API_URL not set; skipping");
            return;
        };
        let alice = SessionManager::new(Box::new(MemoryStore::default()));
        let bob = Arc::new(SessionManager::new(Box::new(MemoryStore::default())));
        let (a_name, b_name) = (unique("rta"), unique("rtb"));
        alice.register(&server, &reg(&a_name, "")).await.unwrap();
        let b_user = bob.register(&server, &reg(&b_name, "")).await.unwrap();

        let got: Arc<StdMutex<Vec<serde_json::Value>>> = Arc::default();
        let (conn_tx, conn_rx) = tokio::sync::oneshot::channel();
        let task = {
            let (bob, got) = (bob.clone(), got.clone());
            tokio::spawn(async move {
                let _ = connect_once(
                    &bob,
                    move || {
                        let _ = conn_tx.send(());
                    },
                    move |e| got.lock().unwrap().push(e),
                )
                .await;
            })
        };
        tokio::time::timeout(Duration::from_secs(10), conn_rx)
            .await
            .expect("connect timed out")
            .unwrap();

        let sent: serde_json::Value = alice
            .call(
                Method::POST,
                "v1/friends/requests",
                Some(serde_json::json!({ "username": b_name })),
            )
            .await
            .unwrap();
        let req_id = sent["request"]["id"].as_str().unwrap().to_string();
        bob.call_empty(
            Method::POST,
            &format!("v1/friends/requests/{req_id}/accept"),
            None,
        )
        .await
        .unwrap();
        let conv: serde_json::Value = alice
            .call(
                Method::POST,
                "v1/conversations/dm",
                Some(serde_json::json!({ "user_id": b_user.id })),
            )
            .await
            .unwrap();
        let path = format!("v1/conversations/{}/messages", conv["id"].as_str().unwrap());
        let _: serde_json::Value = alice
            .call(
                Method::POST,
                &path,
                Some(serde_json::json!({ "content": "hi from rust", "nonce": "x" })),
            )
            .await
            .unwrap();

        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        loop {
            let types: Vec<String> = got
                .lock()
                .unwrap()
                .iter()
                .map(|e| e["type"].as_str().unwrap_or("").to_string())
                .collect();
            if types.iter().any(|t| t == "message.created") {
                assert_eq!(types.first().map(String::as_str), Some("ready"));
                break;
            }
            assert!(
                tokio::time::Instant::now() < deadline,
                "no message.created; got {types:?}"
            );
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        let msg = got
            .lock()
            .unwrap()
            .iter()
            .find(|e| e["type"] == "message.created")
            .cloned()
            .unwrap();
        assert_eq!(msg["data"]["content"], "hi from rust");
        task.abort();
    }

    use reqwest::Method;
}
