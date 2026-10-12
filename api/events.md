# Realtime events

Delivered to clients over the `realtime` service's WebSocket (milestone 2) as
`{"type": "...", "data": {...}}`. Source of truth for type names:
`pkg/events/events.go`.

| type | sent to | data |
|---|---|---|
| `friend_request.created` | sender and recipient | `FriendRequest` |
| `friend_request.removed` | both users | `{"id": "<request uuid>"}` (accepted, declined, cancelled or cleared by a block) |
| `friendship.created` | each side separately | `Friend` naming the *other* user |
| `friendship.removed` | each side separately | `{"user_id": "<other user uuid>"}` (removed or blocked) |

Block and unblock themselves are never announced to the blocked user.
