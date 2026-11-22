-- Admin app -----------------------------------------------------------------------

-- Per-user MFA policy. 'inherit' follows the server-wide setting below;
-- 'required' always enforces enrollment; 'exempt' never does (the user may
-- still enroll). A disabled account can't log in or refresh.
ALTER TABLE users
    ADD COLUMN mfa_policy  text NOT NULL DEFAULT 'inherit'
        CONSTRAINT users_mfa_policy_check CHECK (mfa_policy IN ('inherit', 'required', 'exempt')),
    ADD COLUMN disabled_at timestamptz;

-- Settings an operator can change at runtime. One row per group; a group
-- without a row uses the deployment's environment. Secrets inside a value are
-- sealed with a key derived from PARLEY_MFA_KEY.
CREATE TABLE server_settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text NOT NULL DEFAULT ''
);
-- MFA stays mandatory for everyone until an operator says otherwise.
INSERT INTO server_settings (key, value, updated_by) VALUES ('mfa', '{"require": true}', 'migration');

-- An admin can create a reset link to hand to the user directly, for accounts
-- without a verified email or phone.
ALTER TABLE account_links DROP CONSTRAINT account_links_channel_check;
ALTER TABLE account_links ADD CONSTRAINT account_links_channel_check CHECK (channel IN ('email', 'sms', 'admin'));

-- Operators. Separate from users: a chat account can never reach the admin
-- app, and admins can't be found or messaged.
CREATE TABLE admin_users (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username       text NOT NULL,
    password_hash  text NOT NULL,
    totp_secret    bytea NOT NULL,           -- AES-GCM sealed
    totp_last_step bigint NOT NULL DEFAULT 0, -- newest accepted step (no replays)
    created_at     timestamptz NOT NULL DEFAULT now(),
    disabled_at    timestamptz,
    CONSTRAINT admin_users_username_format CHECK (username ~ '^[a-z0-9_.]{2,32}$')
);
CREATE UNIQUE INDEX admin_users_username_key ON admin_users (username);

-- Server-side sessions; the cookie holds a random token, only its hash is here.
CREATE TABLE admin_sessions (
    token_hash   bytea PRIMARY KEY,
    admin_id     uuid NOT NULL REFERENCES admin_users (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    ip           text NOT NULL DEFAULT ''
);
CREATE INDEX admin_sessions_admin_idx ON admin_sessions (admin_id);

-- Every admin action and sign-in attempt. Never updated or deleted by the app.
CREATE TABLE admin_audit_log (
    id     bigserial PRIMARY KEY,
    at     timestamptz NOT NULL DEFAULT now(),
    admin  text NOT NULL,
    action text NOT NULL,
    target text NOT NULL DEFAULT '',
    detail jsonb NOT NULL DEFAULT '{}',
    ip     text NOT NULL DEFAULT ''
);
CREATE INDEX admin_audit_log_at_idx ON admin_audit_log (at DESC);
