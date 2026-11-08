//! Wire types for the Parley REST API (api/openapi.yaml). They pass through to
//! the UI unchanged, so field names stay snake_case on both sides.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct User {
    pub id: String,
    pub username: String,
    pub display_name: String,
    pub created_at: DateTime<Utc>,
    #[serde(default)]
    pub email: String,
    #[serde(default)]
    pub email_verified: bool,
    #[serde(default)]
    pub phone: String,
    #[serde(default)]
    pub phone_verified: bool,
    /// Servers without MFA don't send this and don't require it.
    #[serde(default = "default_true")]
    pub mfa_enabled: bool,
}

fn default_true() -> bool {
    true
}

/// Optional flows the server has enabled (GET /v1/auth/features).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Features {
    #[serde(default)]
    pub email_recovery: bool,
    #[serde(default)]
    pub sms_recovery: bool,
    #[serde(default)]
    pub signup: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UserRef {
    pub id: String,
    pub username: String,
    pub display_name: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Session {
    pub user: User,
    pub access_token: String,
    pub access_expires_at: DateTime<Utc>,
    pub refresh_token: String,
}

/// POST /v1/auth/login: either a session or a request for the second factor.
#[derive(Debug, Clone, Deserialize)]
#[serde(untagged)]
pub enum LoginResponse {
    Challenge { mfa_token: String },
    Session(Session),
}

/// POST /v1/auth/mfa/enroll/start
#[derive(Debug, Clone, Deserialize)]
pub struct MfaSecret {
    pub secret: String,
    pub otpauth_uri: String,
}

/// POST /v1/auth/mfa/enroll/confirm
#[derive(Debug, Clone, Deserialize)]
pub struct MfaEnrolled {
    #[serde(flatten)]
    pub session: Session,
    pub recovery_codes: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Friend {
    pub user: UserRef,
    pub since: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FriendRequest {
    pub id: String,
    pub from: UserRef,
    pub to: UserRef,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Blocked {
    pub user: UserRef,
    pub blocked_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SendResult {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub request: Option<FriendRequest>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub friendship: Option<Friend>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Requests {
    pub incoming: Vec<FriendRequest>,
    pub outgoing: Vec<FriendRequest>,
}

#[derive(Debug, Deserialize)]
pub struct FriendsList {
    pub friends: Vec<Friend>,
}

#[derive(Debug, Deserialize)]
pub struct BlockedList {
    pub blocked: Vec<Blocked>,
}

#[derive(Debug, Deserialize)]
pub struct ErrorBody {
    pub code: String,
    pub message: String,
}
