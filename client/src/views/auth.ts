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
  onForgot: (server: string, username: string, features: Features) => void;
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
  const email = input("email", "Email (optional)", "you@example.org", "", "email");
  const phone = input("phone", "Phone (optional)", "+15551234567", "", "tel");
  const password = input("password", "Password", "", "", "password");
  username.input.autocomplete = "username";
  for (const f of [displayName, email, phone]) f.input.required = false;

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
  const contactHint = h("p", { class: "hint", text: "Optional. A verified email or phone lets you reset a forgotten password." });
  const passwordHint = h("p", { class: "hint", text: PASSWORD_HINT });
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
    submit.textContent = reg ? "Create account" : "Log in";
    switchLink.textContent = reg ? "Already have an account? Log in" : "Need an account? Register";
    displayName.field.hidden = !reg;
    email.field.hidden = !(reg && features?.email_recovery);
    phone.field.hidden = !(reg && features?.sms_recovery);
    contactHint.hidden = !(reg && anyRecovery);
    passwordHint.hidden = !reg;
    forgotLink.hidden = reg || !anyRecovery;
    password.input.autocomplete = reg ? "new-password" : "current-password";
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
    if (features) opts.onForgot(server.input.value, username.input.value, features);
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
    email.field,
    phone.field,
    contactHint,
    password.field,
    passwordHint,
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
      const user =
        mode === "login"
          ? await api.login(server.input.value, username.input.value, password.input.value, stay.checked)
          : await api.register(
              server.input.value,
              username.input.value,
              password.input.value,
              displayName.input.value,
              email.field.hidden ? "" : email.input.value,
              phone.field.hidden ? "" : phone.input.value,
              stay.checked,
            );
      opts.onAuthed(user);
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
