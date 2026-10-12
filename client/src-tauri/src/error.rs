use serde::{Serialize, Serializer};

/// Errors returned to the UI. Serialized as `{ "code": ..., "message": ... }`.
#[derive(Debug, thiserror::Error)]
pub enum AppError {
    /// The server answered with an error body (see api/openapi.yaml for codes).
    #[error("{message}")]
    Api { code: String, message: String },
    #[error("could not reach the server: {0}")]
    Network(String),
    #[error("you are not logged in")]
    NotLoggedIn,
    #[error("your session has expired; please log in again")]
    SessionExpired,
    #[error("{0}")]
    InvalidServer(String),
    #[error("{0}")]
    Internal(String),
}

impl AppError {
    fn code(&self) -> &str {
        match self {
            AppError::Api { code, .. } => code,
            AppError::Network(_) => "network",
            AppError::NotLoggedIn => "not_logged_in",
            AppError::SessionExpired => "session_expired",
            AppError::InvalidServer(_) => "invalid_server",
            AppError::Internal(_) => "internal",
        }
    }
}

impl Serialize for AppError {
    fn serialize<S: Serializer>(&self, s: S) -> std::result::Result<S::Ok, S::Error> {
        use serde::ser::SerializeStruct;
        let mut st = s.serialize_struct("AppError", 2)?;
        st.serialize_field("code", self.code())?;
        st.serialize_field("message", &self.to_string())?;
        st.end()
    }
}

impl From<reqwest::Error> for AppError {
    fn from(e: reqwest::Error) -> Self {
        if e.is_decode() {
            AppError::Internal(format!("unexpected response from server: {e}"))
        } else {
            AppError::Network(e.without_url().to_string())
        }
    }
}

pub type Result<T> = std::result::Result<T, AppError>;
