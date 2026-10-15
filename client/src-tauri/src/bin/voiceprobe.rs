//! Test tool: joins a call with the desktop app's voice engine (the same
//! shim + pjsua build the app ships), with no sound card: it plays a WAV
//! file into the call and records what it hears. voice/test/call_test.py
//! uses it as one participant (`--app`).
//!
//! Build: cargo build --features voiceprobe --bin voiceprobe
//!
//!   voiceprobe --creds '<join response "sip" JSON>' --identity <user id> \
//!              --play in.wav --record out.wav --seconds 10
//!   voiceprobe --selftest        (echo canceller the app uses, and SIP over TLS)
//!   voiceprobe --list-devices
//!   voiceprobe --tone ring|ringback --seconds 5   (plays a ring tone on the default output)

use std::sync::mpsc;
use std::time::{Duration, Instant};

use parley_client_lib::voice_engine::{CallState, Engine, EngineEvent, SipCredentials, Tone};

fn arg(name: &str) -> Option<String> {
    let mut args = std::env::args();
    while let Some(a) = args.next() {
        if a == name {
            return args.next();
        }
    }
    None
}

fn main() {
    // Run the echo canceller at the app's settings on synthetic audio (a null
    // sound device skips echo cancellation entirely, so call tests can't
    // catch problems in it), and check SIP over TLS is available.
    if std::env::args().any(|a| a == "--selftest" || a == "--ec-selftest") {
        let engine = Engine::start(true, 1, |_| {}).expect("start engine");
        let result = engine.ec_selftest();
        let transports = engine.transports();
        engine.shutdown();
        let mut ok = true;
        match result {
            Ok(algo) => println!("echo canceller ok: {algo}"),
            Err(e) => {
                eprintln!("echo canceller failed: {e}");
                ok = false;
            }
        }
        if transports.contains(&"tls") {
            println!("SIP transports: {}", transports.join(", "));
        } else {
            eprintln!("SIP over TLS unavailable (transports: {})", transports.join(", "));
            ok = false;
        }
        std::process::exit(if ok { 0 } else { 1 });
    }
    // Play a ring tone on the real default output for a while.
    if let Some(kind) = arg("--tone") {
        let tone = match kind.as_str() {
            "ring" => Tone::Ring,
            "ringback" => Tone::Ringback,
            other => panic!("unknown tone {other}"),
        };
        let secs: u64 = arg("--seconds").and_then(|s| s.parse().ok()).unwrap_or(5);
        let engine = Engine::start(false, 1, |_| {}).expect("start engine");
        let result = engine.tone(Some(tone), None);
        if result.is_ok() {
            std::thread::sleep(Duration::from_secs(secs));
            engine.tone(None, None).expect("stop tone");
        }
        engine.shutdown();
        match result {
            Ok(()) => println!("played {kind} for {secs} s"),
            Err(e) => {
                eprintln!("ring tone failed: {e}");
                std::process::exit(1);
            }
        }
        return;
    }
    // List the sound devices the engine sees (real audio backend), then exit.
    if std::env::args().any(|a| a == "--list-devices") {
        let engine = Engine::start(false, 1, |_| {}).expect("start engine");
        for d in engine.devices() {
            println!("{:>3}  in:{} out:{}  {}", d.index, d.inputs, d.outputs, d.name);
        }
        engine.shutdown();
        return;
    }
    let creds: SipCredentials = serde_json::from_str(&arg("--creds").expect("--creds")).expect("creds JSON");
    let identity = arg("--identity").expect("--identity");
    let seconds: u64 = arg("--seconds").map_or(10, |s| s.parse().expect("--seconds"));
    let verbose = std::env::var("VOICEPROBE_LOG").is_ok();

    let (tx, rx) = mpsc::channel();
    let engine = Engine::start(true, if verbose { 4 } else { 1 }, move |ev| {
        if let EngineEvent::Log { text, .. } = &ev {
            if verbose {
                eprintln!("pj: {text}");
            }
            return;
        }
        let _ = tx.send(ev);
    })
    .expect("start engine");

    if let Some(p) = arg("--play") {
        engine.test_play(&p).expect("play file");
    }
    if let Some(r) = arg("--record") {
        engine.test_record(&r).expect("record file");
    }
    engine.join(&creds, &identity).expect("join");

    let wait_for = |want: CallState, timeout: Duration| -> Result<(), String> {
        let deadline = Instant::now() + timeout;
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            match rx.recv_timeout(left) {
                Ok(EngineEvent::Call { state, status, reason }) => {
                    println!("call {state:?} {status} {reason}");
                    if state == want {
                        return Ok(());
                    }
                    if state == CallState::Ended {
                        return Err(format!("call ended: {status} {reason}"));
                    }
                }
                Ok(EngineEvent::Media { active }) => println!("media active={active}"),
                Ok(_) => {}
                Err(_) => return Err(format!("timed out waiting for {want:?}")),
            }
        }
    };

    if let Err(e) = wait_for(CallState::Connected, Duration::from_secs(15)) {
        println!("FAILED: {e}");
        engine.shutdown();
        std::process::exit(2);
    }
    std::thread::sleep(Duration::from_secs(seconds));
    match engine.stats() {
        Some(s) => println!("stats {}", serde_json::to_string(&s).unwrap()),
        None => println!("stats none"),
    }
    engine.leave();
    let _ = wait_for(CallState::Ended, Duration::from_secs(5));
    engine.shutdown();
}
