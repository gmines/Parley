// Audio device choices, saved by name (device indexes can change between
// runs), and volume levels: applied to the voice engine before a call or an
// audio test.
import { api, type AudioDevice, type MessageSound } from "./api";

const DEVICE_KEY = "parley.audioDevices";
const LEVELS_KEY = "parley.audioLevels";
const SOUNDS_KEY = "parley.messageSounds";
const AUTO_LEAVE_KEY = "parley.autoLeave";

export interface DeviceChoice {
  capture: string | null;
  playback: string | null;
}

export function savedDevices(): DeviceChoice {
  try {
    return JSON.parse(localStorage.getItem(DEVICE_KEY) || "") ?? { capture: null, playback: null };
  } catch {
    return { capture: null, playback: null };
  }
}

export function saveDevices(choice: DeviceChoice) {
  try {
    localStorage.setItem(DEVICE_KEY, JSON.stringify(choice));
  } catch {
    // storage unavailable: applies to this session only
  }
}

export interface Levels {
  /** Microphone volume, 0-100. */
  mic: number;
  /** Microphone boost in dB: 0 (off), 5, 10 or 15. */
  boost: number;
  /** Output (speaker / headphones) volume, 0-100. */
  output: number;
  /** Output boost in dB, like the microphone's (#12). */
  outputBoost: number;
}

export const BOOST_STEPS = [0, 5, 10, 15];

const num = (x: unknown, lo: number, hi: number, d: number) => (typeof x === "number" && x >= lo && x <= hi ? x : d);

export function savedLevels(): Levels {
  const def: Levels = { mic: 100, boost: 0, output: 100, outputBoost: 0 };
  try {
    const v = JSON.parse(localStorage.getItem(LEVELS_KEY) || "null");
    const step = (x: unknown) => (BOOST_STEPS.includes(x as number) ? (x as number) : 0);
    return v
      ? { mic: num(v.mic, 0, 100, 100), boost: step(v.boost), output: num(v.output, 0, 100, 100), outputBoost: step(v.outputBoost) }
      : def;
  } catch {
    return def;
  }
}

export function saveLevels(levels: Levels) {
  try {
    localStorage.setItem(LEVELS_KEY, JSON.stringify(levels));
  } catch {
    // storage unavailable: applies to this session only
  }
}

/** Sets the saved volume levels in the engine (they apply right away, also mid-call). */
export function applyLevels(levels = savedLevels()): Promise<void> {
  return api.voiceSetLevels(levels.mic / 100, levels.boost, levels.output / 100, levels.outputBoost);
}

// Message sounds (#14) -------------------------------------------------------------

export interface MessageSounds {
  enabled: boolean;
  sound: MessageSound;
  /** 0-100, independent of the call volume. */
  volume: number;
}

export const MESSAGE_SOUNDS: { id: MessageSound; label: string }[] = [
  { id: "chime", label: "Chime" },
  { id: "pop", label: "Pop" },
  { id: "ding", label: "Ding" },
  { id: "soft", label: "Soft" },
];

export function savedMessageSounds(): MessageSounds {
  const def: MessageSounds = { enabled: true, sound: "chime", volume: 70 };
  try {
    const v = JSON.parse(localStorage.getItem(SOUNDS_KEY) || "null");
    return v
      ? {
          enabled: typeof v.enabled === "boolean" ? v.enabled : def.enabled,
          sound: MESSAGE_SOUNDS.some((s) => s.id === v.sound) ? v.sound : def.sound,
          volume: num(v.volume, 0, 100, def.volume),
        }
      : def;
  } catch {
    return def;
  }
}

export function saveMessageSounds(s: MessageSounds) {
  try {
    localStorage.setItem(SOUNDS_KEY, JSON.stringify(s));
  } catch {
    // storage unavailable: applies to this session only
  }
}

/** Messages arriving together (a burst, or several conversations) ring once. */
const SOUND_COOLDOWN_MS = 2000;
let lastSound = 0;

/** Plays the message sound, if enabled. `preview` plays it regardless (settings). */
export async function playMessageSound(preview?: MessageSounds) {
  const s = preview ?? savedMessageSounds();
  if (!preview) {
    if (!s.enabled || Date.now() - lastSound < SOUND_COOLDOWN_MS) return;
    lastSound = Date.now();
  }
  const playback = await savedPlaybackIndex();
  await api.voiceNotify(s.sound, s.volume / 100, playback).catch(() => {});
}

// Leaving abandoned calls (#15) ----------------------------------------------------

/** Minutes away (no input, microphone silent) before leaving a call; 0 = never. */
export const AUTO_LEAVE_STEPS = [0, 15, 30, 60, 120];

export function savedAutoLeave(): number {
  try {
    const v = JSON.parse(localStorage.getItem(AUTO_LEAVE_KEY) || "null");
    return AUTO_LEAVE_STEPS.includes(v) ? v : 60;
  } catch {
    return 60;
  }
}

export function saveAutoLeave(minutes: number) {
  try {
    localStorage.setItem(AUTO_LEAVE_KEY, JSON.stringify(minutes));
  } catch {
    // storage unavailable: applies to this session only
  }
}

/** Selects the saved devices and levels in the engine (system default where
 * no device is saved or the saved one is gone). Listing the devices also
 * rescans them, so ones plugged in since are found. */
export async function applySavedDevices() {
  await applyLevels().catch(() => {});
  const saved = savedDevices();
  const { devices } = await api.voiceDevices();
  const find = (name: string | null, kind: "inputs" | "outputs") => devices.find((x) => x.name === name && x[kind] > 0)?.index ?? null;
  await api.voiceSetDevices(find(saved.capture, "inputs"), find(saved.playback, "outputs"));
}

/** The saved output device's current index, or null for the default. */
export async function savedPlaybackIndex(): Promise<number | null> {
  const name = savedDevices().playback;
  if (!name) return null;
  const { devices } = await api.voiceDevices().catch(() => ({ devices: [] as AudioDevice[] }));
  return devices.find((d) => d.name === name && d.outputs > 0)?.index ?? null;
}

/** Readable device names. On Linux, ALSA lists every card several times
 * (surround51:..., iec958:..., front:...): keep one entry per card. Returns
 * null for entries to hide. */
export function friendlyName(name: string): string | null {
  if (/^(surround\d+|iec958|front|dmix|dsnoop|hw|plughw|null|lavrate|samplerate|speexrate|upmix|vdownmix|usbstream):/i.test(name)) return null;
  const card = name.match(/^sysdefault:CARD=(.+)$/);
  if (card) return card[1];
  const hdmi = name.match(/^hdmi:CARD=([^,]+),DEV=(\d+)$/);
  if (hdmi) return `${hdmi[1]} output ${Number(hdmi[2]) + 1}`;
  if (name === "pipewire") return "PipeWire";
  if (name === "pulse") return "PulseAudio";
  if (name === "default") return "ALSA default";
  return name;
}
