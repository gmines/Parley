-- Link-based sign-up ------------------------------------------------------------
-- A new account has no password and activated_at NULL until its owner follows
-- the link sent to their email or phone. Accounts that already exist are active.

ALTER TABLE users
    ALTER COLUMN password_hash DROP NOT NULL,
    ADD COLUMN activated_at       timestamptz,
    ADD COLUMN mfa_secret         bytea,          -- AES-GCM sealed TOTP secret
    ADD COLUMN mfa_pending_secret bytea,          -- same, until the first code confirms it
    ADD COLUMN mfa_enabled_at     timestamptz,
    ADD COLUMN mfa_last_step      bigint NOT NULL DEFAULT 0; -- newest accepted TOTP step (no replays)

UPDATE users SET activated_at = created_at;

ALTER TABLE users ADD CONSTRAINT users_active_has_password
    CHECK (activated_at IS NULL OR password_hash IS NOT NULL);

-- One pending sign-up per address; signing up again reuses it.
CREATE UNIQUE INDEX users_pending_email_key ON users (email) WHERE activated_at IS NULL;
CREATE UNIQUE INDEX users_pending_phone_key ON users (phone) WHERE activated_at IS NULL;

-- Single-use links for activating an account or resetting a password. The
-- token is 256 random bits and only its hash is stored.
CREATE TABLE account_links (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose     text NOT NULL CHECK (purpose IN ('activate', 'reset_password')),
    channel     text NOT NULL CHECK (channel IN ('email', 'sms')),
    destination text NOT NULL,
    token_hash  bytea NOT NULL UNIQUE,
    request_ip  text NOT NULL DEFAULT '',
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL
);
CREATE INDEX account_links_user_purpose_idx ON account_links (user_id, purpose, created_at DESC);
CREATE INDEX account_links_ip_idx ON account_links (request_ip, created_at DESC);

-- High-entropy, single-use codes for when the authenticator is unavailable.
CREATE TABLE mfa_recovery_codes (
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash bytea NOT NULL,
    used_at   timestamptz,
    PRIMARY KEY (user_id, code_hash)
);
