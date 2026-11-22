-- User-to-user invites ----------------------------------------------------------
-- An invite is an ordinary activation link with the inviter recorded on it.
-- When invited_by is set, the account page lets the invitee choose their own
-- username, and on completion they are made friends with the inviter.
ALTER TABLE account_links
    ADD COLUMN invited_by uuid REFERENCES users (id) ON DELETE SET NULL;

-- Backs the per-inviter daily cap (invites sent in the last 24h).
CREATE INDEX account_links_inviter_idx ON account_links (invited_by, created_at DESC)
    WHERE invited_by IS NOT NULL;
