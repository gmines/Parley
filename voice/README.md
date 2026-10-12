# Voice

Calls are native SIP/RTP, not WebRTC:

```
desktop client (pjsip) ──SIP/TCP──► Kamailio :5060 ──SIP/UDP loopback──► Asterisk :5080
        │                              (auth, NAT fix-up)                  │ ARI (loopback :8088)
        └──────────── SRTP (Opus / G.722) ─────────────► Asterisk RTP      ▼
                                          (10000-10999/udp)          voice-control (Go)
```

- **Ring + drop-in.** Every DM and group has one call room. Clicking call
  asks `voice-control` for short-lived SIP credentials (`POST /v1/calls/{id}/join`)
  and dials `sip:<conversation id>@<server>`. The first person in triggers
  `call.started` (others ring); people join and leave freely; the room closes
  when the last person leaves.
- **Clients never register.** They only make outbound calls when joining, so
  nothing needs to reach into a user's network.
- **Kamailio** checks the credentials (`auth_ephemeral`: `<expiry>:<user>.<room>`,
  HMAC-SHA1 with a secret shared with voice-control), refuses credentials for a
  different room, replaces any client-supplied `X-Parley-User` with the verified
  user, and relays to Asterisk.
- **Asterisk** accepts SIP only from Kamailio on loopback and hands every call to
  voice-control over ARI. voice-control re-checks membership and blocks, answers,
  and adds the call to the room's mixing bridge. Media always goes through
  Asterisk (`direct_media=no`), with symmetric RTP so NAT'd clients get audio
  back on the path they send from. Media is SRTP (SDES).

## Opus and its license

Group calls are mixed in Asterisk, which needs to decode Opus. The only
maintained decoder is Sangoma's **closed-source** `codec_opus`.
`voice/asterisk/Containerfile` downloads it (pinned by checksum) when built with
`WITH_OPUS=1` (the default). By building with it you accept Sangoma's license:

- Hosting a service for others **is allowed** (the license's section 2.1 exempts "Opus for Asterisk").
- It **may not be redistributed**: every operator must build their own image.
  Never publish an image built with `WITH_OPUS=1`.
- It **reports anonymous usage** to a Sangoma server once a day (a random server
  id, a timestamp, the peak number of Opus streams). If the report fails, the codec
  keeps working, so you can block it at your firewall.
- It's available for x86-64 only.

With `--build-arg WITH_OPUS=0` calls use G.722 (16 kHz, fully open source, no
forward error correction).

## Running locally (host networking)

```sh
make image-asterisk image-kamailio
SECRET=$(openssl rand -hex 24); ARI=$(openssl rand -hex 16); IP=192.168.19.45
podman run -d --name parley-asterisk --network host -e PARLEY_MEDIA_IP=$IP -e PARLEY_ARI_PASSWORD=$ARI parley/asterisk:dev
podman run -d --name parley-kamailio --network host -e PARLEY_SIP_SECRET=$SECRET -e PARLEY_SIP_ADVERTISE=$IP parley/kamailio:dev
PARLEY_DATABASE_URL=... PARLEY_NATS_URL=nats://127.0.0.1:4222 PARLEY_VERIFY_KEY=<from the api log> \
PARLEY_ARI_PASSWORD=$ARI PARLEY_SIP_SECRET=$SECRET PARLEY_SIP_PROXY="sip:$IP:5060;transport=tcp" \
  go run ./services/voice-control
```

Then set `PARLEY_VOICE_PUBLIC_URL=http://$IP:8082` on the api.

Ports: `5060/tcp+udp` (SIP, must be reachable by clients), `10000-10999/udp`
(audio, must be reachable by clients). `8088` (ARI) and `5080` (Asterisk SIP)
stay on loopback.

## Testing without the app

`voice/test/call_test.py` drives real calls with pjsua (pjproject's CLI client,
same version and codecs as the app; `make image-pjsua`). Each participant plays a
different tone and records what it hears. The script checks with a Goertzel
filter that everyone hears the others and not themselves. It also checks that
credentials for another room and forged passwords are refused, and that the
room closes afterwards.

```sh
python3 voice/test/call_test.py --host 192.168.19.45          # 1:1 DM call
python3 voice/test/call_test.py --host 192.168.19.45 --group  # 3 people: Asterisk mixes (softmix + Opus)
```

## Not done yet

- SIP over TLS (`5061`/`443`): signaling is TCP for now, so SRTP keys travel in clear SDP.
- TURN/ICE fallback for networks that block UDP, and call recovery across network changes (phase 4).
- The desktop client side (phase 2: pjsip in the Tauri app).
