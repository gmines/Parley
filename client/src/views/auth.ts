import { api, errorMessage, type Features, type User } from "../api";
import { h } from "../dom";
import { versionLabel } from "./about";

export interface AuthOptions {
  server: string | null;
  username?: string;
  /** "Stay signed in" starts checked unless the user unchecked it last time. */
  staySignedIn?: boolean;
  notice?: string;
  onAuthed: (u: User) => void;
  onForgot: (server: string, username: string) => void;
}

export const PASSWORD_HINT =
  "Use a few unrelated words, like blue-kettle-drift. Common passwords and patterns like 12345678 are rejected.";

/** Login / register screen. */
export function authView(opts: AuthOptions): HTMLElement {
  let mode: "login" | "register" = "login";
  let features: Features | null = null;

  const server = input("server", "Server", "chat.example.org", opts.server ?? "");
  const username = input("username", "Username", "", opts.username ?? "");
  const displayName = input("display_name", "Display name (optional)", "", "");
  const contact = input("contact", "Email", "you@example.org", "");
  const password = input("password", "Password", "", "", "password");
  username.input.autocomplete = "username";
  password.input.autocomplete = "current-password";
  displayName.input.required = false;

  const stay = h("input", { attrs: { type: "checkbox", id: "f-stay" } });
  stay.checked = opts.staySignedIn ?? true;
  const stayField = h(
    "label",
    { class: "check", attrs: { for: "f-stay", title: "Keeps you signed in on this computer (for 30 days without using Parley). Uncheck on a shared computer." } },
    stay,
    h("span", { text: "Stay signed in" }),
  );

  const httpWarning = h("p", {
    class: "hint hint-warn",
    text: "This server address uses plain http. Your password will be sent unencrypted.",
  });
  const contactHint = h("p", { class: "hint", text: "We'll send a link to activate your account and choose a password." });
  const error = h("p", { class: "form-error", attrs: { role: "alert" } });
  const info = h("p", { class: "form-notice", attrs: { role: "status" }, text: opts.notice ?? "" });
  const submit = h("button", { class: "btn btn-primary btn-block", attrs: { type: "submit" } });
  const title = h("h1");
  const switchLink = h("button", { class: "link", attrs: { type: "button" } });
  const forgotLink = h("button", { class: "link link-sm", attrs: { type: "button" }, text: "Forgot password?" });

  const render = () => {
    const reg = mode === "register";
    const anyRecovery = !!features && (features.email_recovery || features.sms_recovery);
    title.textContent = reg ? "Create an account" : "Welcome back";
    submit.textContent = reg ? "Send activation link" : "Log in";
    switchLink.textContent = reg ? "Already have an account? Log in" : "Need an account? Sign up";
    switchLink.hidden = !reg && !features?.signup;
    displayName.field.hidden = !reg;
    contact.field.hidden = !reg;
    contact.input.required = reg;
    const both = features?.email_recovery && features?.sms_recovery;
    (contact.field.firstChild as HTMLElement).textContent = both ? "Email or mobile number" : features?.sms_recovery ? "Mobile number" : "Email";
    contact.input.placeholder = both ? "you@example.org or +15551234567" : features?.sms_recovery ? "+15551234567" : "you@example.org";
    contactHint.hidden = !reg;
    password.field.hidden = reg;
    password.input.required = !reg;
    stayField.hidden = reg;
    forgotLink.hidden = reg || !anyRecovery;
    httpWarning.hidden = !server.input.value.trim().toLowerCase().startsWith("http://");
  };

  // Ask the server which optional flows it supports. Failures just hide them.
  let featuresFor = "";
  let debounce: number | undefined;
  const loadFeatures = () => {
    const target = server.input.value.trim();
    if (!target || target === featuresFor) return;
    featuresFor = target;
    api
      .serverFeatures(target)
      .then((f) => {
        if (featuresFor === target) features = f;
      })
      .catch(() => {
        if (featuresFor === target) features = null;
      })
      .finally(() => {
        if (featuresFor === target) render();
      });
  };
  server.input.addEventListener("input", () => {
    httpWarning.hidden = !server.input.value.trim().toLowerCase().startsWith("http://");
    window.clearTimeout(debounce);
    debounce = window.setTimeout(loadFeatures, 500);
  });
  server.input.addEventListener("change", loadFeatures);

  switchLink.addEventListener("click", () => {
    mode = mode === "login" ? "register" : "login";
    error.textContent = "";
    info.textContent = "";
    render();
  });
  forgotLink.addEventListener("click", () => {
    if (features) opts.onForgot(server.input.value, username.input.value);
  });

  const form = h(
    "form",
    { class: "auth-card" },
    h("div", { class: "brand brand-lg", text: "Parley" }),
    title,
    info,
    server.field,
    httpWarning,
    username.field,
    displayName.field,
    contact.field,
    contactHint,
    password.field,
    stayField,
    error,
    submit,
    h("div", { class: "auth-links" }, switchLink, forgotLink),
    versionFooter(),
  );

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    error.textContent = "";
    info.textContent = "";
    submit.disabled = true;
    try {
      if (mode === "register") {
        const value = contact.input.value.trim();
        const isEmail = value.includes("@");
        await api.signup(server.input.value, username.input.value, displayName.input.value, isEmail ? value : "", isEmail ? "" : value);
        mode = "login";
        render();
        info.textContent = `Check ${isEmail ? "your email" : "your text messages"} for a link to activate your account. It expires in 24 hours.`;
        return;
      }
      const outcome = await api.login(server.input.value, username.input.value, password.input.value, stay.checked);
      if (outcome.user) opts.onAuthed(outcome.user);
      else {
        const step = mfaStep(stay.checked, opts.onAuthed, () => step.replaceWith(form));
        form.replaceWith(step);
      }
    } catch (err) {
      error.textContent = errorMessage(err);
    } finally {
      submit.disabled = false;
    }
  });

  render();
  loadFeatures();
  queueMicrotask(() => (opts.server ? (opts.username ? password : username) : server).input.focus());
  return h("main", { class: "auth-screen" }, form);
}

/** Second step of a login: an authenticator or recovery code. */
function mfaStep(stay: boolean, onAuthed: (u: User) => void, onBack: () => void): HTMLElement {
  const code = input("mfa_code", "Authenticator code", "123456", "");
  code.input.autocomplete = "one-time-code";
  const error = h("p", { class: "form-error", attrs: { role: "alert" } });
  const submit = h("button", { class: "btn btn-primary btn-block", attrs: { type: "submit" }, text: "Verify" });
  const back = h("button", { class: "link", attrs: { type: "button" }, text: "Back", on: { click: onBack } });
  const form = h(
    "form",
    { class: "auth-card" },
    h("div", { class: "brand brand-lg", text: "Parley" }),
    h("h1", { text: "Two-factor authentication" }),
    h("p", { class: "hint", text: "Enter the 6-digit code from your authenticator app. Lost it? Enter one of your recovery codes instead." }),
    code.field,
    error,
    submit,
    h("div", { class: "auth-links" }, back),
  );
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    error.textContent = "";
    submit.disabled = true;
    try {
      onAuthed(await api.mfaLogin(code.input.value, stay));
    } catch (err) {
      error.textContent = errorMessage(err);
      code.input.select();
    } finally {
      submit.disabled = false;
    }
  });
  queueMicrotask(() => code.input.focus());
  return form;
}

export function input(name: string, label: string, placeholder: string, value: string, type = "text") {
  const el = h("input", { attrs: { name, type, placeholder, id: `f-${name}`, spellcheck: "false" } });
  el.value = value;
  el.required = true;
  const field = h("label", { class: "field", attrs: { for: `f-${name}` } }, h("span", { text: label }), el);
  return { field, input: el };
}

/** Small "Parley 0.2.0 (abc123)" line, so testers can tell builds apart before logging in. */
function versionFooter(): HTMLElement {
  const el = h("p", { class: "auth-version" });
  api.appInfo().then((i) => (el.textContent = `Parley ${versionLabel(i)}`)).catch(() => {});
  return el;
}
