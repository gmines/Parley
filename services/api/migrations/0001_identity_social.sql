-- Identity ------------------------------------------------------------------

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text NOT NULL,
    display_name  text NOT NULL,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_username_format CHECK (username ~ '^[a-z0-9_.]{2,32}$')
);
CREATE UNIQUE INDEX users_username_key ON users (username);

-- Refresh tokens rotate on every use. All tokens descended from one login share
-- a family_id; presenting an already-used token revokes the whole family
-- (stolen-token detection).
CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id   uuid NOT NULL,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  bytea NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

-- Social graph ----------------------------------------------------------------

-- Rows exist only while a request is pending; accept/decline/cancel delete them.
CREATE TABLE friend_requests (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    from_user  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    to_user    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT friend_requests_pair_key UNIQUE (from_user, to_user),
    CONSTRAINT friend_requests_not_self CHECK (from_user <> to_user)
);
CREATE INDEX friend_requests_to_user_idx ON friend_requests (to_user);

-- One row per friendship, stored with user_a < user_b.
CREATE TABLE friendships (
    user_a     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    user_b     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_a, user_b),
    CONSTRAINT friendships_ordered CHECK (user_a < user_b)
);
CREATE INDEX friendships_user_b_idx ON friendships (user_b);

CREATE TABLE blocks (
    blocker    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    blocked    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (blocker, blocked),
    CONSTRAINT blocks_not_self CHECK (blocker <> blocked)
);
CREATE INDEX blocks_blocked_idx ON blocks (blocked);
