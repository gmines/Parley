//! Test tool: joins a call with the desktop app's voice engine (the same
//! shim + pjsua build the app ships), with no sound card: it plays a WAV
//! file into the call and records what it hears. voice/test/call_test.py
//! uses it as one participant (`--app`).
//!
//!   voiceprobe --creds '<join response "sip" JSON>' --identity <user id> \
//!              --play in.wav --record out.wav --seconds 10

use std::sync::mpsc;
use std::time::{Duration, Instant};

use parley_client_lib::voice_engine::{CallState, Engine, EngineEvent, SipCredentials};

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
