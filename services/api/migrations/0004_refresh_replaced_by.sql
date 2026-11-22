-- The token a refresh token was exchanged for, so a client that never got
-- the response can retry with the old one (see identity.Refresh).
ALTER TABLE refresh_tokens ADD COLUMN replaced_by uuid;
