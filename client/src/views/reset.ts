import { api, errorMessage } from "../api";
import { h } from "../dom";
import { input } from "./auth";

/** Forgot-password flow: asks the server to send a reset link. */
export function resetView(
  server: string,
  initialUsername: string,
  onDone: (opts: { username?: string; notice?: string }) => void,
): HTMLElement {
  const identifier = input("reset_user", "Username, email or mobile number", "", initialUsername);
  const error = h("p", { class: "form-error", attrs: { role: "alert" } });
  const send = h("button", { class: "btn btn-primary btn-block", attrs: { type: "submit" }, text: "Send reset link" });
  const form = h(
    "form",
    { class: "auth-card" },
    h("div", { class: "brand brand-lg", text: "PartyLine" }),
    h("h1", { text: "Reset your password" }),
    h("p", { class: "hint", text: "We'll send a link to the verified email or phone on your account." }),
    identifier.field,
    error,
    send,
    h("div", { class: "auth-links" }, h("button", { class: "link", attrs: { type: "button" }, text: "Back to login", on: { click: () => onDone({}) } })),
  );
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    send.disabled = true;
    error.textContent = "";
    try {
      await api.requestPasswordReset(server, identifier.input.value);
      onDone({ notice: "If that account has a verified email or phone, a reset link is on its way. It expires in 30 minutes." });
    } catch (err) {
      error.textContent = errorMessage(err);
    } finally {
      send.disabled = false;
    }
  });
  queueMicrotask(() => identifier.input.focus());
  return h("main", { class: "auth-screen" }, form);
}
