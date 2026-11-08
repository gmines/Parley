//! Owns the login session: access/refresh tokens, transparent token refresh and
//! authenticated requests. Tokens never leave this module, so the webview
//! never sees them.
//!
//! Refresh tokens are single-use on the server, and reusing one revokes the
//! whole session. Every refresh therefore happens while holding `state`'s
//! lock, so two requests can never refresh concurrently with the same token.
//!
//! The refresh token is kept in the OS credential store so the next launch
//! resumes the session, unless the user chose not to stay signed in.

use std::sync::atomic::{AtomicBool, Ordering};

use chrono::{DateTime, Duration, Utc};
use reqwest::{Method, Response, StatusCode};
use serde::de::DeserializeOwned;
use serde_json::{json, Value};
use tokio::sync::Mutex;
use url::Url;

use crate::api::{ErrorBody, LoginResponse, MfaEnrolled, MfaSecret, Session, User};
use crate::error::{AppError, Result};
use crate::secrets::SecretStore;

/// Refresh the access token this long before it expires.
const REFRESH_MARGIN: Duration = Duration::seconds(30);

struct Active {
    server: Url,
    user: User,
    access: String,
    access_exp: DateTime<Utc>,
    refresh: String,
}

pub struct SessionManager {
    http: reqwest::Client,
    state: Mutex<Option<Active>>,
    /// A login waiting for its second factor. Like the tokens, never leaves here.
    challenge: Mutex<Option<(Url, String)>>,
    secrets: Box<dyn SecretStore>,
    /// Save the refresh token for the next launch ("stay signed in").
    persist: AtomicBool,
}

impl SessionManager {
    pub fn new(secrets: Box<dyn SecretStore>) -> Self {
        let http = reqwest::Client::builder()
            .user_agent(concat!("parley-client/", env!("CARGO_PKG_VERSION")))
            .connect_timeout(std::time::Duration::from_secs(10))
            .timeout(std::time::Duration::from_secs(30))
            .build()
            .expect("build http client");
        Self {
            http,
            state: Mutex::new(None),
            challenge: Mutex::new(None),
            secrets,
            persist: AtomicBool::new(true),
        }
    }

    /// Whether sessions started from now on are saved for the next launch.
    pub fn set_persist(&self, persist: bool) {
        self.persist.store(persist, Ordering::Relaxed);
    }

    /// Saves the latest refresh token, or makes sure none is kept.
    fn save(&self, server: &Url, refresh: &str) {
        if self.persist.load(Ordering::Relaxed) {
            self.secrets.set(server.as_str(), refresh);
        } else {
            self.secrets.delete(server.as_str());
        }
    }

    /// Returns the user, or None if the login needs a second factor (see
    /// [`mfa_login`](Self::mfa_login)).
    pub async fn login(&self, server: &Url, username: &str, password: &str) -> Result<Option<User>> {
        let body = json!({ "username": username, "password": password });
        match decode(self.post(server, "v1/auth/login", &body).await?).await? {
            LoginResponse::Challenge { mfa_token } => {
                *self.challenge.lock().await = Some((server.clone(), mfa_token));
                Ok(None)
            }
            LoginResponse::Session(sess) => Ok(Some(self.start(server, sess).await)),
        }
    }

    /// Finishes a login with an authenticator or recovery code. The challenge
    /// is kept on failure so the user can try another code.
    pub async fn mfa_login(&self, code: &str) -> Result<(Url, User)> {
        let (server, token) = self.challenge.lock().await.clone().ok_or(AppError::NotLoggedIn)?;
        let body = json!({ "mfa_token": token, "code": code });
        let sess: Session = decode(self.post(&server, "v1/auth/mfa/login", &body).await?).await?;
        *self.challenge.lock().await = None;
        let user = self.start(&server, sess).await;
        Ok((server, user))
    }

    /// Starts authenticator setup. Works with the restricted session an
    /// account without MFA gets.
    pub async fn mfa_enroll_start(&self) -> Result<MfaSecret> {
        let mut guard = self.state.lock().await;
        let (server, token) = self.fresh_access(&mut guard, None).await?;
        let resp = self
            .send(&server, Method::POST, "v1/auth/mfa/enroll/start", &token, None)
            .await?;
        decode(check(resp).await?).await
    }

    /// Finishes setup. The server revokes every earlier session, so the state
    /// lock is held until the replacement is in place.
    pub async fn mfa_enroll_confirm(&self, code: &str) -> Result<(User, Vec<String>)> {
        let mut guard = self.state.lock().await;
        let (server, token) = self.fresh_access(&mut guard, None).await?;
        let body = json!({ "code": code });
        let resp = self
            .send(&server, Method::POST, "v1/auth/mfa/enroll/confirm", &token, Some(&body))
            .await?;
        let out: MfaEnrolled = decode(check(resp).await?).await?;
        self.save(&server, &out.session.refresh_token);
        let user = out.session.user.clone();
        *guard = Some(Active::new(server, out.session));
        Ok((user, out.recovery_codes))
    }

    /// Removes the authenticator; the replacement session can only enroll a new one.
    pub async fn mfa_reset(&self, password: &str, code: &str) -> Result<User> {
        let mut guard = self.state.lock().await;
        let (server, token) = self.fresh_access(&mut guard, None).await?;
        let body = json!({ "password": password, "code": code });
        let resp = self
            .send(&server, Method::POST, "v1/users/me/mfa/reset", &token, Some(&body))
            .await?;
        let sess: Session = decode(check(resp).await?).await?;
        self.save(&server, &sess.refresh_token);
        let user = sess.user.clone();
        *guard = Some(Active::new(server, sess));
        Ok(user)
    }

    /// Resumes a session from the refresh token saved in the OS keychain.
    /// Returns Ok(None) if there is no saved session or the server rejected it.
    pub async fn restore(&self, server: &Url) -> Result<Option<User>> {
        let Some(refresh) = self.secrets.get(server.as_str()) else {
            return Ok(None);
        };
        match self.refresh_call(server, &refresh).await {
            Ok(sess) => {
                self.set_persist(true);
                Ok(Some(self.start(server, sess).await))
            }
            Err(e) if refresh_rejected(&e) => {
                self.secrets.delete(server.as_str());
                Ok(None)
            }
            Err(e) => Err(e),
        }
    }

    /// Ends the session locally and, best effort, on the server.
    pub async fn logout(&self) {
        let Some(active) = self.state.lock().await.take() else {
            return;
        };
        self.secrets.delete(active.server.as_str());
        let body = json!({ "refresh_token": active.refresh });
        if let Err(e) = self.post(&active.server, "v1/auth/logout", &body).await {
            log::warn!("server logout failed: {e}");
        }
    }

    pub async fn current_user(&self) -> Option<User> {
        self.state.lock().await.as_ref().map(|a| a.user.clone())
    }

    /// Updates the cached user (e.g. after verifying a contact method).
    pub async fn set_user(&self, user: User) {
        if let Some(a) = self.state.lock().await.as_mut() {
            a.user = user;
        }
    }

    /// The server and a currently valid access token, refreshing if needed.
    /// For connections the manager doesn't make itself (the realtime socket).
    pub async fn bearer(&self) -> Result<(Url, String)> {
        self.access_token(None).await
    }

    /// Unauthenticated GET, for endpoints used before logging in.
    pub async fn public_get<T: DeserializeOwned>(&self, server: &Url, path: &str) -> Result<T> {
        decode(check(self.http.get(join(server, path)?).send().await?).await?).await
    }

    /// Unauthenticated POST whose response body is ignored.
    pub async fn public_post(&self, server: &Url, path: &str, body: &Value) -> Result<()> {
        self.post(server, path, body).await.map(drop)
    }

    /// Changes the password. The server signs out every session, including
    /// this one, and returns a replacement; the state lock is held throughout
    /// so no concurrent refresh can run with the revoked token in between.
    pub async fn change_password(&self, current: &str, new: &str) -> Result<User> {
        let mut guard = self.state.lock().await;
        let (server, token) = self.fresh_access(&mut guard, None).await?;
        let body = json!({ "current_password": current, "new_password": new });
        let resp = self
            .send(
                &server,
                Method::POST,
                "v1/users/me/password",
                &token,
                Some(&body),
            )
            .await?;
        let sess: Session = decode(check(resp).await?).await?;
        self.save(&server, &sess.refresh_token);
        let user = sess.user.clone();
        *guard = Some(Active::new(server, sess));
        Ok(user)
    }

    /// Authenticated request returning a JSON body.
    pub async fn call<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Option<Value>,
    ) -> Result<T> {
        decode(self.authed(None, method, path, body).await?).await
    }

    /// Authenticated request whose response body is ignored (e.g. 204).
    pub async fn call_empty(&self, method: Method, path: &str, body: Option<Value>) -> Result<()> {
        self.authed(None, method, path, body).await.map(drop)
    }

    /// Like [`call`], but against another service's base URL (e.g.
    /// voice-control), with the same session.
    pub async fn call_at<T: DeserializeOwned>(&self, base: &Url, method: Method, path: &str, body: Option<Value>) -> Result<T> {
        decode(self.authed(Some(base), method, path, body).await?).await
    }

    /// Like [`call_empty`], against another service's base URL.
    pub async fn call_empty_at(&self, base: &Url, method: Method, path: &str, body: Option<Value>) -> Result<()> {
        self.authed(Some(base), method, path, body).await.map(drop)
    }

    /// The server the session belongs to.
    pub async fn server(&self) -> Result<Url> {
        Ok(self.access_token(None).await?.0)
    }

    async fn authed(&self, base: Option<&Url>, method: Method, path: &str, body: Option<Value>) -> Result<Response> {
        let (server, token) = self.access_token(None).await?;
        let resp = self
            .send(base.unwrap_or(&server), method.clone(), path, &token, body.as_ref())
            .await?;
        if resp.status() != StatusCode::UNAUTHORIZED {
            return check(resp).await;
        }
        // Token rejected (e.g. server key rotated): refresh once and retry.
        let (server, token) = self.access_token(Some(&token)).await?;
        check(
            self.send(base.unwrap_or(&server), method, path, &token, body.as_ref())
                .await?,
        )
        .await
    }

    async fn send(
        &self,
        server: &Url,
        method: Method,
        path: &str,
        token: &str,
        body: Option<&Value>,
    ) -> Result<Response> {
        let mut req = self
            .http
            .request(method, join(server, path)?)
            .bearer_auth(token);
        if let Some(b) = body {
            req = req.json(b);
        }
        Ok(req.send().await?)
    }

    /// Returns a usable access token, refreshing it if it is about to expire
    /// or if `rejected` is the current token.
    async fn access_token(&self, rejected: Option<&str>) -> Result<(Url, String)> {
        let mut guard = self.state.lock().await;
        let access = self.fresh_access(&mut guard, rejected).await?;
        // A restricted token works nowhere else, so say why instead of a 401.
        if guard.as_ref().is_some_and(|a| !a.user.mfa_enabled) {
            return Err(AppError::Api {
                code: "mfa_setup_required".into(),
                message: "set up two-factor authentication to continue".into(),
            });
        }
        Ok(access)
    }

    /// access_token's body, for callers that already hold the state lock.
    async fn fresh_access(
        &self,
        guard: &mut Option<Active>,
        rejected: Option<&str>,
    ) -> Result<(Url, String)> {
        let active = guard.as_mut().ok_or(AppError::NotLoggedIn)?;
        let stale = match rejected {
            // Another request may already have refreshed past the rejected token.
            Some(t) => active.access == t,
            None => active.access_exp - REFRESH_MARGIN <= Utc::now(),
        };
        if !stale {
            return Ok((active.server.clone(), active.access.clone()));
        }
        match self.refresh_call(&active.server, &active.refresh).await {
            Ok(sess) => {
                self.save(&active.server, &sess.refresh_token);
                active.user = sess.user;
                active.access = sess.access_token;
                active.access_exp = sess.access_expires_at;
                active.refresh = sess.refresh_token;
                Ok((active.server.clone(), active.access.clone()))
            }
            Err(e) if refresh_rejected(&e) => {
                self.secrets.delete(active.server.as_str());
                *guard = None;
                Err(AppError::SessionExpired)
            }
            // Network trouble or a server error (e.g. mid-deploy): keep the
            // session and let the caller retry later. The server accepts a
            // retry of a refresh whose response we never got.
            Err(e) => Err(e),
        }
    }

    async fn refresh_call(&self, server: &Url, refresh: &str) -> Result<Session> {
        decode(
            self.post(
                server,
                "v1/auth/refresh",
                &json!({ "refresh_token": refresh }),
            )
            .await?,
        )
        .await
    }

    async fn post(&self, server: &Url, path: &str, body: &Value) -> Result<Response> {
        check(
            self.http
                .post(join(server, path)?)
                .json(body)
                .send()
                .await?,
        )
        .await
    }

    async fn start(&self, server: &Url, sess: Session) -> User {
        self.save(server, &sess.refresh_token);
        let user = sess.user.clone();
        *self.state.lock().await = Some(Active::new(server.clone(), sess));
        user
    }
}

impl Active {
    fn new(server: Url, sess: Session) -> Self {
        Self {
            server,
            user: sess.user,
            access: sess.access_token,
            access_exp: sess.access_expires_at,
            refresh: sess.refresh_token,
        }
    }
}

/// Validates and normalizes a user-entered server address. A missing scheme
/// defaults to https. The result always ends in '/' so API paths join onto it,
/// which also allows hosting under a sub-path behind a reverse proxy.
pub fn normalize_server(input: &str) -> Result<Url> {
    let input = input.trim();
    if input.is_empty() {
        return Err(AppError::InvalidServer("enter a server address".into()));
    }
    let with_scheme = if input.contains("://") {
        input.to_string()
    } else {
        format!("https://{input}")
    };
    let mut url = Url::parse(&with_scheme)
        .map_err(|e| AppError::InvalidServer(format!("invalid server address: {e}")))?;
    if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
        return Err(AppError::InvalidServer(
            "server address must be an http(s) URL".into(),
        ));
    }
    url.set_query(None);
    url.set_fragment(None);
    if !url.path().ends_with('/') {
        let p = format!("{}/", url.path());
        url.set_path(&p);
    }
    Ok(url)
}

fn join(server: &Url, path: &str) -> Result<Url> {
    server
        .join(path)
        .map_err(|e| AppError::Internal(e.to_string()))
}

async fn check(resp: Response) -> Result<Response> {
    let status = resp.status();
    if status.is_success() {
        return Ok(resp);
    }
    Err(match resp.json::<ErrorBody>().await {
        Ok(b) => AppError::Api {
            code: b.code,
            message: b.message,
        },
        Err(_) => AppError::Api {
            code: format!("http_{}", status.as_u16()),
            message: format!("server returned {status}"),
        },
    })
}

/// The server refused the refresh token itself (expired, revoked, signed out
/// elsewhere), as opposed to failing to answer: only then is the session over.
fn refresh_rejected(e: &AppError) -> bool {
    matches!(e, AppError::Api { code, .. } if code == "invalid_refresh_token" || code == "http_401")
}

async fn decode<T: DeserializeOwned>(resp: Response) -> Result<T> {
    Ok(resp.json().await?)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::{Arc, Mutex as StdMutex};

    #[derive(Default, Clone)]
    pub(crate) struct MemoryStore(Arc<StdMutex<HashMap<String, String>>>);

    impl SecretStore for MemoryStore {
        fn get(&self, account: &str) -> Option<String> {
            self.0.lock().unwrap().get(account).cloned()
        }
        fn set(&self, account: &str, secret: &str) {
            self.0.lock().unwrap().insert(account.into(), secret.into());
        }
        fn delete(&self, account: &str) {
            self.0.lock().unwrap().remove(account);
        }
    }

    /// A one-shot HTTP server answering every request with `status` and `body`.
    fn canned_server(status: &'static str, body: &'static str) -> Url {
        use std::io::{Read, Write};
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
        std::thread::spawn(move || {
            for mut conn in listener.incoming().flatten() {
                let mut buf = [0u8; 4096];
                let _ = conn.read(&mut buf);
                let _ = write!(
                    conn,
                    "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                );
            }
        });
        url
    }

    #[tokio::test]
    async fn server_errors_keep_the_saved_session() {
        // e.g. the api restarting during a deploy, behind the ingress
        let server = canned_server("503 Service Unavailable", "no available server");
        let store = MemoryStore::default();
        store.set(server.as_str(), "saved-token");
        let mgr = SessionManager::new(Box::new(store.clone()));
        assert!(mgr.restore(&server).await.is_err());
        assert_eq!(store.get(server.as_str()).as_deref(), Some("saved-token"));
    }

    #[tokio::test]
    async fn rejected_refresh_token_ends_the_session() {
        let server = canned_server("401 Unauthorized", r#"{"code":"invalid_refresh_token","message":"invalid or expired refresh token"}"#);
        let store = MemoryStore::default();
        store.set(server.as_str(), "revoked-token");
        let mgr = SessionManager::new(Box::new(store.clone()));
        assert!(mgr.restore(&server).await.unwrap().is_none());
        assert_eq!(store.get(server.as_str()), None);
    }

    #[test]
    fn normalizes_server_addresses() {
        let cases = [
            ("chat.example.org", "https://chat.example.org/"),
            ("  https://chat.example.org  ", "https://chat.example.org/"),
            ("http://localhost:8080", "http://localhost:8080/"),
            (
                "https://example.org/parley?x=1#y",
                "https://example.org/parley/",
            ),
            ("https://example.org/parley/", "https://example.org/parley/"),
        ];
        for (input, want) in cases {
            assert_eq!(
                normalize_server(input).unwrap().as_str(),
                want,
                "input {input:?}"
            );
        }
        for bad in ["", "ftp://example.org", "https://", "http://exa mple.org"] {
            assert!(normalize_server(bad).is_err(), "accepted {bad:?}");
        }
    }
}
