// Voice calls in the UI: active calls (from realtime events), our own call
// state (from the voice engine), and the widgets built on them: the call
// bar, the incoming-call card and the conversation call button. Audio
// settings are in views/audio.ts.
import { api, errorMessage, type Call, type RealtimeEvent, type VoiceCallState, type VoiceEvent, type VoiceStats } from "./api";
import { applyLevels, applySavedDevices, savedAutoLeave, savedPlaybackIndex } from "./audio";
import { avatar, h, toast } from "./dom";
import { conversationTitle, store } from "./store";
import { audioDialog } from "./views/audio";

type Listener = () => void;

const calls = new Map<string, Call>(); // conversation id -> call in progress
const listeners = new Set<Listener>();
let me = "";
let mine: {
  conversation: string | null;
  state: VoiceCallState;
  muted: boolean;
  deafened: boolean;
  /** In the call without a microphone: nobody hears us until one's selected. */
  listenOnly: boolean;
  stats: VoiceStats | null;
  since: number;
} = {
  conversation: null,
  state: "idle",
  muted: false,
  deafened: false,
  listenOnly: false,
  stats: null,
  since: 0,
};
let statsTimer: number | undefined;
let ringing: { conversation: string; card: HTMLElement; stop: () => void } | null = null;
// We started a call and nobody else is in it yet: the caller hears ringback.
let ringback: { conversation: string; timer: number } | null = null;
// Audio settings' "Test speakers": a few seconds of the ring tone.
let speakerTest: number | undefined;

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

  /** Plays the ring tone on the selected output for a few seconds. */
  testSpeakers() {
    window.clearTimeout(speakerTest);
    speakerTest = window.setTimeout(() => {
      speakerTest = undefined;
      updateTone();
    }, 3500);
    toneNow = null; // restart the tone, so it's audible from the start on the new device
    updateTone();
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
      mine = { ...mine, conversation: st.conversation, muted: st.muted, deafened: st.deafened, listenOnly: st.listen_only };
      changed();
    } catch {
      // voice not configured on this server, or offline: leave as is
    }
  },

  /** Re-reads mute / listen-only from the engine (after a device change). */
  async refreshStatus() {
    const st = await api.voiceStatus().catch(() => null);
    if (!st || !mine.conversation) return;
    const gotMic = mine.listenOnly && !st.listen_only;
    mine = { ...mine, muted: st.muted, deafened: st.deafened, listenOnly: st.listen_only };
    if (gotMic) toast("Microphone connected: others can hear you now.");
    changed();
  },

  async join(conversationId: string) {
    stopRinging(conversationId);
    if (mine.conversation && mine.conversation !== conversationId) {
      await voice.leave();
      await new Promise((r) => setTimeout(r, 300));
    }
    mine = { ...mine, conversation: conversationId, state: "calling", stats: null, since: Date.now() };
    changed();
    // Nobody's in it yet: we're placing the call, so ring until someone answers.
    const starting = !(calls.get(conversationId)?.participants.some((p) => p !== me));
    try {
      // If the saved devices can't be selected, the engine falls back to the defaults.
      await applySavedDevices().catch(() => {});
      const r = await api.voiceJoin(conversationId);
      mine = { ...mine, listenOnly: r.listen_only };
      changed();
      if (r.listen_only) {
        toast("No microphone found: you've joined listen-only. Connect one and pick it in Audio settings (⚙) so others can hear you.", "error");
      }
      if (starting && mine.conversation === conversationId) startRingback(conversationId);
    } catch (e) {
      mine = { ...mine, conversation: null, state: "idle", listenOnly: false };
      changed();
      toast(`Couldn't join the call: ${errorMessage(e)}`, "error");
      // Answering someone's call: tell them we tried, so they stop waiting (#6).
      if (!starting) api.voiceDecline(conversationId, "failed").catch(() => {});
    }
  },

  async leave() {
    stopRingback();
    stopAwayWatch();
    await api.voiceLeave().catch(() => {});
    mine = { ...mine, conversation: null, state: "idle", listenOnly: false, stats: null };
    stopStats();
    changed();
  },

  async toggleMute() {
    const st = await api.voiceSetMute(!mine.muted);
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
        // Someone answered our call: stop the ringback.
        if (ringback?.conversation === c.conversation_id && c.participants.some((p) => p !== me)) stopRingback();
        break;
      }
      case "call.ended":
        calls.delete(e.data.conversation_id);
        stopRinging(e.data.conversation_id);
        break;
      case "call.declined": {
        const conv = e.data.conversation_id;
        const failed = e.data.reason === "failed";
        if (e.data.user_id === me) stopRinging(conv);
        else if (voice.inCall(conv) && (store.get(conv)?.kind === "dm" || failed)) {
          const who = nameOf(conv, e.data.user_id);
          if (failed) toast(`${who} tried to join but couldn't (a problem with their audio or connection).`, "error");
          else toast(`${who} declined the call.`);
          // Like a phone: a declined 1:1 call ends, unless they'd already answered.
          if (ringback?.conversation === conv && store.get(conv)?.kind === "dm") voice.leave();
        }
        break;
      }
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
      mine = { ...mine, conversation: null, state: "idle", listenOnly: false, stats: null };
      stopStats();
      stopRingback();
      stopAwayWatch();
    } else {
      mine = { ...mine, state: e.state };
      if (e.state === "connected") {
        mine.since = Date.now();
        startStats();
        startAwayWatch();
      }
    }
    changed();
  },

  reset() {
    calls.clear();
    mine = { conversation: null, state: "idle", muted: false, deafened: false, listenOnly: false, stats: null, since: 0 };
    stopStats();
    if (ringing) stopRinging(ringing.conversation);
    stopRingback();
    stopAwayWatch();
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

// Leaving abandoned calls (#15) ------------------------------------------------------
// A call ends by itself once we've been away for the chosen time (no input
// on the computer and a silent microphone), or alone in it for 5 minutes,
// after a minute's warning that brings the window back.

const ALONE_LEAVE_MS = 5 * 60_000;
const WARNING_S = 60;
const IDLE_POLL_MS = 15_000;
const MIC_POLL_MS = 2_000;
/** pv_mic_level counted as speech (full scale reads about 127). */
const MIC_ACTIVE = 20;

let away: {
  lastActive: number;
  aloneSince: number;
  timers: number[];
  warning: { card: HTMLElement; timer: number } | null;
} | null = null;

const markActive = () => {
  if (away) away.lastActive = Date.now();
};
const APP_INPUT = ["keydown", "pointerdown", "pointermove", "wheel"] as const;

function startAwayWatch() {
  stopAwayWatch();
  const now = Date.now();
  away = { lastActive: now, aloneSince: 0, timers: [], warning: null };
  for (const ev of APP_INPUT) window.addEventListener(ev, markActive, { passive: true });
  away.timers.push(
    window.setInterval(async () => {
      const level = await api.voiceMicLevel().catch(() => 0);
      if (level >= MIC_ACTIVE) markActive();
    }, MIC_POLL_MS),
    window.setInterval(checkAway, IDLE_POLL_MS),
  );
}

function stopAwayWatch() {
  if (!away) return;
  for (const t of away.timers) window.clearInterval(t);
  for (const ev of APP_INPUT) window.removeEventListener(ev, markActive);
  dismissWarning();
  away = null;
}

/** Nobody else is in our call (unknown, e.g. offline, counts as not alone). */
function alone(): boolean {
  const c = mine.conversation ? calls.get(mine.conversation) : undefined;
  return !!c && !c.participants.some((p) => p !== me);
}

async function checkAway() {
  if (!away || away.warning) return;
  const minutes = savedAutoLeave();
  if (minutes === 0) return;
  const idle = await api.systemIdle().catch(() => null);
  if (!away || away.warning) return;
  const now = Date.now();
  // Where the OS can't say how long it's been idle, only a locked screen
  // counts as away: never leave a call someone may be using.
  if (idle?.seconds != null) away.lastActive = Math.max(away.lastActive, now - idle.seconds * 1000);
  else if (!idle?.locked) away.lastActive = now;
  away.aloneSince = alone() ? away.aloneSince || now : 0;

  if (away.aloneSince && now - away.aloneSince >= ALONE_LEAVE_MS) warn("alone");
  else if (now - away.lastActive >= minutes * 60_000) warn("away");
}

function warn(reason: "alone" | "away") {
  if (!away) return;
  let left = WARNING_S;
  const text = h("span", { class: "row-sub" });
  const still = () => (reason === "alone" ? alone() : Date.now() - away!.lastActive > 5_000);
  const update = () => (text.textContent = `Leaving in ${left} s`);
  const stay = () => {
    if (!away) return;
    away.lastActive = Date.now();
    away.aloneSince = 0;
    dismissWarning();
  };
  const card = h(
    "div",
    { class: "ring-card", attrs: { role: "alertdialog", "aria-label": "Still in the call?" } },
    h("div", { class: "row-text" }, h("span", { class: "row-name", text: reason === "alone" ? "You're the only one in this call" : "Still in the call?" }), text),
    h("button", { class: "btn btn-sm btn-ok", text: "Stay", on: { click: stay } }),
    h("button", { class: "btn btn-sm btn-danger", text: "Leave now", on: { click: () => voice.leave() } }),
  );
  update();
  document.body.append(card);
  const timer = window.setInterval(() => {
    if (!still()) return dismissWarning(); // someone joined, or we're back
    if (--left > 0) return update();
    voice.leave();
    toast(reason === "alone" ? "You left the call: nobody else was in it." : "You left the call: you seemed to be away.");
  }, 1000);
  away.warning = { card, timer };
  api.requestAttention().catch(() => {});
  savedPlaybackIndex()
    .then((playback) => api.voiceNotify("ding", 0.8, playback))
    .catch(() => {});
}

function dismissWarning() {
  if (!away?.warning) return;
  window.clearInterval(away.warning.timer);
  away.warning.card.remove();
  away.warning = null;
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
  const timer = window.setTimeout(() => stopRinging(c.conversation_id), RING_TIMEOUT_MS);
  ringing = {
    conversation: c.conversation_id,
    card,
    stop: () => {
      window.clearTimeout(timer);
      card.remove();
    },
  };
  updateTone();
}

function stopRinging(conversationId: string) {
  if (ringing?.conversation === conversationId) {
    ringing.stop();
    ringing = null;
    updateTone();
  }
}

function startRingback(conversationId: string) {
  stopRingback();
  const timer = window.setTimeout(() => {
    // Recipients stop ringing after the same time.
    const dm = store.get(conversationId)?.kind === "dm";
    stopRingback();
    if (dm && voice.inCall(conversationId)) {
      toast("No answer.");
      voice.leave();
    }
  }, RING_TIMEOUT_MS);
  ringback = { conversation: conversationId, timer };
  updateTone();
}

function stopRingback() {
  if (!ringback) return;
  window.clearTimeout(ringback.timer);
  ringback = null;
  updateTone();
}

/** Plays whichever tone applies: an incoming call's ring wins over our ringback. */
let toneNow: "ring" | "ringback" | null = null;
let toneSeq = 0;
let webTone: (() => void) | null = null;
async function updateTone() {
  const want = ringing ? "ring" : ringback ? "ringback" : speakerTest !== undefined ? "ring" : null;
  if (want === toneNow) return;
  toneNow = want;
  const seq = ++toneSeq;
  webTone?.();
  webTone = null;
  if (want) await applyLevels().catch(() => {}); // tones play at the output volume
  const playback = want ? await savedPlaybackIndex() : null;
  if (seq !== toneSeq) return; // changed again meanwhile: a newer call handles it
  try {
    await api.voiceTone(want, playback);
  } catch {
    // The native engine couldn't play it: fall back to the webview (which may
    // stay silent until the user has interacted with the app).
    if (want && seq === toneSeq) webTone = webAudioTone(want);
  }
}

/** Fallback tones in the webview (generated, no audio files). Returns a stop function. */
function webAudioTone(kind: "ring" | "ringback"): () => void {
  let ctx: AudioContext | null = null;
  let timer: number | undefined;
  try {
    ctx = new AudioContext();
    ctx.resume().catch(() => {});
    const beep = (freqs: number[], start: number, len: number, vol: number) => {
      const t = ctx!.currentTime + start;
      for (const f of freqs) {
        const osc = ctx!.createOscillator();
        const gain = ctx!.createGain();
        osc.frequency.value = f;
        gain.gain.setValueAtTime(0, t);
        gain.gain.linearRampToValueAtTime(vol, t + 0.02);
        gain.gain.setValueAtTime(vol, t + len - 0.03);
        gain.gain.linearRampToValueAtTime(0, t + len);
        osc.connect(gain).connect(ctx!.destination);
        osc.start(t);
        osc.stop(t + len);
      }
    };
    const once =
      kind === "ring"
        ? () => [[660, 0], [880, 0.25], [660, 0.9], [880, 1.15]].forEach(([f, at]) => beep([f], at, 0.22, 0.12))
        : () => beep([440, 480], 0, 2, 0.06);
    once();
    timer = window.setInterval(once, kind === "ring" ? 3000 : 6000);
  } catch {
    // No audio output: the card / call bar alone will do.
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
        mine.listenOnly
          ? iconBtn("No microphone: others can't hear you. Open audio settings to pick one.", "🚫", true, () => audioDialog())
          : iconBtn(mine.muted ? "Unmute" : "Mute", mine.muted ? "🔇" : "🎤", mine.muted, () => voice.toggleMute()),
        iconBtn(mine.deafened ? "Undeafen" : "Deafen", mine.deafened ? "🙉" : "🎧", mine.deafened, () => voice.toggleDeafen()),
        iconBtn("Audio settings", "⚙", false, () => audioDialog()),
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
