-- Contact methods for account recovery ---------------------------------------
-- Optional, and only usable once verified. Email is stored lowercased; phone in
-- E.164 form (+15551234567). Uniqueness applies to verified values only, so an
-- unverified entry can't squat on someone else's address.

ALTER TABLE users
    ADD COLUMN email             text,
    ADD COLUMN email_verified_at timestamptz,
    ADD COLUMN phone             text,
    ADD COLUMN phone_verified_at timestamptz;

CREATE UNIQUE INDEX users_verified_email_key ON users (email) WHERE email_verified_at IS NOT NULL;
CREATE UNIQUE INDEX users_verified_phone_key ON users (phone) WHERE phone_verified_at IS NOT NULL;

-- Short numeric codes sent by email/SMS, for verifying a contact method or
-- resetting a password. Only a hash is stored.
CREATE TABLE one_time_codes (
    id          uuid PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose     text NOT NULL CHECK (purpose IN ('verify_email', 'verify_phone', 'reset_password')),
    channel     text NOT NULL CHECK (channel IN ('email', 'sms')),
    destination text NOT NULL,
    code_hash   bytea NOT NULL,
    attempts    int NOT NULL DEFAULT 0,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL
);
CREATE INDEX one_time_codes_user_purpose_idx ON one_time_codes (user_id, purpose, created_at DESC);

-- Login lockout -----------------------------------------------------------------
-- One row per throttle key: "a:<username>" (whole account) or
-- "aip:<username>|<ip>" (account from one address).

CREATE TABLE login_throttle (
    key          text PRIMARY KEY,
    failures     int NOT NULL DEFAULT 0,
    window_start timestamptz NOT NULL,
    locked_until timestamptz
);
