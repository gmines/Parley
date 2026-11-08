import { api, errorMessage, type MfaSetup, type User } from "../api";
import { h } from "../dom";
import { input } from "./auth";

function qrCanvas(setup: MfaSetup): HTMLCanvasElement {
  const quiet = 4; // the blank margin scanners need
  const side = setup.size + quiet * 2;
  const canvas = h("canvas", { class: "mfa-qr", attrs: { width: String(side), height: String(side), role: "img", "aria-label": "QR code for your authenticator app" } });
  const ctx = canvas.getContext("2d")!;
  ctx.fillStyle = "#fff";
  ctx.fillRect(0, 0, side, side);
  ctx.fillStyle = "#000";
  setup.qr.forEach((dark, i) => {
    if (dark) ctx.fillRect((i % setup.size) + quiet, Math.floor(i / setup.size) + quiet, 1, 1);
  });
  return canvas;
}

/**
 * Mandatory authenticator setup: there's no way into the app past this screen
 * except finishing it (the server enforces that; this is only the UI for it).
 */
export function mfaSetupView(onDone: (u: User) => void, onLogout: () => void): HTMLElement {
  const card = h("div", { class: "auth-card" }, h("p", { class: "hint", text: "Preparing setup…" }));
  const brand = () => h("div", { class: "brand brand-lg", text: "Parley" });
  const logout = () => h("div", { class: "auth-links" }, h("button", { class: "link", attrs: { type: "button" }, text: "Log out", on: { click: onLogout } }));

  function showRecoveryCodes(user: User, codes: string[]) {
    const saved = h("input", { attrs: { type: "checkbox", id: "f-saved" } });
    const finish = h("button", { class: "btn btn-primary btn-block", attrs: { type: "button", disabled: "" }, text: "Continue to Parley", on: { click: () => onDone(user) } });
    saved.addEventListener("change", () => (finish.disabled = !saved.checked));
    const copy = h("button", {
      class: "btn btn-sm btn-ghost",
      attrs: { type: "button" },
      text: "Copy codes",
      on: {
        click: async () => {
          await navigator.clipboard.writeText(codes.join("\n")).catch(() => {});
          copy.textContent = "Copied";
        },
      },
    });
    card.replaceChildren(
      brand(),
      h("h1", { text: "Save your recovery codes" }),
      h("p", { class: "hint", text: "Each code works once if you lose your authenticator. They won't be shown again, so keep them somewhere safe." }),
      h("div", { class: "mfa-codes" }, ...codes.map((c) => h("span", { text: c }))),
      copy,
      h("label", { class: "check", attrs: { for: "f-saved" } }, saved, h("span", { text: "I've saved these codes" })),
      finish,
    );
  }

  function showSetup(setup: MfaSetup) {
    const code = input("mfa_enroll", "6-digit code", "123456", "");
    code.input.inputMode = "numeric";
    code.input.autocomplete = "one-time-code";
    code.input.maxLength = 6;
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const submit = h("button", { class: "btn btn-primary btn-block", attrs: { type: "submit" }, text: "Turn on two-factor" });
    const form = h(
      "form",
      { class: "auth-card-body" },
      brand(),
      h("h1", { text: "Set up two-factor authentication" }),
      h("p", { class: "hint", text: "Parley now requires this on every account. Scan the code with an authenticator app such as Aegis, Google Authenticator or 1Password." }),
      qrCanvas(setup),
      h("p", { class: "hint", text: "Can't scan it? Enter this key instead:" }),
      h("p", { class: "mfa-secret", text: setup.secret }),
      code.field,
      error,
      submit,
      logout(),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      error.textContent = "";
      submit.disabled = true;
      try {
        const out = await api.mfaEnrollConfirm(code.input.value);
        showRecoveryCodes(out.user, out.recovery_codes);
      } catch (err) {
        error.textContent = errorMessage(err);
        submit.disabled = false;
      }
    });
    card.replaceChildren(form);
    code.input.focus();
  }

  api
    .mfaEnrollStart()
    .then(showSetup)
    .catch((err) => card.replaceChildren(brand(), h("p", { class: "form-error", text: errorMessage(err) }), logout()));
  return h("main", { class: "auth-screen" }, card);
}
