import { api, errorMessage, type Features, type ResetChannel } from "../api";
import { h } from "../dom";
import { input, PASSWORD_HINT } from "./auth";

// Matches the server's minimum gap between codes.
const RESEND_SECONDS = 60;

/**
 * Forgot-password flow: request a code by email or SMS, then enter it with a
 * new password. onDone returns to the login screen.
 */
export function resetView(
  server: string,
  initialUsername: string,
  features: Features,
  onDone: (opts: { username?: string; notice?: string }) => void,
): HTMLElement {
  const channels: { value: ResetChannel; label: string }[] = [];
  if (features.email_recovery) channels.push({ value: "email", label: "Email" });
  if (features.sms_recovery) channels.push({ value: "sms", label: "Text message (SMS)" });
  let channel: ResetChannel = channels[0].value;

  const card = h("div", { class: "auth-card" });
  const back = () => h("button", { class: "link", attrs: { type: "button" }, text: "Back to login", on: { click: () => onDone({}) } });

  // Step 1: username + channel ------------------------------------------------
  function step1() {
    const username = input("reset_user", "Username", "", initialUsername);
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const send = h("button", { class: "btn btn-primary btn-block", attrs: { type: "submit" }, text: "Send reset code" });

    const choices =
      channels.length > 1
        ? h(
            "fieldset",
            { class: "choices" },
            h("legend", { text: "Send the code by" }),
            ...channels.map((c) => {
              const radio = h("input", { attrs: { type: "radio", name: "channel", value: c.value } });
              radio.checked = c.value === channel;
              radio.addEventListener("change", () => (channel = c.value));
              return h("label", { class: "choice" }, radio, h("span", { text: c.label }));
            }),
          )
        : h("p", { class: "hint", text: `We'll send a code to the ${channel === "email" ? "email address" : "phone number"} verified on your account.` });

    const form = h(
      "form",
      { class: "auth-card-body" },
      h("h1", { text: "Reset your password" }),
      username.field,
      choices,
      error,
      send,
      back(),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      send.disabled = true;
      error.textContent = "";
      try {
        await api.requestPasswordReset(server, username.input.value, channel);
        step2(username.input.value.trim());
      } catch (err) {
        error.textContent = errorMessage(err);
      } finally {
        send.disabled = false;
      }
    });
    card.replaceChildren(form);
    queueMicrotask(() => username.input.focus());
  }

  // Step 2: code + new password -------------------------------------------------
  function step2(username: string) {
    const code = input("reset_code", "Code", "123456", "");
    code.input.inputMode = "numeric";
    code.input.autocomplete = "one-time-code";
    code.input.maxLength = 6;
    const password = input("reset_password", "New password", "", "", "password");
    password.input.autocomplete = "new-password";
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const submit = h("button", { class: "btn btn-primary btn-block", attrs: { type: "submit" }, text: "Set new password" });
    const resend = h("button", { class: "link link-sm", attrs: { type: "button" } });

    let wait = RESEND_SECONDS;
    const tick = () => {
      resend.disabled = wait > 0;
      resend.textContent = wait > 0 ? `Resend code (${wait}s)` : "Resend code";
      if (wait-- > 0) timer = window.setTimeout(tick, 1000);
    };
    let timer = 0;
    tick();
    resend.addEventListener("click", async () => {
      resend.disabled = true;
      try {
        await api.requestPasswordReset(server, username, channel);
        wait = RESEND_SECONDS;
        tick();
      } catch (err) {
        error.textContent = errorMessage(err);
        resend.disabled = false;
      }
    });

    const form = h(
      "form",
      { class: "auth-card-body" },
      h("h1", { text: "Enter your code" }),
      h("p", {
        class: "hint",
        text: `If @${username} has a verified ${channel === "email" ? "email address" : "phone number"}, a 6-digit code is on its way. It expires in 15 minutes.`,
      }),
      code.field,
      password.field,
      h("p", { class: "hint", text: PASSWORD_HINT }),
      error,
      submit,
      h("div", { class: "auth-links" }, back(), resend),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      submit.disabled = true;
      error.textContent = "";
      try {
        await api.confirmPasswordReset(server, username, code.input.value, password.input.value);
        window.clearTimeout(timer);
        onDone({ username, notice: "Password reset. You've been signed out everywhere; log in with your new password." });
      } catch (err) {
        error.textContent = errorMessage(err);
      } finally {
        submit.disabled = false;
      }
    });
    card.replaceChildren(form);
    queueMicrotask(() => code.input.focus());
  }

  step1();
  return h("main", { class: "auth-screen" }, card);
}
