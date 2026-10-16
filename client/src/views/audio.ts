import { api, errorMessage, type AudioDevice } from "../api";
import { applyLevels, applySavedDevices, BOOST_STEPS, friendlyName, savedDevices, saveDevices, savedLevels, saveLevels } from "../audio";
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
  const boost = h("select", { class: "select select-sm", attrs: { id: "f-boost", title: "Raises a quiet microphone. Off unless you need it." } }) as HTMLSelectElement;
  for (const db of BOOST_STEPS) {
    const o = h("option", { text: db ? `+${db} dB` : "Off", attrs: { value: String(db) } });
    if (db === levels.boost) o.selected = true;
    boost.append(o);
  }
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
    const l = { mic: Number(micVol.value), boost: Number(boost.value), output: Number(outVol.value) };
    saveLevels(l);
    applyLevels(l).catch((e) => (error.textContent = errorMessage(e)));
  };
  for (const el of [micVol, outVol]) el.addEventListener("input", levelsChanged);
  boost.addEventListener("change", levelsChanged);
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
