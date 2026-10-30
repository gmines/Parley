//! Custom sounds (#13, #14): the user picks an audio file for the incoming
//! ring, the ringback or the message sound. It's decoded here (WAV, MP3,
//! OGG Vorbis, FLAC), mixed to mono, resampled and trimmed, and stored as a
//! 16-bit WAV in the app's data folder, which the voice engine plays. The
//! original file isn't needed afterwards.

use std::io::{Cursor, ErrorKind};
use std::path::PathBuf;

use serde::{Deserialize, Serialize};
use symphonia::core::audio::SampleBuffer;
use symphonia::core::codecs::{DecoderOptions, CODEC_TYPE_NULL};
use symphonia::core::errors::Error as DecodeError;
use symphonia::core::formats::FormatOptions;
use symphonia::core::io::MediaSourceStream;
use symphonia::core::meta::MetadataOptions;
use symphonia::core::probe::Hint;
use tauri::ipc::{InvokeBody, Request};
use tauri::{AppHandle, Manager};

use crate::error::{AppError, Result};

/// The rate stored files are resampled to (the voice engine's).
pub const RATE: u32 = 48_000;
/// Larger files are refused before decoding.
const MAX_FILE_BYTES: usize = 25 * 1024 * 1024;
/// Shorter decoded audio is refused (an empty or broken file).
const MIN_MS: usize = 100;
/// Fade in and out, so a trimmed sound doesn't click.
const FADE_MS: usize = 10;

/// Where a custom sound is used.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Slot {
    Ring,
    Ringback,
    Message,
}

impl Slot {
    fn name(self) -> &'static str {
        match self {
            Slot::Ring => "ring",
            Slot::Ringback => "ringback",
            Slot::Message => "message",
        }
    }

    /// Longer sounds are cut: ring tones loop, a message sound plays once.
    fn max_ms(self) -> usize {
        match self {
            Slot::Ring | Slot::Ringback => 30_000,
            Slot::Message => 5_000,
        }
    }
}

#[derive(Debug, Serialize)]
pub struct Imported {
    /// Length as stored (after trimming), in ms.
    pub ms: u64,
    /// The file was longer and was cut to the slot's maximum.
    pub trimmed: bool,
}

fn dir(app: &AppHandle) -> Result<PathBuf> {
    Ok(app.path().app_data_dir().map_err(|e| AppError::Internal(e.to_string()))?.join("sounds"))
}

/// The slot's stored sound, if there is one.
pub fn custom_path(app: &AppHandle, slot: Slot) -> Option<PathBuf> {
    let p = dir(app).ok()?.join(format!("{}.wav", slot.name()));
    p.is_file().then_some(p)
}

/// Imports an audio file (the request body) for a slot (header `slot`;
/// header `ext`, the file's extension, helps detect the format), replacing
/// the slot's current one.
#[tauri::command]
pub async fn sound_import(app: AppHandle, request: Request<'_>) -> Result<Imported> {
    let header = |name: &str| request.headers().get(name).and_then(|v| v.to_str().ok()).map(str::to_owned);
    let slot: Slot = serde_json::from_value(serde_json::Value::from(header("slot").unwrap_or_default()))
        .map_err(|_| AppError::Internal("unknown sound slot".into()))?;
    let ext = header("ext");
    let InvokeBody::Raw(bytes) = request.body() else {
        return Err(AppError::Internal("expected the file's contents".into()));
    };
    if bytes.len() > MAX_FILE_BYTES {
        return Err(user_error("That file is too large (over 25 MB). Pick a shorter sound."));
    }
    let bytes = bytes.clone();
    let dir = dir(&app)?;
    tauri::async_runtime::spawn_blocking(move || {
        let (samples, trimmed) = decode(bytes, ext.as_deref(), slot.max_ms())?;
        std::fs::create_dir_all(&dir).map_err(|e| AppError::Internal(e.to_string()))?;
        // Written aside and renamed, so a ring never plays a half-written file.
        let tmp = dir.join(format!("{}.wav.tmp", slot.name()));
        std::fs::write(&tmp, wav_bytes(&samples)).map_err(|e| AppError::Internal(e.to_string()))?;
        std::fs::rename(&tmp, dir.join(format!("{}.wav", slot.name()))).map_err(|e| AppError::Internal(e.to_string()))?;
        Ok(Imported { ms: (samples.len() * 1000 / RATE as usize) as u64, trimmed })
    })
    .await
    .map_err(|e| AppError::Internal(e.to_string()))?
}

/// Removes a slot's custom sound (the built-in one plays instead).
#[tauri::command]
pub fn sound_remove(app: AppHandle, slot: Slot) -> Result<()> {
    match std::fs::remove_file(dir(&app)?.join(format!("{}.wav", slot.name()))) {
        Err(e) if e.kind() != ErrorKind::NotFound => Err(AppError::Internal(e.to_string())),
        _ => Ok(()),
    }
}

fn user_error(message: &str) -> AppError {
    AppError::Api { code: "bad_sound".into(), message: message.into() }
}

const UNREADABLE: &str = "Parley couldn't read that file as audio. Use a WAV, MP3, OGG or FLAC file.";

/// Decodes `bytes` to mono samples at [`RATE`], at most `max_ms` long (and
/// whether it was cut), with short fades at both ends.
pub fn decode(bytes: Vec<u8>, ext: Option<&str>, max_ms: usize) -> Result<(Vec<i16>, bool)> {
    let mut hint = Hint::new();
    if let Some(ext) = ext {
        hint.with_extension(ext);
    }
    let source = MediaSourceStream::new(Box::new(Cursor::new(bytes)), Default::default());
    let probed = symphonia::default::get_probe()
        .format(&hint, source, &FormatOptions::default(), &MetadataOptions::default())
        .map_err(|_| user_error(UNREADABLE))?;
    let mut format = probed.format;
    let track = format.tracks().iter().find(|t| t.codec_params.codec != CODEC_TYPE_NULL).ok_or_else(|| user_error(UNREADABLE))?;
    let track_id = track.id;
    let rate = track.codec_params.sample_rate.filter(|r| (4_000..=384_000).contains(r)).ok_or_else(|| user_error(UNREADABLE))?;
    let mut decoder = symphonia::default::get_codecs()
        .make(&track.codec_params, &DecoderOptions::default())
        .map_err(|_| user_error(UNREADABLE))?;

    // Decode a little past the limit, to know whether it was cut.
    let limit = rate as usize * max_ms / 1000;
    let mut mono: Vec<f32> = Vec::new();
    let mut buf: Option<SampleBuffer<f32>> = None;
    while mono.len() <= limit {
        let packet = match format.next_packet() {
            Ok(p) => p,
            Err(DecodeError::IoError(e)) if e.kind() == ErrorKind::UnexpectedEof => break,
            Err(_) if !mono.is_empty() => break, // a damaged end: keep what played
            Err(_) => return Err(user_error(UNREADABLE)),
        };
        if packet.track_id() != track_id {
            continue;
        }
        let decoded = match decoder.decode(&packet) {
            Ok(d) => d,
            Err(DecodeError::DecodeError(_)) => continue, // skip a bad frame
            Err(_) => return Err(user_error(UNREADABLE)),
        };
        let spec = *decoded.spec();
        let channels = spec.channels.count().max(1);
        let b = match &mut buf {
            Some(b) if b.capacity() >= decoded.capacity() * channels => b,
            _ => buf.insert(SampleBuffer::new(decoded.capacity() as u64, spec)),
        };
        b.copy_interleaved_ref(decoded);
        mono.extend(b.samples().chunks(channels).map(|f| f.iter().sum::<f32>() / channels as f32));
    }
    let trimmed = mono.len() > limit;
    mono.truncate(limit);
    if mono.len() < rate as usize * MIN_MS / 1000 {
        return Err(user_error("That sound is too short, or Parley couldn't read it."));
    }

    let mut out = resample(&mono, rate, RATE);
    let fade = (RATE as usize * FADE_MS / 1000).min(out.len() / 2);
    let n = out.len();
    for i in 0..fade {
        let g = i as f32 / fade as f32;
        out[i] *= g;
        out[n - 1 - i] *= g;
    }
    Ok((out.iter().map(|&x| (x.clamp(-1.0, 1.0) * 32767.0) as i16).collect(), trimmed))
}

/// Linear-interpolation resampling. Plenty for ring tones; when lowering the
/// rate, each output sample averages the input it spans, so high notes
/// don't fold back as noise.
fn resample(input: &[f32], from: u32, to: u32) -> Vec<f32> {
    if from == to || input.is_empty() {
        return input.to_vec();
    }
    let ratio = from as f64 / to as f64;
    let len = ((input.len() as f64) / ratio).floor().max(1.0) as usize;
    (0..len)
        .map(|i| {
            let pos = i as f64 * ratio;
            if ratio > 1.0 {
                let a = (pos as usize).min(input.len() - 1);
                let span = &input[a..((pos + ratio) as usize).clamp(a + 1, input.len())];
                span.iter().sum::<f32>() / span.len() as f32
            } else {
                let a = pos as usize;
                let t = (pos - a as f64) as f32;
                let x0 = input[a];
                let x1 = *input.get(a + 1).unwrap_or(&x0);
                x0 + (x1 - x0) * t
            }
        })
        .collect()
}

/// A mono 16-bit PCM WAV file at [`RATE`].
pub fn wav_bytes(samples: &[i16]) -> Vec<u8> {
    let data = (samples.len() * 2) as u32;
    let mut w = Vec::with_capacity(44 + data as usize);
    w.extend_from_slice(b"RIFF");
    w.extend_from_slice(&(36 + data).to_le_bytes());
    w.extend_from_slice(b"WAVEfmt ");
    w.extend_from_slice(&16u32.to_le_bytes()); // fmt chunk size
    w.extend_from_slice(&1u16.to_le_bytes()); // PCM
    w.extend_from_slice(&1u16.to_le_bytes()); // mono
    w.extend_from_slice(&RATE.to_le_bytes());
    w.extend_from_slice(&(RATE * 2).to_le_bytes()); // bytes per second
    w.extend_from_slice(&2u16.to_le_bytes()); // block align
    w.extend_from_slice(&16u16.to_le_bytes()); // bits per sample
    w.extend_from_slice(b"data");
    w.extend_from_slice(&data.to_le_bytes());
    for s in samples {
        w.extend_from_slice(&s.to_le_bytes());
    }
    w
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A stereo 16-bit WAV at `rate`: a 440 Hz tone, `ms` long.
    fn stereo_wav(rate: u32, ms: usize) -> Vec<u8> {
        let frames = rate as usize * ms / 1000;
        let data = (frames * 4) as u32;
        let mut w = Vec::new();
        w.extend_from_slice(b"RIFF");
        w.extend_from_slice(&(36 + data).to_le_bytes());
        w.extend_from_slice(b"WAVEfmt ");
        w.extend_from_slice(&16u32.to_le_bytes());
        w.extend_from_slice(&1u16.to_le_bytes());
        w.extend_from_slice(&2u16.to_le_bytes());
        w.extend_from_slice(&rate.to_le_bytes());
        w.extend_from_slice(&(rate * 4).to_le_bytes());
        w.extend_from_slice(&4u16.to_le_bytes());
        w.extend_from_slice(&16u16.to_le_bytes());
        w.extend_from_slice(b"data");
        w.extend_from_slice(&data.to_le_bytes());
        for i in 0..frames {
            let v = ((i as f32 * 440.0 * std::f32::consts::TAU / rate as f32).sin() * 16000.0) as i16;
            w.extend_from_slice(&v.to_le_bytes());
            w.extend_from_slice(&v.to_le_bytes());
        }
        w
    }

    #[test]
    fn decodes_resamples_and_keeps_length() {
        let (s, trimmed) = decode(stereo_wav(44_100, 1000), Some("wav"), 30_000).unwrap();
        assert!(!trimmed);
        assert!((s.len() as i64 - RATE as i64).abs() < 50, "{} samples", s.len());
        let peak = s.iter().map(|x| x.unsigned_abs()).max().unwrap();
        assert!((15_000..=16_500).contains(&peak), "peak {peak}");
        assert_eq!(s[0], 0); // faded in
    }

    #[test]
    fn trims_long_sounds() {
        let (s, trimmed) = decode(stereo_wav(48_000, 7000), None, 5_000).unwrap();
        assert!(trimmed);
        assert_eq!(s.len(), 5 * RATE as usize);
    }

    #[test]
    fn refuses_garbage_and_silence() {
        assert!(decode(b"not audio at all".repeat(100), Some("mp3"), 5_000).is_err());
        assert!(decode(stereo_wav(48_000, 20), None, 5_000).is_err());
    }

    #[test]
    fn writes_a_wav_symphonia_reads_back() {
        let samples: Vec<i16> = (0..4800).map(|i| (i % 100) as i16 * 100).collect();
        let (back, _) = decode(wav_bytes(&samples), Some("wav"), 5_000).unwrap();
        assert_eq!(back.len(), samples.len());
    }
}
