import { api, errorCode, errorMessage, type ContactKind, type Features, type User } from "../api";
import { h, toast } from "../dom";
import { input, PASSWORD_HINT } from "./auth";

/**
 * Account settings: change password and, when the server enables them,
 * recovery email / phone. onUser is called whenever the user record changes;
 * onSignedOut if the session ends.
 */
export function accountView(
  initialUser: User,
  server: string | null,
  onUser: (u: User) => void,
  onSignedOut: (message: string) => void,
): HTMLElement {
  let user = initialUser;
  let features: Features | null = null;

  const recovery = h("section", { class: "settings-section" });
  const el = h(
    "div",
    { class: "main" },
    h("header", { class: "main-header" }, h("h1", { text: "Account" })),
    h("section", { class: "content settings" }, profileSection(), passwordSection(), recovery),
  );

  function setUser(u: User) {
    user = u;
    onUser(u);
    renderRecovery();
  }

  function fail(err: unknown, target: HTMLElement) {
    const code = errorCode(err);
    if (code === "session_expired" || code === "not_logged_in") return onSignedOut(errorMessage(err));
    target.textContent = errorMessage(err);
  }

  function profileSection() {
    return h(
      "section",
      { class: "settings-section" },
      h("h2", { text: "Profile" }),
      h("dl", { class: "kv" }, h("dt", { text: "Display name" }), h("dd", { text: user.display_name }), h("dt", { text: "Username" }), h("dd", { text: `@${user.username}` })),
    );
  }

  function passwordSection() {
    const current = input("cur_password", "Current password", "", "", "password");
    current.input.autocomplete = "current-password";
    const next = input("new_password", "New password", "", "", "password");
    next.input.autocomplete = "new-password";
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const save = h("button", { class: "btn btn-primary", attrs: { type: "submit" }, text: "Change password" });
    const form = h(
      "form",
      { class: "settings-section" },
      h("h2", { text: "Password" }),
      current.field,
      next.field,
      h("p", { class: "hint", text: PASSWORD_HINT }),
      h("p", { class: "hint", text: "Changing your password signs you out on all other devices." }),
      error,
      h("div", { class: "form-actions" }, save),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      error.textContent = "";
      save.disabled = true;
      try {
        setUser(await api.changePassword(current.input.value, next.input.value));
        current.input.value = next.input.value = "";
        toast("Password changed. Other devices have been signed out.");
      } catch (err) {
        fail(err, error);
      } finally {
        save.disabled = false;
      }
    });
    return form;
  }

  function renderRecovery() {
    if (!features) {
      recovery.replaceChildren();
      return;
    }
    const kinds: ContactKind[] = [];
    if (features.email_recovery) kinds.push("email");
    if (features.sms_recovery) kinds.push("phone");
    if (kinds.length === 0) {
      recovery.replaceChildren(
        h("h2", { text: "Account recovery" }),
        h("p", { class: "hint", text: "Password reset by email or text message isn't enabled on this server." }),
      );
      return;
    }
    recovery.replaceChildren(
      h("h2", { text: "Account recovery" }),
      h("p", { class: "hint", text: "A verified email or phone number lets you reset your password if you forget it." }),
      ...kinds.map(contactRow),
    );
  }

  function contactRow(kind: ContactKind): HTMLElement {
    const label = kind === "email" ? "Email" : "Phone";
    const value = kind === "email" ? user.email : user.phone;
    const verified = kind === "email" ? user.email_verified : user.phone_verified;
    const panel = h("div", { class: "contact-panel" });
    const status = !value
      ? h("span", { class: "pill", text: "Not set" })
      : verified
        ? h("span", { class: "pill pill-ok", text: "Verified" })
        : h("span", { class: "pill pill-warn", text: "Not verified" });

    const row = h(
      "div",
      { class: "contact" },
      h(
        "div",
        { class: "contact-head" },
        h("div", { class: "row-text" }, h("span", { class: "row-name", text: label }), h("span", { class: "row-sub", text: value || "—" })),
        status,
        h(
          "div",
          { class: "row-actions" },
          value && !verified
            ? h("button", { class: "btn btn-sm btn-primary", text: "Enter code", on: { click: () => showVerify(kind, panel) } })
            : null,
          h("button", { class: "btn btn-sm btn-ghost", text: value ? "Change" : "Add", on: { click: () => showSet(kind, panel) } }),
          value ? h("button", { class: "btn btn-sm btn-ghost btn-danger-text", text: "Remove", on: { click: () => showRemove(kind, panel) } }) : null,
        ),
      ),
      panel,
    );
    return row;
  }

  function showSet(kind: ContactKind, panel: HTMLElement) {
    const value = input(`${kind}_value`, kind === "email" ? "Email address" : "Phone number", kind === "email" ? "you@example.org" : "+15551234567", "", kind === "email" ? "email" : "tel");
    const password = input(`${kind}_pw`, "Current password", "", "", "password");
    password.input.autocomplete = "current-password";
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const send = h("button", { class: "btn btn-primary btn-sm", attrs: { type: "submit" }, text: "Send code" });
    const form = h(
      "form",
      { class: "inline-form" },
      value.field,
      password.field,
      kind === "phone" ? h("p", { class: "hint", text: "International format, starting with + and the country code." }) : null,
      error,
      h("div", { class: "form-actions" }, cancelBtn(panel), send),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      error.textContent = "";
      send.disabled = true;
      try {
        await api.setContact(kind, value.input.value, password.input.value);
        setUser(await api.fetchMe());
        // renderRecovery rebuilt the rows; open the code form on the new one.
        const fresh = recovery.querySelectorAll<HTMLElement>(".contact-panel")[kindIndex(kind)];
        if (fresh) showVerify(kind, fresh);
        toast("Code sent.");
      } catch (err) {
        fail(err, error);
      } finally {
        send.disabled = false;
      }
    });
    panel.replaceChildren(form);
    value.input.focus();
  }

  function showVerify(kind: ContactKind, panel: HTMLElement) {
    const code = input(`${kind}_code`, "6-digit code", "123456", "");
    code.input.inputMode = "numeric";
    code.input.autocomplete = "one-time-code";
    code.input.maxLength = 6;
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const verify = h("button", { class: "btn btn-primary btn-sm", attrs: { type: "submit" }, text: "Verify" });
    const form = h(
      "form",
      { class: "inline-form" },
      h("p", { class: "hint", text: "Enter the code we sent. It expires in 15 minutes. To get a new one, use Change and enter the same value." }),
      code.field,
      error,
      h("div", { class: "form-actions" }, cancelBtn(panel), verify),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      error.textContent = "";
      verify.disabled = true;
      try {
        setUser(await api.verifyContact(kind, code.input.value));
        toast(`${kind === "email" ? "Email" : "Phone number"} verified.`);
      } catch (err) {
        fail(err, error);
      } finally {
        verify.disabled = false;
      }
    });
    panel.replaceChildren(form);
    code.input.focus();
  }

  function showRemove(kind: ContactKind, panel: HTMLElement) {
    const password = input(`${kind}_rm_pw`, "Current password", "", "", "password");
    password.input.autocomplete = "current-password";
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const remove = h("button", { class: "btn btn-danger btn-sm", attrs: { type: "submit" }, text: "Remove" });
    const form = h(
      "form",
      { class: "inline-form" },
      h("p", { class: "hint", text: `Without a verified ${kind === "email" ? "email" : "phone number"} you can't use it to reset your password.` }),
      password.field,
      error,
      h("div", { class: "form-actions" }, cancelBtn(panel), remove),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      error.textContent = "";
      remove.disabled = true;
      try {
        setUser(await api.removeContact(kind, password.input.value));
        toast("Removed.");
      } catch (err) {
        fail(err, error);
      } finally {
        remove.disabled = false;
      }
    });
    panel.replaceChildren(form);
    password.input.focus();
  }

  function kindIndex(kind: ContactKind) {
    return kind === "email" || !features?.email_recovery ? 0 : 1;
  }

  function cancelBtn(panel: HTMLElement) {
    return h("button", { class: "btn btn-ghost btn-sm", attrs: { type: "button" }, text: "Cancel", on: { click: () => panel.replaceChildren() } });
  }

  if (server) {
    api
      .serverFeatures(server)
      .then((f) => (features = f))
      .catch(() => (features = { email_recovery: false, sms_recovery: false }))
      .finally(renderRecovery);
  }
  api.fetchMe().then(setUser).catch(() => {});

  return el;
}
