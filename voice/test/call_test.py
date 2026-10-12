#!/usr/bin/env python3
"""End-to-end voice test: two pjsua clients join one call through
Kamailio -> FreeSWITCH (voice-control admits them over the Event Socket), each plays a
different tone, and we check that each side heard the other's tone.

Needs the stack running (api, voice-control, Kamailio, FreeSWITCH) and the
parley/pjsua:dev image (voice/test/pjsua.Containerfile). Creates two
throwaway users and deletes them afterwards (via psql).

    python3 voice/test/call_test.py --host 192.168.19.45
"""
import argparse
import json
import math
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import wave

PASSWORD = "plum-orbit-candle-42"


def http(method, url, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"null")


def tone_power(path, freq, skip_s=3.0, window_s=4.0):
    """Goertzel power of `freq` in a window of a 16-bit mono/stereo WAV."""
    with wave.open(path) as w:
        rate, ch, width = w.getframerate(), w.getnchannels(), w.getsampwidth()
        assert width == 2, "expected 16-bit audio"
        w.setpos(min(int(skip_s * rate), w.getnframes()))
        frames = w.readframes(int(window_s * rate))
    samples = [int.from_bytes(frames[i:i + 2], "little", signed=True) for i in range(0, len(frames), 2 * ch)]
    if not samples:
        return 0.0
    k = 2 * math.cos(2 * math.pi * freq / rate)
    s1 = s2 = 0.0
    for x in samples:
        s1, s2 = x + k * s1 - s2, s1
    return (s1 * s1 + s2 * s2 - k * s1 * s2) / len(samples)


def sdp_answer(log):
    """The SDP body of the 200 OK to our INVITE, from a pjsua log."""
    out, inside = [], False
    for line in log.splitlines():
        if "Response msg 200/INVITE" in line:
            inside = True
        elif inside and line.startswith("--end msg--"):
            break
        elif inside:
            out.append(line)
    return "\n".join(out)


def pjsua(name, workdir, creds, local_port, rtp_port, play, rec, target, seconds, srtp=True):
    """Starts pjsua in a container joining `target`; returns the Popen."""
    args = [
        "--null-audio", "--no-udp", "--local-port", str(local_port), "--rtp-port", str(rtp_port),
        "--id", f"sip:{creds['username'].split(':')[1].split('.')[0]}@parley",
        "--outbound", creds["proxy"] + ";lr",
        "--realm", creds["realm"], "--username", creds["username"], "--password", creds["password"],
        "--use-srtp", "2" if srtp else "0", "--srtp-secure", "0", "--add-codec", "opus",
        "--max-calls", "1", "--log-level", "4", "--app-log-level", "4",
    ]
    if play:
        args += ["--play-file", f"/work/{play}", "--auto-play"]  # the player loops the file by default
    if rec:
        args += ["--rec-file", f"/work/{rec}", "--auto-rec"]
    args.append(target)
    quoted = " ".join("'" + a.replace("'", "'\\''") + "'" for a in args)
    script = f"(sleep {seconds}; echo h; sleep 1; echo q) | pjsua {quoted} > /work/{name}.log 2>&1"
    return subprocess.Popen(["podman", "run", "--rm", "--network", "host", "-v", f"{workdir}:/work:Z",
                             "--entrypoint", "sh", "parley/pjsua:dev", "-c", script])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="192.168.19.45")
    ap.add_argument("--db", default="postgres://postgres:parley@localhost:55432/parley?sslmode=disable")
    ap.add_argument("--group", action="store_true", help="3-person group call instead of a DM (exercises the conference mixer)")
    ap.add_argument("--app", metavar="VOICEPROBE", help="use the desktop app's voice engine (path to the voiceprobe binary) for alice instead of pjsua")
    opt = ap.parse_args()
    if opt.group:
        return group_call(opt)
    api, voice = f"http://{opt.host}:18080", f"http://{opt.host}:18082"
    ok = True

    def check(cond, msg):
        nonlocal ok
        print(("PASS " if cond else "FAIL ") + msg)
        ok = ok and cond

    suffix = str(int(time.time()) % 1000000)
    names = [f"calla{suffix}", f"callb{suffix}"]
    workdir = tempfile.mkdtemp(prefix="parley-call-")
    os.chmod(workdir, 0o777)
    try:
        sess = []
        for n in names:
            st, s = http("POST", api + "/v1/auth/register", body={"username": n, "password": PASSWORD})
            assert st == 201, (st, s)
            sess.append(s)
        a, b = sess
        st, req = http("POST", api + "/v1/friends/requests", a["access_token"], {"username": names[1]})
        http("POST", api + f"/v1/friends/requests/{req['request']['id']}/accept", b["access_token"])
        st, conv = http("POST", api + "/v1/conversations/dm", a["access_token"], {"user_id": b["user"]["id"]})
        conv_id = conv["id"]

        st, ja = http("POST", voice + f"/v1/calls/{conv_id}/join", a["access_token"])
        st2, jb = http("POST", voice + f"/v1/calls/{conv_id}/join", b["access_token"])
        check(st == 200 and st2 == 200 and ja["call"] is None, "voice-control issues join credentials; no call yet")

        st, _ = http("POST", voice + f"/v1/calls/{conv_id}/join")
        check(st == 401, "join without a token is refused")

        # Tones: alice 440 Hz, bob 880 Hz.
        for f, hz in (("a.wav", 440), ("b.wav", 880)):
            subprocess.run(["podman", "run", "--rm", "-v", f"{workdir}:/work:Z", "--entrypoint", "sox", "parley/pjsua:dev",
                            "-n", "-r", "16000", "-c", "1", "-b", "16", f"/work/{f}", "synth", "30", "sine", str(hz), "vol", "0.5"],
                           check=True)

        target = f"sip:{conv_id}@{opt.host}"
        if opt.app:
            # Alice is the desktop app's engine (shim + pjsua), not the pjsua CLI.
            pa = subprocess.Popen([opt.app, "--creds", json.dumps(ja["sip"]), "--identity", a["user"]["id"],
                                   "--play", os.path.join(workdir, "a.wav"), "--record", os.path.join(workdir, "a-rec.wav"),
                                   "--seconds", "13"], stdout=open(os.path.join(workdir, "alice.log"), "w"), stderr=subprocess.STDOUT)
        else:
            pa = pjsua("alice", workdir, ja["sip"], 5070, 40000, "a.wav", "a-rec.wav", target, 14)
        time.sleep(3)
        pb = pjsua("bob", workdir, jb["sip"], 5072, 40100, "b.wav", "b-rec.wav", target, 10)
        time.sleep(5)
        st, fresh = http("POST", api + "/v1/auth/login", body={"username": names[0], "password": PASSWORD})
        st, calls = http("GET", voice + "/v1/calls", fresh["access_token"])
        parts = calls["calls"][0]["participants"] if calls and calls.get("calls") else []
        check(sorted(parts) == sorted([a["user"]["id"], b["user"]["id"]]), f"both users are in the call ({len(parts)} participants)")

        # Attacks while the call runs: alice's credentials for another room,
        # and a forged password, must both be refused by Kamailio.
        other_room = f"sip:00000000-0000-4000-8000-000000000000@{opt.host}"
        p1 = pjsua("wrongroom", workdir, ja["sip"], 5074, 40200, None, None, other_room, 4)
        bad = dict(jb["sip"], password="AAAAAAAAAAAAAAAAAAAAAAAAAAA=")
        p2 = pjsua("badpass", workdir, bad, 5076, 40300, None, None, target, 4)
        # A client offering unencrypted audio must be refused, not downgraded.
        p3 = pjsua("plainrtp", workdir, ja["sip"], 5078, 40400, None, None, target, 4, srtp=False)
        for p in (pa, pb, p1, p2, p3):
            p.wait(timeout=60)

        logs = {n: open(os.path.join(workdir, n + ".log"), errors="replace").read() for n in ("alice", "bob", "wrongroom", "badpass", "plainrtp")}
        check("CONFIRMED" not in logs["plainrtp"], "a client offering unencrypted audio (plain RTP) is refused")
        check("403" in logs["wrongroom"] and "CONFIRMED" not in logs["wrongroom"], "credentials for one room can't open another (403)")
        check("CONFIRMED" not in logs["badpass"] and ("407" in logs["badpass"] or "401" in logs["badpass"]), "a forged password is rejected")
        alice_ok = ("call Connected" in logs["alice"]) if opt.app else ("CONFIRMED" in logs["alice"])
        check(alice_ok and "CONFIRMED" in logs["bob"], "both calls were answered" + (" (alice = desktop app engine)" if opt.app else ""))
        if opt.app:
            stats_line = next((l for l in logs["alice"].splitlines() if l.startswith("stats {")), "")
            st_ = json.loads(stats_line[6:]) if stats_line else {}
            print(f"     app engine stats: {st_}")
            check(st_.get("encrypted") is True and st_.get("codec", "").lower() == "opus", "the app's call is SRTP-encrypted Opus (from its own stats)")
        answer = sdp_answer(logs["bob"])
        check("RTP/SAVP" in answer and "a=crypto:" in answer, "the server's SDP answer requires SRTP (RTP/SAVP + a=crypto)")
        check("opus/48000" in answer and "useinbandfec=1" in answer, "the server answered Opus with in-band FEC")
        check(f"c=IN IP4 {opt.host}" in answer, f"the server advertises {opt.host} for media")

        heard_by_b = (tone_power(os.path.join(workdir, "b-rec.wav"), 440), tone_power(os.path.join(workdir, "b-rec.wav"), 880))
        heard_by_a = (tone_power(os.path.join(workdir, "a-rec.wav"), 880), tone_power(os.path.join(workdir, "a-rec.wav"), 440))
        print(f"     bob's recording:   440 Hz power {heard_by_b[0]:.3g}, 880 Hz (own tone) {heard_by_b[1]:.3g}")
        print(f"     alice's recording: 880 Hz power {heard_by_a[0]:.3g}, 440 Hz (own tone) {heard_by_a[1]:.3g}")
        check(heard_by_b[0] > 1e6 and heard_by_b[0] > 20 * heard_by_b[1], "bob hears alice (440 Hz) and not his own echo")
        check(heard_by_a[0] > 1e6 and heard_by_a[0] > 20 * heard_by_a[1], "alice hears bob (880 Hz) and not her own echo")

        time.sleep(1)
        # Access tokens may be short-lived (the dev api uses 5s): log in again.
        st, fresh = http("POST", api + "/v1/auth/login", body={"username": names[0], "password": PASSWORD})
        st, calls = http("GET", voice + "/v1/calls", fresh["access_token"])
        check(st == 200 and calls["calls"] == [], "the call ends when everyone hangs up")
    finally:
        for n in names:
            subprocess.run(["psql", "-X", "-q", opt.db,
                            "-c", f"DELETE FROM conversations c USING conversation_members m JOIN users u ON u.id=m.user_id WHERE m.conversation_id=c.id AND u.username='{n}'",
                            "-c", f"DELETE FROM users WHERE username='{n}'"], check=False)
        if os.environ.get("KEEP"):
            print("kept recordings and logs in", workdir)
        else:
            shutil.rmtree(workdir, ignore_errors=True)
    print("ALL PASSED" if ok else "SOME CHECKS FAILED")
    sys.exit(0 if ok else 1)


def group_call(opt):
    """Three people in a group call: FreeSWITCH's conference must decode, mix
    and re-encode everyone's Opus. Each must hear both others."""
    api, voice = f"http://{opt.host}:18080", f"http://{opt.host}:18082"
    ok = True

    def check(cond, msg):
        nonlocal ok
        print(("PASS " if cond else "FAIL ") + msg)
        ok = ok and cond

    suffix = str(int(time.time()) % 1000000)
    people = [("alice", 440), ("bob", 880), ("carol", 660)]
    names = [f"grp{p[0]}{suffix}" for p in people]
    workdir = tempfile.mkdtemp(prefix="parley-group-")
    os.chmod(workdir, 0o777)
    try:
        sess = []
        for n in names:
            st, s_ = http("POST", api + "/v1/auth/register", body={"username": n, "password": PASSWORD})
            assert st == 201, (st, s_)
            sess.append(s_)
        for i in (1, 2):
            st, req = http("POST", api + "/v1/friends/requests", sess[0]["access_token"], {"username": names[i]})
            http("POST", api + f"/v1/friends/requests/{req['request']['id']}/accept", sess[i]["access_token"])
        st, conv = http("POST", api + "/v1/conversations/group", sess[0]["access_token"],
                        {"user_ids": [sess[1]["user"]["id"], sess[2]["user"]["id"]], "name": "voice test"})
        assert st == 201, (st, conv)
        creds = []
        for s_ in sess:
            st, j = http("POST", voice + f"/v1/calls/{conv['id']}/join", s_["access_token"])
            assert st == 200, (st, j)
            creds.append(j["sip"])
        for (who, hz) in people:
            subprocess.run(["podman", "run", "--rm", "-v", f"{workdir}:/work:Z", "--entrypoint", "sox", "parley/pjsua:dev",
                            "-n", "-r", "16000", "-c", "1", "-b", "16", f"/work/{who}.wav", "synth", "30", "sine", str(hz), "vol", "0.3"],
                           check=True)
        target = f"sip:{conv['id']}@{opt.host}"
        procs = []
        for i, (who, _) in enumerate(people):
            procs.append(pjsua(who, workdir, creds[i], 5070 + 2 * i, 40000 + 100 * i, f"{who}.wav", f"{who}-rec.wav", target, 14 - 2 * i))
            time.sleep(2)
        for p in procs:
            p.wait(timeout=60)
        for i, (who, own) in enumerate(people):
            rec = os.path.join(workdir, f"{who}-rec.wav")
            others = [hz for (w, hz) in people if w != who]
            # Window after everyone has joined (carol joins ~4s after alice).
            skip = 5.0 - 2 * i
            p_others = [tone_power(rec, hz, skip_s=skip, window_s=3) for hz in others]
            p_own = tone_power(rec, own, skip_s=skip, window_s=3)
            print(f"     {who}: others {', '.join(f'{hz} Hz {p:.2g}' for hz, p in zip(others, p_others))}; own {own} Hz {p_own:.2g}")
            check(min(p_others) > 1e6 and min(p_others) > 20 * p_own, f"{who} hears both others and not themselves")
        alog = open(os.path.join(workdir, "alice.log"), errors="replace").read().lower()
        check("opus" in alog, "the call negotiated Opus")
    finally:
        for n in names:
            subprocess.run(["psql", "-X", "-q", opt.db,
                            "-c", f"DELETE FROM conversations c USING conversation_members m JOIN users u ON u.id=m.user_id WHERE m.conversation_id=c.id AND u.username='{n}'",
                            "-c", f"DELETE FROM users WHERE username='{n}'"], check=False)
        if os.environ.get("KEEP"):
            print("kept recordings and logs in", workdir)
        else:
            shutil.rmtree(workdir, ignore_errors=True)
    print("ALL PASSED" if ok else "SOME CHECKS FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
