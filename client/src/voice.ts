// Voice calls in the UI: active calls (from realtime events), our own call
// state (from the voice engine), and the widgets built on them: the call
// bar, the incoming-call card, the conversation call button and the device
// picker.
import { api, errorMessage, type AudioDevice, type Call, type RealtimeEvent, type VoiceCallState, type VoiceEvent, type VoiceStats } from "./api";
import { avatar, h, toast } from "./dom";
import { conversationTitle, store } from "./store";

type Listener = () => void;

const calls = new Map<string, Call>(); // conversation id -> call in progress
const listeners = new Set<Listener>();
let me = "";
let mine: { conversation: string | null; state: VoiceCallState; muted: boolean; deafened: boolean; stats: VoiceStats | null; since: number } = {
  conversation: null,
  state: "idle",
  muted: false,
  deafened: false,
  stats: null,
  since: 0,
};
let statsTimer: number | undefined;
let ringing: { conversation: string; card: HTMLElement; stop: () => void } | null = null;

function changed() {
  for (const fn of listeners) fn();
}

function nameOf(conversationId: string, userId: string): string {
  const m = store.get(conversationId)?.members.find((u) => u.id === userId);
  return m?.display_name ?? "Someone";
}

export const voice = {
  subscribe(fn: Listener): () => void {
    listeners.add(fn);
    return () => listeners.delete(fn);
  },

  setMe(id: string) {
    me = id;
  },

  call(conversationId: string): Call | undefined {
    return calls.get(conversationId);
  },

  /** The conversation whose call we're in, if any. */
  current() {
    return mine;
  },

  inCall(conversationId?: string): boolean {
    return mine.conversation !== null && (conversationId === undefined || mine.conversation === conversationId);
  },

  async resync() {
    try {
      const list = await api.voiceCalls();
      calls.clear();
      for (const c of list) calls.set(c.conversation_id, c);
      const st = await api.voiceStatus();
      mine = { ...mine, conversation: st.conversation, muted: st.muted, deafened: st.deafened };
      changed();
    } catch {
      // voice not configured on this server, or offline: leave as is
    }
  },

  async join(conversationId: string) {
    stopRinging(conversationId);
    if (mine.conversation && mine.conversation !== conversationId) {
      await voice.leave();
      await new Promise((r) => setTimeout(r, 300));
    }
    mine = { ...mine, conversation: conversationId, state: "calling", stats: null, since: Date.now() };
    changed();
    try {
      await applySavedDevices();
      await api.voiceJoin(conversationId);
    } catch (e) {
      mine = { ...mine, conversation: null, state: "idle" };
      changed();
      toast(`Couldn't join the call: ${errorMessage(e)}`, "error");
    }
  },

  async leave() {
    await api.voiceLeave().catch(() => {});
    mine = { ...mine, conversation: null, state: "idle", stats: null };
    stopStats();
    changed();
  },

  async toggleMute() {
    const st = await api.voiceSetMute(!(mine.muted || mine.deafened));
    mine = { ...mine, muted: st.muted, deafened: st.deafened };
    changed();
  },

  async toggleDeafen() {
    const st = await api.voiceSetDeaf(!mine.deafened);
    mine = { ...mine, muted: st.muted, deafened: st.deafened };
    changed();
  },

  async decline(conversationId: string) {
    stopRinging(conversationId);
    await api.voiceDecline(conversationId).catch(() => {});
  },

  /** Realtime call.* events. */
  handleRealtime(e: RealtimeEvent) {
    switch (e.type) {
      case "call.started": {
        const c = e.data as Call;
        calls.set(c.conversation_id, c);
        if (c.started_by !== me && !voice.inCall(c.conversation_id)) startRinging(c);
        break;
      }
      case "call.updated": {
        const c = e.data as Call;
        calls.set(c.conversation_id, c);
        // Someone answered elsewhere on our account: stop ringing.
        if (c.participants.includes(me)) stopRinging(c.conversation_id);
        break;
      }
      case "call.ended":
        calls.delete(e.data.conversation_id);
        stopRinging(e.data.conversation_id);
        break;
      case "call.declined":
        if (e.data.user_id === me) stopRinging(e.data.conversation_id);
        else if (voice.inCall(e.data.conversation_id) && store.get(e.data.conversation_id)?.kind === "dm") {
          toast(`${nameOf(e.data.conversation_id, e.data.user_id)} declined the call.`);
        }
        break;
      default:
        return;
    }
    changed();
  },

  /** Events from our own voice engine. */
  handleVoiceEvent(e: VoiceEvent) {
    if (e.type === "media") return;
    if (e.state === "ended") {
      const failed = e.status >= 400 && mine.state !== "connected";
      if (failed && mine.conversation) toast(`Call failed: ${e.reason || `error ${e.status}`}`, "error");
      mine = { ...mine, conversation: null, state: "idle", stats: null };
      stopStats();
    } else {
      mine = { ...mine, state: e.state };
      if (e.state === "connected") {
        mine.since = Date.now();
        startStats();
      }
    }
    changed();
  },

  reset() {
    calls.clear();
    mine = { conversation: null, state: "idle", muted: false, deafened: false, stats: null, since: 0 };
    stopStats();
    if (ringing) stopRinging(ringing.conversation);
    changed();
  },
};

function startStats() {
  stopStats();
  const poll = async () => {
    mine = { ...mine, stats: await api.voiceStats().catch(() => null) };
    changed();
  };
  poll();
  statsTimer = window.setInterval(poll, 2000);
}

function stopStats() {
  window.clearInterval(statsTimer);
  statsTimer = undefined;
}

/** "good" / "fair" / "poor" from packet loss, round-trip time and jitter. */
export function quality(s: VoiceStats | null): { level: "good" | "fair" | "poor" | "unknown"; detail: string } {
  if (!s) return { level: "unknown", detail: "Measuring…" };
  const rxLoss = s.rx_packets + s.rx_lost ? (100 * s.rx_lost) / (s.rx_packets + s.rx_lost) : 0;
  const txLoss = s.tx_packets ? (100 * s.tx_lost) / s.tx_packets : 0;
  const loss = Math.max(rxLoss, txLoss);
  const level = loss < 2 && s.rtt_ms < 250 && s.jitter_ms < 30 ? "good" : loss < 8 && s.rtt_ms < 500 ? "fair" : "poor";
  const detail = [
    `${s.codec.toUpperCase()} ${Math.round(s.clock_rate / 1000)} kHz${s.encrypted ? ", encrypted" : ", NOT encrypted"}`,
    `Round trip ${s.rtt_ms ? `${s.rtt_ms} ms` : "measuring…"}`,
    `Jitter ${s.jitter_ms} ms`,
    `Loss: ${rxLoss.toFixed(1)}% in, ${txLoss.toFixed(1)}% out`,
  ].join("\n");
  return { level, detail };
}

// Ringing --------------------------------------------------------------------------

const RING_TIMEOUT_MS = 30_000;

function startRinging(c: Call) {
  if (ringing) stopRinging(ringing.conversation);
  const conv = store.get(c.conversation_id);
  const caller = conv?.members.find((m) => m.id === c.started_by);
  const title = conv?.kind === "group" ? `${caller?.display_name ?? "Someone"} started a call in ${conv ? conversationTitle(conv) : "a group"}` : `${caller?.display_name ?? "Someone"} is calling`;
  const card = h(
    "div",
    { class: "ring-card", attrs: { role: "alertdialog", "aria-label": title } },
    caller ? avatar(caller) : null,
    h("div", { class: "row-text" }, h("span", { class: "row-name", text: title }), h("span", { class: "row-sub", text: "Incoming call" })),
    h("button", { class: "btn btn-sm btn-ok", text: "Join", on: { click: () => voice.join(c.conversation_id) } }),
    h("button", { class: "btn btn-sm btn-danger", text: "Decline", on: { click: () => voice.decline(c.conversation_id) } }),
  );
  document.body.append(card);
  const stopTone = ringTone();
  const timer = window.setTimeout(() => stopRinging(c.conversation_id), RING_TIMEOUT_MS);
  ringing = {
    conversation: c.conversation_id,
    card,
    stop: () => {
      stopTone();
      window.clearTimeout(timer);
      card.remove();
    },
  };
}

function stopRinging(conversationId: string) {
  if (ringing?.conversation === conversationId) {
    ringing.stop();
    ringing = null;
  }
}

/** A gentle two-tone ring (generated, no audio files). Returns a stop function. */
function ringTone(): () => void {
  let ctx: AudioContext | null = null;
  let timer: number | undefined;
  try {
    ctx = new AudioContext();
    const ring = () => {
      if (!ctx) return;
      const t = ctx.currentTime;
      for (const [f, start] of [[660, 0], [880, 0.25], [660, 0.9], [880, 1.15]] as const) {
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        osc.frequency.value = f;
        gain.gain.setValueAtTime(0, t + start);
        gain.gain.linearRampToValueAtTime(0.08, t + start + 0.02);
        gain.gain.linearRampToValueAtTime(0, t + start + 0.22);
        osc.connect(gain).connect(ctx.destination);
        osc.start(t + start);
        osc.stop(t + start + 0.25);
      }
    };
    ring();
    timer = window.setInterval(ring, 3000);
  } catch {
    // No audio output available: the card alone will do.
  }
  return () => {
    window.clearInterval(timer);
    ctx?.close().catch(() => {});
    ctx = null;
  };
}

// Widgets ------------------------------------------------------------------------

function fmtDuration(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  const mm = Math.floor(s / 60);
  return `${mm}:${String(s % 60).padStart(2, "0")}`;
}

/** The sidebar call bar, shown while we're in (or joining) a call. */
export function callBar(onOpen: (conversationId: string) => void): HTMLElement {
  const el = h("div", { class: "call-bar", attrs: { hidden: "" } });
  let tick: number | undefined;

  const render = () => {
    const c = mine.conversation;
    el.hidden = !c;
    window.clearInterval(tick);
    if (!c) {
      el.replaceChildren();
      return;
    }
    const conv = store.get(c);
    const q = quality(mine.stats);
    const status =
      mine.state === "connected" ? "Voice connected" : mine.state === "connecting" ? "Connecting…" : "Calling…";
    const timer = h("span", { class: "call-timer" });
    const updateTimer = () => (timer.textContent = mine.state === "connected" ? fmtDuration(Date.now() - mine.since) : "");
    updateTimer();
    tick = window.setInterval(updateTimer, 1000);

    el.replaceChildren(
      h(
        "div",
        { class: "call-bar-top" },
        h("span", { class: `quality-dot quality-${mine.state === "connected" ? q.level : "unknown"}`, attrs: { title: q.detail } }),
        h(
          "button",
          { class: "call-bar-title", attrs: { title: "Open conversation" }, on: { click: () => onOpen(c) } },
          h("span", { class: `call-status call-status-${mine.state}`, text: status }),
          h("span", { class: "call-where", text: conv ? conversationTitle(conv) : "" }),
        ),
        timer,
      ),
      h(
        "div",
        { class: "call-bar-actions" },
        iconBtn(mine.muted || mine.deafened ? "Unmute" : "Mute", mine.muted || mine.deafened ? "🔇" : "🎤", mine.muted || mine.deafened, () => voice.toggleMute()),
        iconBtn(mine.deafened ? "Undeafen" : "Deafen", mine.deafened ? "🙉" : "🎧", mine.deafened, () => voice.toggleDeafen()),
        iconBtn("Audio devices", "⚙", false, () => devicesDialog()),
        iconBtn("Leave call", "✕", false, () => voice.leave(), "call-leave"),
      ),
    );
  };
  const off = [voice.subscribe(render), store.subscribe(render)];
  render();
  (el as HTMLElement & { dispose?: () => void }).dispose = () => {
    off.forEach((f) => f());
    window.clearInterval(tick);
  };
  return el;
}

/** Releases a widget created by callBar() or callControl(). */
export function disposeWidget(el: HTMLElement) {
  (el as HTMLElement & { dispose?: () => void }).dispose?.();
}

function iconBtn(label: string, icon: string, active: boolean, fn: () => void, extra = ""): HTMLElement {
  return h("button", {
    class: `call-btn${active ? " call-btn-active" : ""} ${extra}`,
    text: icon,
    attrs: { title: label, "aria-label": label, "aria-pressed": String(active) },
    on: { click: fn },
  });
}

/** The call control for a conversation header. Re-renders itself. */
export function callControl(conversationId: string): HTMLElement {
  const el = h("div", { class: "call-control" });
  const render = () => {
    const c = calls.get(conversationId);
    const here = voice.inCall(conversationId);
    const elsewhere = voice.inCall() && !here;
    const people = (c?.participants ?? []).map((id) => {
      const u = store.get(conversationId)?.members.find((m) => m.id === id);
      return u ? avatar(u, "xs") : null;
    });
    const btn = here
      ? h("button", { class: "btn btn-sm btn-danger", text: "Leave call", on: { click: () => voice.leave() } })
      : h("button", {
          class: `btn btn-sm ${c ? "btn-ok" : "btn-ghost"}`,
          text: c ? `Join call` : "📞 Call",
          attrs: elsewhere ? { title: "Joining will leave your current call" } : {},
          on: { click: () => voice.join(conversationId) },
        });
    el.replaceChildren(c ? h("div", { class: "call-people", attrs: { title: `${c.participants.length} in call` } }, ...people) : "", btn);
  };
  const off = [voice.subscribe(render), store.subscribe(render)];
  render();
  (el as HTMLElement & { dispose?: () => void }).dispose = () => off.forEach((f) => f());
  return el;
}

// Devices ------------------------------------------------------------------------

const DEVICE_KEY = "parley.audioDevices";

function savedDevices(): { capture: string | null; playback: string | null } {
  try {
    return JSON.parse(localStorage.getItem(DEVICE_KEY) || "") ?? { capture: null, playback: null };
  } catch {
    return { capture: null, playback: null };
  }
}

/** Device indexes can change between runs, so choices are saved by name. */
async function applySavedDevices() {
  const saved = savedDevices();
  if (!saved.capture && !saved.playback) return;
  const { devices } = await api.voiceDevices();
  const find = (name: string | null, kind: "inputs" | "outputs") => {
    const d = devices.find((x) => x.name === name && x[kind] > 0);
    return d ? d.index : null;
  };
  await api.voiceSetDevices(find(saved.capture, "inputs"), find(saved.playback, "outputs")).catch(() => {});
}

async function devicesDialog() {
  let devices: AudioDevice[] = [];
  try {
    devices = (await api.voiceDevices()).devices;
  } catch (e) {
    toast(errorMessage(e), "error");
    return;
  }
  const saved = savedDevices();
  const select = (kind: "inputs" | "outputs", current: string | null) => {
    const s = h("select", { class: "select" }, h("option", { text: "System default", attrs: { value: "" } }));
    for (const d of devices.filter((x) => x[kind] > 0 && friendlyName(x.name) !== null)) {
      const o = h("option", { text: friendlyName(d.name)!, attrs: { value: d.name } });
      if (d.name === current) o.selected = true;
      s.append(o);
    }
    return s;
  };
  const mic = select("inputs", saved.capture);
  const out = select("outputs", saved.playback);
  const dialog = h("dialog", { class: "dialog" });
  const close = () => {
    dialog.close();
    dialog.remove();
  };
  const save = async () => {
    const choice = { capture: mic.value || null, playback: out.value || null };
    try {
      localStorage.setItem(DEVICE_KEY, JSON.stringify(choice));
    } catch {
      // storage unavailable: applies to this session only
    }
    await applySavedDevices();
    if (!choice.capture && !choice.playback) await api.voiceSetDevices(null, null).catch(() => {});
    toast("Audio devices updated.");
    close();
  };
  dialog.append(
    h("h2", { text: "Audio devices" }),
    devices.length === 0 ? h("p", { class: "hint", text: "No audio devices were found." }) : "",
    h("label", { class: "field" }, h("span", { text: "Microphone" }), mic),
    h("label", { class: "field" }, h("span", { text: "Speakers / headphones" }), out),
    h("div", { class: "dialog-actions" }, h("button", { class: "btn btn-ghost", text: "Cancel", on: { click: close } }), h("button", { class: "btn btn-primary", text: "Save", on: { click: save } })),
  );
  dialog.addEventListener("cancel", (e) => {
    e.preventDefault();
    close();
  });
  document.body.append(dialog);
  dialog.showModal();
}

/** Readable device names. On Linux, ALSA lists every card several times
 * (surround51:..., iec958:..., front:...): keep one entry per card. Returns
 * null for entries to hide. */
function friendlyName(name: string): string | null {
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
