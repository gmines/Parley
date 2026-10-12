-- Conversations: 1:1 DMs and small group DMs ----------------------------------

CREATE TABLE conversations (
    id              uuid PRIMARY KEY,
    kind            text NOT NULL CHECK (kind IN ('dm', 'group')),
    name            text,                -- groups only; optional
    dm_key          text UNIQUE,         -- "<smaller uuid>:<larger uuid>" for DMs: one DM per pair
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_message_at timestamptz,
    CONSTRAINT conversations_dm_key CHECK ((kind = 'dm') = (dm_key IS NOT NULL))
);

CREATE TABLE conversation_members (
    conversation_id      uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    user_id              uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    joined_at            timestamptz NOT NULL DEFAULT now(),
    last_read_message_id uuid,           -- unread = messages after this one
    PRIMARY KEY (conversation_id, user_id)
);
CREATE INDEX conversation_members_user_idx ON conversation_members (user_id);

-- Message ids are UUIDv7, so ordering by id is ordering by time; clients page
-- with before/after=<id> and resync after reconnecting with after=<last seen>.
CREATE TABLE messages (
    id              uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    author_id       uuid REFERENCES users (id) ON DELETE SET NULL,
    content         text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 4000),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_conversation_idx ON messages (conversation_id, id);
