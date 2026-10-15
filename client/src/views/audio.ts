import { api, errorMessage, type AudioDevice } from "../api";
import { applySavedDevices, friendlyName, savedDevices, saveDevices } from "../audio";
import { h } from "../dom";
import { voice } from "../voice";

/**
 * Audio settings: microphone and speaker choice (saved, used by every call),
 * a microphone test with a level meter and optional playback, and a speaker
 * test. Works outside a call; in a call the meter shows the call's microphone.
 * Call dispose() when it's removed.
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
    const live = testing || inCall;
    meter.classList.toggle("meter-off", !live);
    if (live && poll === undefined) {
      poll = window.setInterval(async () => {
        const level = await api.voiceMicLevel().catch(() => 0);
        if (!disposed) {
          const pct = Math.min(100, Math.round((level / 200) * 100));
          fill.style.width = `${pct}%`;
          meter.setAttribute("aria-valuenow", String(pct));
        }
      }, 100);
    } else if (!live && poll !== undefined) {
      window.clearInterval(poll);
      poll = undefined;
      fill.style.width = "0";
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
    try {
      await applySavedDevices();
    } catch (e) {
      error.textContent = errorMessage(e);
    }
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
    meter,
    h("div", { class: "audio-actions" }, testMic, loopbackField),
    h("label", { class: "field", attrs: { for: "f-out" } }, h("span", { text: "Speakers / headphones" }), out),
    h("div", { class: "audio-actions" }, testOut),
    hint,
    error,
  );

  const off = voice.subscribe(() => {
    if (voice.inCall() && testing) testing = false; // joining a call ends the test (the engine stops it)
    render();
  });
  api
    .voiceDevices()
    .then((r) => {
      devices = r.devices;
      const saved = savedDevices();
      fillSelect(mic, "inputs", saved.capture);
      fillSelect(out, "outputs", saved.playback);
      if (devices.length === 0) error.textContent = "No audio devices were found.";
    })
    .catch((e) => (error.textContent = errorMessage(e)));
  render();

  return {
    el,
    dispose: () => {
      disposed = true;
      off();
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
