# Voice

Calls are native SIP/RTP, not WebRTC:

```
desktop client (pjsip) ──SIP/TLS──► Kamailio :5061 ──SIP/UDP loopback──► FreeSWITCH :5080
        │                              (auth, NAT fix-up)                  │ Event Socket (loopback :8021)
        └────────── SRTP (Opus+FEC / G.722) ───────────► FreeSWITCH RTP     ▼
                                          (10000-10999/udp)          voice-control (Go)
```

- **Ring + drop-in.** Every DM and group has one call room. Clicking call
  asks `voice-control` for short-lived SIP credentials (`POST /v1/calls/{id}/join`)
  and dials `sip:<conversation id>@<server>`. The first person in triggers
  `call.started` (others ring); people join and leave freely; the room closes
  when the last person leaves.
- **Clients never register.** They only make outbound calls when joining, so
  nothing needs to reach into a user's network.
- **SIP over TLS.** Clients (0.4+) connect to Kamailio on 5061 by hostname and
  check its certificate against the OS's trusted CAs, so the SDP, which
  carries the SRTP keys, can't be read or altered on the way; over TLS the
  client also refuses to send SRTP keys any other way. The join request says
  which transports the client has (`{"transports": ["tls", "tcp"]}`) and
  voice-control picks the proxy: TLS when both sides have it, plain TCP on
  5060 only for older clients and only if the server enables it (otherwise
  426, "update Parley").
- **Kamailio** checks the credentials (`auth_ephemeral`: `<expiry>:<user>.<room>`,
  HMAC-SHA1 with a secret shared with voice-control), refuses credentials for a
  different room, replaces any client-supplied `X-Parley-User` with the verified
  user, and relays to FreeSWITCH.
- **FreeSWITCH** accepts SIP only from Kamailio on loopback. Its dialplan parks
  every call; voice-control (subscribed to `CHANNEL_PARK` on the Event Socket)
  re-checks membership and blocks, then moves the call into the room's
  conference (`uuid_transfer … conference:<room>@parley`), or hangs up. Calls not
  admitted within 15 s are dropped. Media always goes through FreeSWITCH, and
  RTP auto-adjust (symmetric RTP) sends audio back to wherever it arrives from,
  so NAT'd clients get audio. SRTP is **mandatory**: plain RTP offers are refused.
- **Opus with in-band FEC** is the preferred codec (G.722 as fallback), and the
  conference mixes at 48 kHz. Everything is open source and redistributable:
  FreeSWITCH (MPL 1.1), sofia-sip and spandsp (LGPL 2.1), libopus (BSD).

## The FreeSWITCH image

`voice/freeswitch/Containerfile` builds FreeSWITCH 1.11.3, sofia-sip 1.13.18 and
spandsp 3.1.1 from source (each pinned by checksum), with only the 9 modules
Parley uses (`voice/freeswitch/modules.conf`). The vanilla demo configuration
(default users, public profiles) is deleted from the image. The config in
`voice/freeswitch/conf` is the only one FreeSWITCH can load.

## Running locally (host networking)

```sh
make image-freeswitch image-kamailio
SECRET=$(openssl rand -hex 24); ESL=$(openssl rand -hex 16); IP=192.168.19.45
podman run -d --name parley-freeswitch --network host -e PARLEY_MEDIA_IP=$IP -e PARLEY_ESL_PASSWORD=$ESL parley/freeswitch:dev
podman run -d --name parley-kamailio  --network host -e PARLEY_SIP_SECRET=$SECRET -e PARLEY_SIP_ADVERTISE=$IP parley/kamailio:dev
PARLEY_DATABASE_URL=... PARLEY_NATS_URL=nats://127.0.0.1:4222 PARLEY_VERIFY_KEY=<from the api log> \
PARLEY_ESL_PASSWORD=$ESL PARLEY_SIP_SECRET=$SECRET PARLEY_SIP_PROXY="sip:$IP:5060;transport=tcp" \
  go run ./services/voice-control
```

Then set `PARLEY_VOICE_PUBLIC_URL=http://$IP:8082` on the api. This runs plain
SIP (no certificate): for TLS, give Kamailio a certificate for a hostname
clients resolve (`PARLEY_SIP_TLS_CERT_DIR` with `tls.crt`/`tls.key`, see
`kamailio/entrypoint.sh`) and voice-control
`PARLEY_SIP_PROXY_TLS="sip:<hostname>:5061;transport=tls"`.

Ports: `5061/tcp` (SIP over TLS) and/or `5060/tcp+udp` (plain SIP), and
`10000-10999/udp` (audio): all must be reachable by clients. `8021` (Event
Socket), `5080` (FreeSWITCH SIP) and `5090` (Kamailio's FreeSWITCH side) stay
on loopback.

Kamailio also rate-limits per IP (more than ~30 requests in 2 s: ignored for
5 minutes), ignores known SIP scanners, and doesn't announce its version.

## Testing without the app

`voice/test/call_test.py` drives real calls with pjsua (pjproject's CLI client,
same version and codecs as the app; `make image-pjsua`). Each participant plays a
different tone and records what it hears. The script checks with a Goertzel
filter that everyone hears the others and not themselves, and that the server's
SDP answer requires SRTP and offers Opus with FEC on the right address. It also
checks that credentials for another room, forged passwords and unencrypted
(plain RTP) calls are refused, that forged audio packets sent to a call's
media port from another address don't redirect its audio ("RTP bleed"), and
that the room closes afterwards. SIP goes over TLS (verifying the server's
certificate) unless `--transport tcp`.

```sh
# A server with SIP over TLS: --host is the media IP, HTTP goes to the hostname.
S="--host 203.0.113.5 --api https://parley.example.org --voice https://parley.example.org --db <postgres URL>"
python3 voice/test/call_test.py $S            # 1:1 DM call
python3 voice/test/call_test.py $S --group    # 3 people, mixed by the conference

# The desktop app's own voice engine as one participant:
(cd client/src-tauri && cargo build --features voiceprobe --bin voiceprobe)
python3 voice/test/call_test.py $S --app client/src-tauri/target/debug/voiceprobe

# The plain-SIP LAN dev server (no certificate):
python3 voice/test/call_test.py --host 192.168.19.45 --transport tcp

# A Helm deployment in a local kind cluster (rootless podman): SIP clients join
# the "kind" network to reach the media node; HTTP goes through the ingress
# (e.g. kubectl -n traefik port-forward svc/traefik 28000:80, host=localhost,
# voice.sip.tls.enabled=false, voice.sip.plain.enabled=true).
python3 voice/test/call_test.py --host <kind node IP> --pjsua-network kind --transport tcp \
  --api http://localhost:28000 --voice http://localhost:28000 --db <postgres URL>
```

## Not done yet

- TURN/ICE fallback for networks that block UDP, and call recovery across network changes (phase 4).
- The desktop client builds pjproject + Opus itself: see `client/scripts/build-voice-deps.*`.
