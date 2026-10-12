# Parley (working name)

A minimal, self-hostable alternative to Discord with two features: **text chat** and
**reliable voice chat**, plus friends, friend requests and blocking.

## Architecture

| Component | Tech | Status |
|---|---|---|
| `services/api`: accounts, sessions, friend graph, message history | Go, Postgres | **identity + social done** |
| `services/api` messaging: DMs, group DMs, history, unread | Go, Postgres | **done** |
| `services/realtime`: live events over WebSocket | Go, NATS | **done** (presence pending) |
| `services/voice-control`: call/room lifecycle, SIP + TURN credentials | Go, Asterisk ARI | milestone 4 |
| `voice/kamailio`: SIP edge, NAT fix-up, load balancing | Kamailio | milestone 4 |
| `voice/asterisk`: audio media, ConfBridge group calls | Asterisk (PJSIP) | milestone 4 |
| `voice/coturn`: TURN relay when UDP is blocked | coturn | milestone 5 |
| `client`: desktop app for Windows, macOS and Linux | Tauri (+ pjsip for voice) | **text chat done**; voice next |
| `deploy`: Kubernetes | Helm, CloudNativePG | milestone 6 |

Voice is native SIP/RTP, not WebRTC. Clients only make outbound connections.
Audio is always relayed through the server, and Asterisk replies to the address
it actually receives audio from (symmetric RTP), which avoids one-way audio
behind NAT. A TURN relay over TLS on port 443 is the fallback when a network
blocks UDP.

## Development

Requirements: Go 1.26+, podman (or `CONTAINER=docker`), `pg_isready` (postgres client tools).

```sh
make test        # starts a throwaway Postgres on :55432 and runs every test
make test-unit   # no database; DB tests are skipped
make run-api     # runs the api on :8080 with an ephemeral signing key
                 # (add PARLEY_RECOVERY_EMAIL=log PARLEY_RECOVERY_SMS=log to try password reset)
make dev-nats    # NATS on localhost:4222 for realtime events
make image-api image-realtime   # container images
```

### Running the full stack locally

```sh
make test-db dev-nats
# api: logs its token public key as PARLEY_VERIFY_KEY at startup
PARLEY_DATABASE_URL='postgres://postgres:parley@localhost:55432/parley?sslmode=disable' \
PARLEY_SIGNING_KEY="$(openssl rand -base64 32)" \
PARLEY_NATS_URL=nats://127.0.0.1:4222 \
PARLEY_REALTIME_PUBLIC_URL=ws://<this host>:8081/v1/realtime \
go run ./services/api
# realtime, in another terminal
PARLEY_NATS_URL=nats://127.0.0.1:4222 PARLEY_VERIFY_KEY=<from the api log> go run ./services/realtime
```

Behind a single ingress that routes `/v1/realtime` to the realtime service, leave
`PARLEY_REALTIME_PUBLIC_URL` unset: clients then use `<server>/v1/realtime`.

### Desktop client (`client/`)

Tauri 2: a Rust core (`src-tauri/`) that owns the session and talks to the server,
plus a plain TypeScript UI (`src/`). Access and refresh tokens never reach the
webview. The refresh token is kept in the OS credential store, so you stay logged in.

Linux build dependencies (Fedora):
`sudo dnf install cargo webkit2gtk4.1-devel openssl-devel libappindicator-gtk3-devel librsvg2-devel gcc-c++`
(see the Tauri prerequisites docs for Debian/Ubuntu, macOS and Windows).

End users get a normal installer for their OS. They never need Node, npm or Rust: those are build tools only.

| OS | Installers |
|---|---|
| Linux | `.deb` (Debian/Ubuntu), `.rpm` (Fedora/RHEL), `.AppImage` (any distro, no install) |
| Windows | `Parley_x.y.z_x64-setup.exe` (per-user, no admin rights), `.msi` |
| macOS | universal `.dmg` (Apple Silicon + Intel), macOS 11+ |

**Release builds (all platforms):** push a tag such as `client-v0.1.0`. `.github/workflows/client-release.yml` runs the tests, then builds on Linux, Windows and macOS runners and attaches the installers to a *draft* GitHub release. You can also start it by hand from the Actions tab and download the installers from the run's artifacts. Builds are unsigned until signing secrets are added, so Windows SmartScreen and macOS Gatekeeper will warn on first launch.

**Local builds** (installers for the OS you're on):

```sh
cd client
npm install
npm run tauri dev     # development: run against a local api (server http://localhost:8080)
npm run tauri build   # installers in src-tauri/target/release/bundle/
```

API reference: [`api/openapi.yaml`](api/openapi.yaml). Realtime events: [`api/events.md`](api/events.md).

### api configuration

| Variable | Default | |
|---|---|---|
| `PARLEY_DATABASE_URL` | (required) | Postgres URL; migrations run on startup |
| `PARLEY_SIGNING_KEY` | (required) | base64 32-byte Ed25519 seed (`openssl rand -base64 32`) |
| `PARLEY_LISTEN_ADDR` | `:8080` | |
| `PARLEY_ACCESS_TTL` | `15m` | access token lifetime |
| `PARLEY_REFRESH_TTL` | `720h` | refresh token lifetime |
| `PARLEY_TRUSTED_PROXIES` | (none) | CIDRs (e.g. the ingress) whose `X-Forwarded-For` is trusted for the client IP |

**Login lockout.** By default 5 failed logins from one IP lock that IP out of the account for 5 minutes, and 20 failures from all IPs combined lock the account. Unknown usernames are counted the same way, so the lockout doesn't reveal which accounts exist.

| Variable | Default | |
|---|---|---|
| `PARLEY_LOGIN_MAX_FAILURES` | `5` | `0` disables lockout |
| `PARLEY_LOGIN_LOCKOUT` | `5m` | lock duration |
| `PARLEY_LOGIN_FAILURE_WINDOW` | `15m` | failures older than this are forgotten |
| `PARLEY_LOGIN_LOCKOUT_SCOPE` | `account_ip` | or `account`: counts all failures together. Anyone who knows a username can then keep that account locked |
| `PARLEY_LOGIN_ACCOUNT_MAX_FAILURES` | `20` | account-wide cap in `account_ip` scope (`0` disables) |

**Account recovery (email / SMS).** Off by default. When a channel is off, registration and the reset endpoints reject it and the client hides it.

| Variable | Default | |
|---|---|---|
| `PARLEY_RECOVERY_EMAIL` | `off` | `smtp`, or `log` (development only, see below) |
| `PARLEY_RECOVERY_SMS` | `off` | `twilio`, or `log` (development only) |
| `PARLEY_SMTP_HOST` / `_PORT` / `_USERNAME` / `_PASSWORD` / `_FROM` | port `587` | TLS required |
| `PARLEY_SMTP_TLS` | `starttls` | or `implicit` (port 465) |
| `PARLEY_TWILIO_ACCOUNT_SID` / `_AUTH_TOKEN` / `_FROM` | | `_FROM` is a number or a Messaging Service SID |

> ⚠️ `log` mode writes verification and reset codes to the server log instead of sending them. Use it to test the flows without a mail or SMS provider. Never use it in production: anyone who can read the logs can reset any password. The server prints a warning banner at startup when it's on.

## Layout

```
pkg/            shared Go libraries (auth tokens, db + migrations, events, http helpers)
services/api/       internal/{identity,social,messaging,delivery,httpapi}, migrations/
services/realtime/  WebSocket gateway (NATS subject per user → connections)
api/            OpenAPI spec and realtime event catalogue
```
