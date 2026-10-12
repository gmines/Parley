# Realtime events

Delivered over the `realtime` service's WebSocket as `{"type": "...", "data": {...}}`.
Connect to the URL from `GET /v1/meta` (`realtime_url`) with
`Authorization: Bearer <access token>`. The client sends nothing; the server
pings every 25s.

Events are not replayed. The first message on every connection is `ready`;
treat it as "resync": reload the conversation list and fetch
`GET /v1/conversations/{id}/messages?after=<last seen id>` for open
conversations. Source of truth for type names: `pkg/events/events.go`.

| type | sent to | data |
|---|---|---|
| `ready` | the connecting client | `{"user_id": "<uuid>"}` |
| `message.created` | every conversation member | `Message` (plus the sender's `nonce`) |
| `conversation.created` | every member | `Conversation` (unread fields empty) |
| `conversation.read` | the reader's own connections | `{"conversation_id", "last_read_message_id"}` |
| `friend_request.created` | sender and recipient | `FriendRequest` |
| `friend_request.removed` | both users | `{"id": "<request uuid>"}` (accepted, declined, cancelled or cleared by a block) |
| `friendship.created` | each side separately | `Friend` naming the *other* user |
| `friendship.removed` | each side separately | `{"user_id": "<other user uuid>"}` (removed or blocked) |

Block and unblock themselves are never announced to the blocked user.
