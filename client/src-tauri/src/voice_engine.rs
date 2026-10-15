//! Safe wrapper around the voice shim (voice/parley_voice.c), which drives
//! pjsua (pjproject's SIP client). Independent of Tauri so it can be exercised
//! by `src/bin/voiceprobe.rs` against a real server.
//!
//! pjsua is process-global, so there is one [`Engine`], created by
//! [`Engine::start`]. It handles one call at a time.

use std::ffi::{c_char, c_int, c_void, CStr, CString};
use std::sync::OnceLock;

use serde::{Deserialize, Serialize};

#[repr(C)]
struct PvOptions {
    log_level: c_int,
    null_audio: c_int,
    user_agent: *const c_char,
}

#[repr(C)]
struct PvJoinParams {
    room_uri: *const c_char,
    proxy: *const c_char,
    realm: *const c_char,
    username: *const c_char,
    password: *const c_char,
    identity: *const c_char,
}

#[repr(C)]
#[derive(Default)]
struct PvStats {
    codec: [c_char; 32],
    clock_rate: u32,
    rtt_ms: u32,
    rx_jitter_ms: u32,
    rx_packets: u32,
    rx_lost: u32,
    tx_packets: u32,
    tx_lost: u32,
    duration_s: u32,
    srtp: c_int,
}

#[repr(C)]
struct PvDevice {
    name: [c_char; 128],
    inputs: c_int,
    outputs: c_int,
}

type EventCb = extern "C" fn(ctx: *mut c_void, kind: c_int, a: c_int, b: c_int, text: *const c_char);

extern "C" {
    fn pv_init(opt: *const PvOptions, cb: EventCb, ctx: *mut c_void) -> c_int;
    fn pv_shutdown();
    fn pv_has_tls() -> c_int;
    fn pv_tone(kind: c_int, playback: c_int) -> c_int;
    fn pv_join(p: *const PvJoinParams) -> c_int;
    fn pv_leave();
    fn pv_set_mute(muted: c_int);
    fn pv_set_deaf(deaf: c_int);
    fn pv_get_stats(out: *mut PvStats) -> c_int;
    fn pv_device_count() -> c_int;
    fn pv_device_info(index: c_int, out: *mut PvDevice) -> c_int;
    fn pv_set_devices(capture: c_int, playback: c_int) -> c_int;
    fn pv_test_play(path: *const c_char) -> c_int;
    fn pv_test_record(path: *const c_char) -> c_int;
    fn pv_ec_selftest(algo: *mut c_char, algo_len: c_int) -> c_int;
}

/// Ring tones ([`Engine::tone`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Tone {
    /// We're calling: waiting for someone to answer.
    Ringback,
    /// Incoming call.
    Ring,
}

/// Credentials from voice-control's join endpoint.
#[derive(Debug, Clone, Deserialize)]
pub struct SipCredentials {
    pub uri: String,
    pub proxy: String,
    pub realm: String,
    pub username: String,
    pub password: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum CallState {
    Idle,
    Calling,
    Connecting,
    Connected,
    Ended,
}

#[derive(Debug, Clone)]
pub enum EngineEvent {
    Log { level: i32, text: String },
    Call { state: CallState, status: i32, reason: String },
    Media { active: bool },
}

/// Live call quality, for the call bar.
#[derive(Debug, Clone, Serialize)]
pub struct Stats {
    pub codec: String,
    pub clock_rate: u32,
    pub rtt_ms: u32,
    pub jitter_ms: u32,
    pub rx_packets: u32,
    pub rx_lost: u32,
    pub tx_packets: u32,
    pub tx_lost: u32,
    pub duration_s: u32,
    pub encrypted: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct Device {
    pub index: i32,
    pub name: String,
    pub inputs: i32,
    pub outputs: i32,
}

type Sink = Box<dyn Fn(EngineEvent) + Send + Sync>;
static SINK: OnceLock<Sink> = OnceLock::new();
static ENGINE: OnceLock<Engine> = OnceLock::new();

extern "C" fn on_event(_ctx: *mut c_void, kind: c_int, a: c_int, b: c_int, text: *const c_char) {
    let Some(sink) = SINK.get() else { return };
    let text = if text.is_null() { String::new() } else { unsafe { CStr::from_ptr(text) }.to_string_lossy().into_owned() };
    let ev = match kind {
        0 => EngineEvent::Log { level: a, text },
        1 => EngineEvent::Call {
            state: match a {
                1 => CallState::Calling,
                2 => CallState::Connecting,
                3 => CallState::Connected,
                4 => CallState::Ended,
                _ => CallState::Idle,
            },
            status: b,
            reason: text,
        },
        2 => EngineEvent::Media { active: a != 0 },
        _ => return,
    };
    // Never let a panic unwind into C.
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| sink(ev)));
}

pub struct Engine(());

impl Engine {
    /// Starts the SIP stack (once per process); later calls return the same
    /// engine. `sink` receives every event, on pjsua's threads.
    pub fn start(null_audio: bool, log_level: i32, sink: impl Fn(EngineEvent) + Send + Sync + 'static) -> Result<&'static Engine, String> {
        if let Some(e) = ENGINE.get() {
            return Ok(e);
        }
        let _ = SINK.set(Box::new(sink));
        let ua = CString::new(concat!("Parley/", env!("CARGO_PKG_VERSION"))).unwrap();
        let opt = PvOptions { log_level, null_audio: null_audio as c_int, user_agent: ua.as_ptr() };
        let rc = unsafe { pv_init(&opt, on_event, std::ptr::null_mut()) };
        if rc != 0 {
            return Err(format!("voice engine failed to start (code {rc})"));
        }
        Ok(ENGINE.get_or_init(|| Engine(())))
    }

    pub fn get() -> Option<&'static Engine> {
        ENGINE.get()
    }

    /// Joins the room the credentials are for. The result arrives as
    /// [`EngineEvent::Call`] events.
    pub fn join(&self, creds: &SipCredentials, identity: &str) -> Result<(), String> {
        let c = |s: &str| CString::new(s).map_err(|_| "credentials contain a NUL byte".to_string());
        let (uri, proxy, realm, user, pass, id) =
            (c(&creds.uri)?, c(&creds.proxy)?, c(&creds.realm)?, c(&creds.username)?, c(&creds.password)?, c(identity)?);
        let p = PvJoinParams {
            room_uri: uri.as_ptr(),
            proxy: proxy.as_ptr(),
            realm: realm.as_ptr(),
            username: user.as_ptr(),
            password: pass.as_ptr(),
            identity: id.as_ptr(),
        };
        match unsafe { pv_join(&p) } {
            0 => Ok(()),
            -2 => Err("already in a call".into()),
            rc => Err(format!("couldn't start the call (code {rc})")),
        }
    }

    /// SIP transports this engine can use, in order of preference: what
    /// voice-control picks the outbound proxy from.
    pub fn transports(&self) -> &'static [&'static str] {
        if unsafe { pv_has_tls() } != 0 { &["tls", "tcp"] } else { &["tcp"] }
    }

    pub fn leave(&self) {
        unsafe { pv_leave() }
    }

    pub fn set_mute(&self, muted: bool) {
        unsafe { pv_set_mute(muted as c_int) }
    }

    pub fn set_deaf(&self, deaf: bool) {
        unsafe { pv_set_deaf(deaf as c_int) }
    }

    pub fn stats(&self) -> Option<Stats> {
        let mut s = PvStats::default();
        if unsafe { pv_get_stats(&mut s) } != 0 {
            return None;
        }
        Some(Stats {
            codec: unsafe { CStr::from_ptr(s.codec.as_ptr()) }.to_string_lossy().into_owned(),
            clock_rate: s.clock_rate,
            rtt_ms: s.rtt_ms,
            jitter_ms: s.rx_jitter_ms,
            rx_packets: s.rx_packets,
            rx_lost: s.rx_lost,
            tx_packets: s.tx_packets,
            tx_lost: s.tx_lost,
            duration_s: s.duration_s,
            encrypted: s.srtp != 0,
        })
    }

    pub fn devices(&self) -> Vec<Device> {
        let n = unsafe { pv_device_count() };
        (0..n)
            .filter_map(|i| {
                let mut d = PvDevice { name: [0; 128], inputs: 0, outputs: 0 };
                (unsafe { pv_device_info(i, &mut d) } == 0).then(|| Device {
                    index: i,
                    name: unsafe { CStr::from_ptr(d.name.as_ptr()) }.to_string_lossy().into_owned(),
                    inputs: d.inputs,
                    outputs: d.outputs,
                })
            })
            .collect()
    }

    /// Selects capture/playback devices by index; `None` = system default.
    pub fn set_devices(&self, capture: Option<i32>, playback: Option<i32>) -> Result<(), String> {
        let rc = unsafe { pv_set_devices(capture.unwrap_or(-1), playback.unwrap_or(-1)) };
        if rc == 0 { Ok(()) } else { Err(format!("couldn't switch audio devices (code {rc})")) }
    }

    /// Plays a looping ring tone on its own output stream (`None` stops it),
    /// on `playback` (device index) or the call's / default output.
    pub fn tone(&self, tone: Option<Tone>, playback: Option<i32>) -> Result<(), String> {
        let kind = match tone {
            None => 0,
            Some(Tone::Ringback) => 1,
            Some(Tone::Ring) => 2,
        };
        match unsafe { pv_tone(kind, playback.unwrap_or(-1)) } {
            0 => Ok(()),
            -2 => Err("no sound device".into()),
            rc => Err(format!("couldn't play the ring tone (code {rc})")),
        }
    }

    /// Test hook: loop a WAV file into the call instead of the microphone.
    pub fn test_play(&self, path: &str) -> Result<(), String> {
        let p = CString::new(path).map_err(|e| e.to_string())?;
        if unsafe { pv_test_play(p.as_ptr()) } == 0 { Ok(()) } else { Err("couldn't open WAV file".into()) }
    }

    /// Test hook: record what the call delivers to a WAV file.
    pub fn test_record(&self, path: &str) -> Result<(), String> {
        let p = CString::new(path).map_err(|e| e.to_string())?;
        if unsafe { pv_test_record(p.as_ptr()) } == 0 { Ok(()) } else { Err("couldn't create WAV file".into()) }
    }

    /// Runs the call's echo canceller over synthetic audio; returns its name
    /// and rate. A bad configuration aborts the process here instead of in
    /// the first real call.
    pub fn ec_selftest(&self) -> Result<String, String> {
        let mut algo = [0 as c_char; 64];
        match unsafe { pv_ec_selftest(algo.as_mut_ptr(), algo.len() as c_int) } {
            0 => Ok(unsafe { CStr::from_ptr(algo.as_ptr()) }.to_string_lossy().into_owned()),
            rc => Err(format!("code {rc}")),
        }
    }

    /// Stops the SIP stack. Only for process exit.
    pub fn shutdown(&self) {
        unsafe { pv_shutdown() }
    }
}
