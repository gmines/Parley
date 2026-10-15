// Audio device choices, saved by name (device indexes can change between
// runs) and applied to the voice engine before a call or an audio test.
import { api, type AudioDevice } from "./api";

const DEVICE_KEY = "parley.audioDevices";

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

/** Selects the saved devices in the engine (system default where none is
 * saved or the saved one is gone). */
export async function applySavedDevices() {
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
