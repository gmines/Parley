import { api, errorMessage, type AudioDevice, type SoundSlot } from "../api";
import {
  applyLevels,
  applySavedDevices,
  AUTO_LEAVE_STEPS,
  BOOST_STEPS,
  customSoundNames,
  defaultCallSounds,
  friendlyName,
  MESSAGE_SOUNDS,
  playMessageSound,
  RING_SOUNDS,
  RINGBACK_SOUNDS,
  saveAutoLeave,
  saveCallSounds,
  saveCustomSoundName,
  savedCallSounds,
  savedAutoLeave,
  savedDevices,
  saveDevices,
  savedLevels,
  savedMessageSounds,
  saveLevels,
  saveMessageSounds,
} from "../audio";
import { h } from "../dom";
import { voice } from "../voice";

/** pv_mic_level's scale: the average signal as inverted u-law, so a full
 * scale signal reads 127. */
const LEVEL_MAX = 127;
/** About an eighth of full scale on average: speech peaks start clipping. */
const LEVEL_LOUD = 95;

/**
 * Audio settings: microphone and speaker choice and volume (saved, used by
 * every call), a microphone test with a level meter and optional playback,
 * and a speaker test. Works outside a call; in a call the meter shows the
 * call's microphone and changes apply right away. The device lists are
 * rescanned when the window regains focus (something may have been plugged
 * in). Call dispose() when it's removed.
 */
export function audioPanel(): { el: HTMLElement; dispose: () => void } {
  let devices: AudioDevice[] = [];
  let testing = false;
  let poll: number | undefined;
  let disposed = false;

  const mic = h("select", { class: "select", attrs: { id: "f-mic" } });
  const out = h("select", { class: "select", attrs: { id: "f-out" } });
  const fill = h("div", { class: "meter-fill" });
  const meter = h("div", { class: "meter", attrs: { role: "meter", "aria-label": "Microphone level", "aria-valuemin": "0", "aria-valuemax": "100" } }, fill);
  const meterPct = h("span", { class: "meter-pct", text: "" });
  const loud = h("p", { class: "hint hint-warn", text: "Your microphone is very loud and may sound distorted: lower its volume or boost." });
  loud.hidden = true;

  const levels = savedLevels();
  const slider = (id: string, value: number) => h("input", { class: "range", attrs: { type: "range", id, min: "0", max: "100", step: "1", value: String(value) } }) as HTMLInputElement;
  const micVol = slider("f-mic-vol", levels.mic);
  const outVol = slider("f-out-vol", levels.output);
  const micVolLabel = h("span", { class: "range-value" });
  const outVolLabel = h("span", { class: "range-value" });
  const boostSelect = (id: string, value: number, title: string) => {
    const s = h("select", { class: "select select-sm", attrs: { id, title } }) as HTMLSelectElement;
    for (const db of BOOST_STEPS) {
      const o = h("option", { text: db ? `+${db} dB` : "Off", attrs: { value: String(db) } });
      if (db === value) o.selected = true;
      s.append(o);
    }
    return s;
  };
  const boost = boostSelect("f-boost", levels.boost, "Raises a quiet microphone. Off unless you need it.");
  const outBoost = boostSelect("f-out-boost", levels.outputBoost, "Raises quiet voices above full volume. Off unless you need it: loud voices may distort.");
  const error = h("p", { class: "form-error", attrs: { role: "alert" } });
  const testMic = h("button", { class: "btn btn-sm btn-ghost", attrs: { type: "button" } });
  const loopback = h("input", { attrs: { type: "checkbox", id: "f-loopback" } });
  const loopbackField = h("label", { class: "check", attrs: { for: "f-loopback" } }, loopback, h("span", { text: "Hear yourself (use headphones)" }));
  const testOut = h("button", { class: "btn btn-sm btn-ghost", attrs: { type: "button" }, text: "Test speakers" });
  const hint = h("p", { class: "hint" });

  const fillSelect = (s: HTMLSelectElement, kind: "inputs" | "outputs", current: string | null) => {
    s.replaceChildren(h("option", { text: "System default", attrs: { value: "" } }));
    for (const d of devices.filter((x) => x[kind] > 0 && friendlyName(x.name) !== null)) {
      const o = h("option", { text: friendlyName(d.name)!, attrs: { value: d.name } });
      if (d.name === current) o.selected = true;
      s.append(o);
    }
  };

  const showLevels = () => {
    micVolLabel.textContent = `${micVol.value}%`;
    outVolLabel.textContent = `${outVol.value}%`;
  };
  const levelsChanged = () => {
    showLevels();
    const l = { mic: Number(micVol.value), boost: Number(boost.value), output: Number(outVol.value), outputBoost: Number(outBoost.value) };
    saveLevels(l);
    applyLevels(l).catch((e) => (error.textContent = errorMessage(e)));
  };
  for (const el of [micVol, outVol]) el.addEventListener("input", levelsChanged);
  for (const el of [boost, outBoost]) el.addEventListener("change", levelsChanged);
  showLevels();

  const render = () => {
    const inCall = voice.inCall();
    testMic.textContent = testing ? "Stop test" : "Test microphone";
    testMic.disabled = inCall;
    loopbackField.hidden = inCall;
    hint.textContent = inCall
      ? "You're in a call: the meter shows your microphone as the call hears it."
      : testing
        ? "Speak: the bar should move."
        : "Changes apply to your next call, or right away if you're in one.";
    if (inCall && voice.current().listenOnly) hint.textContent = "You're in the call without a microphone: pick one above so others can hear you.";
    const live = testing || inCall;
    meter.classList.toggle("meter-off", !live);
    if (live && poll === undefined) {
      let loudSince = 0;
      poll = window.setInterval(async () => {
        const level = await api.voiceMicLevel().catch(() => 0);
        if (disposed) return;
        const pct = Math.min(100, Math.round((level / LEVEL_MAX) * 100));
        fill.style.width = `${pct}%`;
        meter.setAttribute("aria-valuenow", String(pct));
        meterPct.textContent = `${pct}%`;
        // Warn once it stays that loud for a moment, not on a single cough.
        const isLoud = level >= LEVEL_LOUD;
        loudSince = isLoud ? loudSince || Date.now() : 0;
        meter.classList.toggle("meter-loud", isLoud);
        if (isLoud && Date.now() - loudSince > 300) loud.hidden = false;
      }, 100);
    } else if (!live && poll !== undefined) {
      window.clearInterval(poll);
      poll = undefined;
      fill.style.width = "0";
      meterPct.textContent = "";
      meter.classList.remove("meter-loud");
    }
  };

  const startTest = async () => {
    error.textContent = "";
    try {
      await applySavedDevices();
      await api.voiceAudioTest(loopback.checked ? "loopback" : "meter");
      testing = true;
    } catch (e) {
      testing = false;
      error.textContent = errorMessage(e);
    }
    render();
  };
  const stopTest = async () => {
    testing = false;
    render();
    await api.voiceAudioTest(null).catch(() => {});
  };

  const changed = async () => {
    saveDevices({ capture: mic.value || null, playback: out.value || null });
    error.textContent = "";
    loud.hidden = true;
    try {
      await applySavedDevices();
    } catch (e) {
      error.textContent = errorMessage(e);
    }
    await voice.refreshStatus(); // a listen-only call may have a microphone now
  };
  mic.addEventListener("change", changed);
  out.addEventListener("change", changed);
  testMic.addEventListener("click", () => (testing ? stopTest() : startTest()));
  loopback.addEventListener("change", () => {
    if (testing) startTest();
  });
  testOut.addEventListener("click", () => voice.testSpeakers());

  const el = h(
    "div",
    { class: "audio-panel" },
    h("label", { class: "field", attrs: { for: "f-mic" } }, h("span", { text: "Microphone" }), mic),
    h("div", { class: "range-row" }, h("label", { attrs: { for: "f-mic-vol" }, text: "Volume" }), micVol, micVolLabel),
    h("div", { class: "range-row" }, h("label", { attrs: { for: "f-boost" }, text: "Boost" }), boost),
    h("div", { class: "meter-row" }, meter, meterPct),
    loud,
    h("div", { class: "audio-actions" }, testMic, loopbackField),
    h("label", { class: "field", attrs: { for: "f-out" } }, h("span", { text: "Speakers / headphones" }), out),
    h("div", { class: "range-row" }, h("label", { attrs: { for: "f-out-vol" }, text: "Volume" }), outVol, outVolLabel),
    h("div", { class: "range-row" }, h("label", { attrs: { for: "f-out-boost" }, text: "Boost" }), outBoost),
    h("div", { class: "audio-actions" }, testOut),
    hint,
    error,
  );

  const off = voice.subscribe(() => {
    if (voice.inCall() && testing) testing = false; // joining a call ends the test (the engine stops it)
    render();
  });
  // Lists the devices (the engine rescans them first), redrawing the
  // choices only when the list changed (not under an open dropdown).
  let listed = "";
  const loadDevices = () =>
    api
      .voiceDevices()
      .then((r) => {
        const sig = JSON.stringify(r.devices.map((d) => [d.name, d.inputs, d.outputs]));
        if (disposed || sig === listed) return;
        listed = sig;
        devices = r.devices;
        const saved = savedDevices();
        fillSelect(mic, "inputs", saved.capture);
        fillSelect(out, "outputs", saved.playback);
        const noMic = !devices.some((d) => d.inputs > 0);
        error.textContent =
          devices.length === 0 ? "No audio devices were found. Connect one; it shows up here." : noMic ? "No microphone was found. Connect one; it shows up here." : "";
      })
      .catch((e) => (error.textContent = errorMessage(e)));
  const onFocus = () => {
    if (!testing) loadDevices(); // don't rescan under a running test
  };
  window.addEventListener("focus", onFocus);
  // Also every few seconds, for devices plugged in while Parley has focus.
  const rescan = window.setInterval(onFocus, 4000);
  loadDevices();
  render();

  return {
    el,
    dispose: () => {
      disposed = true;
      off();
      window.removeEventListener("focus", onFocus);
      window.clearInterval(rescan);
      window.clearInterval(poll);
      if (testing) api.voiceAudioTest(null).catch(() => {});
    },
  };
}

/**
 * A sound choice: the built-in sounds and "Custom file…", which asks for a
 * file (WAV, MP3, OGG or FLAC) the first time; with a custom sound chosen,
 * the file's name and Change / Remove below. onChange runs once a choice is
 * made (and a file imported).
 */
function soundPicker<T extends string>(opts: {
  id: string;
  slot: SoundSlot;
  builtins: { id: T; label: string }[];
  value: T | "custom";
  /** Chosen when the custom file is removed. */
  fallback: T;
  onChange: (value: T | "custom") => void;
}) {
  const select = h("select", { class: "select select-sm", attrs: { id: opts.id } }) as HTMLSelectElement;
  for (const b of opts.builtins) select.append(h("option", { text: b.label, attrs: { value: b.id } }));
  select.append(h("option", { text: "Custom file…", attrs: { value: "custom" } }));
  select.value = opts.value;
  const file = h("input", { attrs: { type: "file", accept: ".wav,.mp3,.ogg,.oga,.flac,audio/*" } }) as HTMLInputElement;
  file.hidden = true;
  const name = h("span", { class: "custom-sound-name" });
  const change = h("button", { class: "btn btn-sm btn-ghost", attrs: { type: "button" } });
  const remove = h("button", { class: "btn btn-sm btn-ghost", attrs: { type: "button" }, text: "Remove" });
  const row = h("div", { class: "custom-sound" }, name, change, remove, file);
  const status = h("p", { class: "hint", attrs: { role: "status" } });
  status.hidden = true;
  let current = opts.value;
  let busy = false;
  let enabled = true;

  const render = () => {
    const n = customSoundNames()[opts.slot];
    row.hidden = select.value !== "custom";
    name.textContent = n ?? "No file chosen: the default plays.";
    name.title = n ?? "";
    change.textContent = n ? "Change file…" : "Choose file…";
    remove.hidden = !n;
    for (const el of [select, change, remove]) el.disabled = busy || !enabled;
  };
  const choose = (value: T | "custom") => {
    current = value;
    select.value = value;
    render();
    opts.onChange(value);
  };
  const showStatus = (text: string, warn = false) => {
    status.textContent = text;
    status.hidden = !text; // no empty gap in the panel
    status.classList.toggle("hint-warn", warn);
  };

  select.addEventListener("change", () => {
    showStatus("");
    if (select.value === "custom" && !customSoundNames()[opts.slot]) {
      // Stays on the current sound until a file is imported (or the picker is cancelled).
      select.value = current;
      file.click();
      return;
    }
    choose(select.value as T | "custom");
  });
  change.addEventListener("click", () => file.click());
  file.addEventListener("change", async () => {
    const f = file.files?.[0];
    file.value = "";
    if (!f) return;
    busy = true;
    showStatus(`Importing ${f.name}…`);
    render();
    try {
      const r = await api.soundImport(opts.slot, f);
      saveCustomSoundName(opts.slot, f.name);
      showStatus(r.trimmed ? `It's longer than that: Parley plays the first ${Math.round(r.ms / 1000)} seconds.` : "");
      busy = false;
      choose("custom");
    } catch (e) {
      busy = false;
      showStatus(errorMessage(e), true);
      render();
    }
  });
  remove.addEventListener("click", async () => {
    try {
      await api.soundRemove(opts.slot);
      saveCustomSoundName(opts.slot, null);
      showStatus("");
      choose(opts.fallback);
    } catch (e) {
      showStatus(errorMessage(e), true);
    }
  });
  render();

  return {
    select,
    /** The custom file's row and the import status, to place under the select. */
    extra: [row, status],
    setEnabled(on: boolean) {
      enabled = on;
      render();
    },
  };
}

/** Message sounds (#14): on/off, which sound (or a custom file), its volume, and a preview. */
export function messageSoundsPanel(): HTMLElement {
  const s = savedMessageSounds();
  const enabled = h("input", { attrs: { type: "checkbox", id: "f-msg-sound-on" } }) as HTMLInputElement;
  enabled.checked = s.enabled;
  let sound = s.sound;
  const picker = soundPicker({
    id: "f-msg-sound",
    slot: "message",
    builtins: MESSAGE_SOUNDS,
    value: s.sound,
    fallback: "chime",
    onChange: (v) => {
      sound = v;
      changed();
      playMessageSound(current());
    },
  });
  const volume = h("input", { class: "range", attrs: { type: "range", id: "f-msg-vol", min: "0", max: "100", step: "1", value: String(s.volume) } }) as HTMLInputElement;
  const volumeLabel = h("span", { class: "range-value" });
  const preview = h("button", { class: "btn btn-sm btn-ghost", attrs: { type: "button" }, text: "Play" });

  const current = () => ({ enabled: enabled.checked, sound, volume: Number(volume.value) });
  const render = () => {
    volumeLabel.textContent = `${volume.value}%`;
    for (const el of [volume, preview]) el.disabled = !enabled.checked;
    picker.setEnabled(enabled.checked);
  };
  function changed() {
    saveMessageSounds(current());
    render();
  }
  enabled.addEventListener("change", changed);
  volume.addEventListener("input", changed);
  volume.addEventListener("change", () => playMessageSound(current()));
  preview.addEventListener("click", () => playMessageSound(current()));
  render();

  return h(
    "div",
    { class: "audio-panel" },
    h("label", { class: "check", attrs: { for: "f-msg-sound-on" } }, enabled, h("span", { text: "Play a sound for new messages" })),
    h("div", { class: "range-row" }, h("label", { attrs: { for: "f-msg-sound" }, text: "Sound" }), picker.select, preview),
    ...picker.extra,
    h("div", { class: "range-row" }, h("label", { attrs: { for: "f-msg-vol" }, text: "Volume" }), volume, volumeLabel),
    h("p", {
      class: "hint",
      text: "Plays on your speakers / headphones above, unless you're reading that conversation. A custom sound can be a WAV, MP3, OGG or FLAC file; Parley plays up to its first 5 seconds.",
    }),
  );
}

/** Ring tones (#13): for incoming calls and while calling, each with on/off,
 * a built-in sound or a custom file, and a preview; one volume for both. */
export function callSoundsPanel(): HTMLElement {
  const s = savedCallSounds();
  const play = (tone: "ring" | "ringback") => voice.previewTone({ tone, sound: s[tone].sound, volume: s.volume / 100 });
  const groups: { on: HTMLInputElement; picker: ReturnType<typeof soundPicker>; preview: HTMLButtonElement }[] = [];

  const group = <T extends string>(tone: "ring" | "ringback", title: string, builtins: { id: T; label: string }[], fallback: T) => {
    const setting = s[tone] as { enabled: boolean; sound: T | "custom" };
    const on = h("input", { attrs: { type: "checkbox", id: `f-${tone}-on` } }) as HTMLInputElement;
    on.checked = setting.enabled;
    const picker = soundPicker({
      id: `f-${tone}-sound`,
      slot: tone,
      builtins,
      value: setting.sound,
      fallback,
      onChange: (v) => {
        setting.sound = v;
        saveCallSounds(s);
        play(tone);
      },
    });
    const preview = h("button", { class: "btn btn-sm btn-ghost", attrs: { type: "button" }, text: "Play" }) as HTMLButtonElement;
    preview.addEventListener("click", () => play(tone));
    on.addEventListener("change", () => {
      setting.enabled = on.checked;
      saveCallSounds(s);
      render();
    });
    groups.push({ on, picker, preview });
    return [
      h("label", { class: "check", attrs: { for: `f-${tone}-on` } }, on, h("span", { text: title })),
      h("div", { class: "range-row" }, h("label", { attrs: { for: `f-${tone}-sound` }, text: "Sound" }), picker.select, preview),
      ...picker.extra,
    ];
  };

  const volume = h("input", { class: "range", attrs: { type: "range", id: "f-ring-vol", min: "0", max: "100", step: "1", value: String(s.volume) } }) as HTMLInputElement;
  const volumeLabel = h("span", { class: "range-value" });
  volume.addEventListener("input", () => {
    s.volume = Number(volume.value);
    saveCallSounds(s);
    render();
  });
  volume.addEventListener("change", () => play("ring"));

  const restore = h("button", { class: "btn btn-sm btn-ghost", attrs: { type: "button" }, text: "Restore defaults" });
  restore.addEventListener("click", async () => {
    saveCallSounds(defaultCallSounds());
    for (const slot of ["ring", "ringback"] as const) {
      await api.soundRemove(slot).catch(() => {});
      saveCustomSoundName(slot, null);
    }
    el.replaceWith(callSoundsPanel());
  });

  function render() {
    volumeLabel.textContent = `${volume.value}%`;
    for (const g of groups) {
      g.picker.setEnabled(g.on.checked);
      g.preview.disabled = !g.on.checked;
    }
  }

  const el = h(
    "div",
    { class: "audio-panel" },
    ...group("ring", "Ring for incoming calls", RING_SOUNDS, "chirp"),
    ...group("ringback", "Ring while I'm calling someone", RINGBACK_SOUNDS, "ringback"),
    h("div", { class: "range-row" }, h("label", { attrs: { for: "f-ring-vol" }, text: "Volume" }), volume, volumeLabel),
    h("div", { class: "audio-actions" }, restore),
    h("p", {
      class: "hint",
      text: "Plays on your speakers / headphones above. A custom sound can be a WAV, MP3, OGG or FLAC file; Parley loops up to its first 30 seconds.",
    }),
  );
  render();
  return el;
}

/** Leaving calls you've walked away from (#15). */
export function callSettingsPanel(): HTMLElement {
  const away = h("select", { class: "select select-sm", attrs: { id: "f-auto-leave" } }) as HTMLSelectElement;
  const current = savedAutoLeave();
  for (const min of AUTO_LEAVE_STEPS) {
    const o = h("option", { text: min === 0 ? "Never" : min < 60 ? `After ${min} minutes` : `After ${min / 60} hour${min > 60 ? "s" : ""}`, attrs: { value: String(min) } });
    if (min === current) o.selected = true;
    away.append(o);
  }
  away.addEventListener("change", () => saveAutoLeave(Number(away.value)));
  return h(
    "div",
    { class: "audio-panel" },
    h("label", { class: "field", attrs: { for: "f-auto-leave" } }, h("span", { text: "Leave a call when I'm away" }), away),
    h("p", {
      class: "hint",
      text: "Away means no keyboard or mouse use and a silent microphone (on Linux, unless you use GNOME: while the screen is locked). Parley also leaves after 5 minutes alone in a call. Either way, you get a minute's warning to stay. Never turns both off.",
    }),
  );
}

/** The audio settings in a dialog (from the call bar). */
export function audioDialog() {
  const panel = audioPanel();
  const dialog = h("dialog", { class: "dialog" });
  const close = () => {
    panel.dispose();
    dialog.close();
    dialog.remove();
  };
  dialog.append(
    h("h2", { text: "Audio settings" }),
    panel.el,
    h("div", { class: "dialog-actions" }, h("button", { class: "btn btn-primary", text: "Done", on: { click: close } })),
  );
  dialog.addEventListener("cancel", (e) => {
    e.preventDefault();
    close();
  });
  document.body.append(dialog);
  dialog.showModal();
}
