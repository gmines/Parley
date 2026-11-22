"use strict";

// Parley admin app. Plain DOM building only (no innerHTML), so nothing a user
// typed into their profile can run here.

const root = document.getElementById("root");
let me = null;

// --- helpers -------------------------------------------------------------------

function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "text") el.textContent = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k in el && k !== "list") el[k] = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

class ApiError extends Error {
  constructor(status, body) {
    super((body && body.message) || `Request failed (${status})`);
    this.status = status;
  }
}

async function api(method, path, body) {
  const opts = { method, headers: { "X-Parley-Admin": "1" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch("/api" + path, opts);
  if (res.status === 204) return null;
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    if (res.status === 401 && path !== "/login") {
      me = null;
      showLogin("Your session ended. Sign in again.");
    }
    throw new ApiError(res.status, data);
  }
  return data;
}

function fmtDate(s) {
  if (!s) return "—";
  const d = new Date(s);
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function chip(text, kind) {
  return h("span", { class: "chip" + (kind ? " " + kind : ""), text });
}

let toastTimer;
function toast(msg) {
  document.querySelector(".toast")?.remove();
  const t = h("div", { class: "toast", role: "status", text: msg });
  document.body.append(t);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.remove(), 3500);
}

/** Runs fn with the button disabled, showing failures in errEl. */
async function busy(btn, errEl, fn) {
  if (errEl) errEl.textContent = "";
  btn.disabled = true;
  try {
    return await fn();
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) return;
    if (errEl) errEl.textContent = e.message;
    else toast(e.message);
  } finally {
    btn.disabled = false;
  }
}

function field(label, input, hint) {
  const id = "f" + Math.random().toString(36).slice(2);
  input.id = id;
  return h("div", {}, h("label", { for: id, text: label }), input, hint ? h("div", { class: "hint", text: hint }) : null);
}

function textInput(value, props) {
  return h("input", { type: "text", value: value ?? "", autocomplete: "off", spellcheck: false, ...props });
}

function select(value, options) {
  const s = h("select", {}, options.map(([v, label]) => h("option", { value: v, text: label })));
  s.value = value;
  return s;
}

// --- shell ---------------------------------------------------------------------

function showLogin(notice) {
  const username = textInput("", { autocomplete: "username" });
  const password = h("input", { type: "password", autocomplete: "current-password" });
  const code = textInput("", { inputMode: "numeric", autocomplete: "one-time-code", maxLength: 6, placeholder: "123456" });
  const err = h("p", { class: "error", role: "alert" });
  const go = h("button", { class: "primary", type: "submit", text: "Sign in" });
  const form = h("form", { class: "card login" },
    h("h1", { text: "Parley Admin" }),
    notice ? h("p", { class: "notice warn", text: notice }) : null,
    field("Username", username),
    field("Password", password),
    field("Authenticator code", code),
    err,
    h("div", { class: "actions" }, go),
  );
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    busy(go, err, async () => {
      me = await api("POST", "/login", { username: username.value, password: password.value, code: code.value });
      route();
    });
  });
  root.replaceChildren(h("main", { class: "center" }, form));
  username.focus();
}

function shell(active, ...content) {
  const link = (href, text) => h("a", { href, text, class: active === href ? "active" : null });
  const out = h("button", {
    text: "Sign out",
    onclick: async () => {
      await api("POST", "/logout").catch(() => {});
      me = null;
      showLogin();
    },
  });
  root.replaceChildren(
    h("header", { class: "top" },
      h("div", { class: "brand" }, "Parley ", h("span", { text: "Admin" })),
      h("nav", {}, link("#/users", "Users"), link("#/settings", "Settings"), link("#/audit", "Audit log"), link("#/account", "Your account")),
      h("div", { class: "who" }, h("span", { text: me.username }), out),
    ),
    h("main", { class: "page" }, ...content),
  );
}

async function route() {
  if (!me) {
    try {
      me = await api("GET", "/me");
    } catch {
      if (!me) showLogin();
      return;
    }
  }
  const hash = location.hash || "#/users";
  const m = hash.match(/^#\/users\/([0-9a-f-]{36})$/);
  try {
    if (m) await userPage(m[1]);
    else if (hash.startsWith("#/settings")) await settingsPage();
    else if (hash.startsWith("#/audit")) await auditPage();
    else if (hash.startsWith("#/account")) accountPage();
    else await usersPage();
  } catch (e) {
    if (!(e instanceof ApiError && e.status === 401)) {
      shell("", h("div", { class: "card" }, h("p", { class: "error", text: e.message })));
    }
  }
}
window.addEventListener("hashchange", route);

// --- users ---------------------------------------------------------------------

let userQuery = "";
let userOffset = 0;
const PAGE = 50;

function mfaChip(u) {
  if (u.mfa_enabled) return chip("MFA on", "ok");
  if (u.mfa_required) return chip("MFA pending", "warn");
  return chip("MFA off");
}

function statusChip(u) {
  if (u.disabled_at) return chip("Disabled", "bad");
  if (!u.activated) return chip("Not activated", "warn");
  return chip("Active", "ok");
}

function contactCell(value, verified) {
  if (!value) return h("span", { class: "muted", text: "—" });
  return h("span", {}, value, " ", verified ? chip("verified", "ok") : chip("unverified", "warn"));
}

async function usersPage() {
  const search = h("input", { type: "search", placeholder: "Search username, name, email or phone", value: userQuery });
  const tbody = h("tbody");
  const pager = h("div", { class: "pager" });
  const newUserPanel = h("div");
  const newUserBtn = h("button", { class: "primary", text: "New user", onclick: () => toggleCreateForm() });
  const card = h("div", { class: "card" },
    h("div", { class: "toolbar" }, search, newUserBtn),
    newUserPanel,
    h("div", { class: "table-wrap" },
      h("table", {},
        h("thead", {}, h("tr", {}, ["User", "Email", "Phone", "MFA", "Status", "Last active"].map((t) => h("th", { text: t })))),
        tbody,
      ),
    ),
    pager,
  );
  shell("#/users", h("h1", { text: "Users" }), card);

  function toggleCreateForm() {
    if (newUserPanel.firstChild) {
      newUserPanel.replaceChildren();
      newUserBtn.textContent = "New user";
      return;
    }
    newUserBtn.textContent = "Cancel";
    const username = textInput("", { autocomplete: "off", maxLength: 32, placeholder: "jsmith" });
    const name = textInput("", { maxLength: 64, placeholder: "Jane Smith" });
    const email = textInput("", { type: "email", autocomplete: "off", placeholder: "name@example.org" });
    const err = h("p", { class: "error", role: "alert" });
    const out = h("div");
    const create = h("button", { class: "primary", type: "submit", text: "Create user" });
    const form = h("form", { class: "subform" },
      field("Username", username, "Lowercase a–z, 0–9, '_' or '.'; 2–32 characters. The user keeps this."),
      field("Display name", name, "Optional; defaults to the username."),
      field("Email", email, "The activation link is sent here if email delivery is on."),
      err,
      h("div", { class: "actions" }, create),
      out,
    );
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      busy(create, err, async () => {
        const r = await api("POST", "/users", { username: username.value, display_name: name.value, email: email.value });
        const box = h("input", { type: "text", readOnly: true, value: r.url });
        const copy = h("button", {
          type: "button", text: "Copy",
          onclick: async () => { try { await navigator.clipboard.writeText(r.url); toast("Copied."); } catch { box.select(); } },
        });
        const sent = r.emailed
          ? `A welcome email with this link was sent to ${email.value}. You can also hand the link over directly.`
          : `Email delivery is off, so no message was sent. Give this link to @${r.user.username} privately.`;
        out.replaceChildren(
          h("div", { class: "notice ok" },
            h("p", { text: `Created @${r.user.username}. ${sent}` }),
            h("p", { class: "small", text: `It works once and expires ${fmtDate(r.expires_at)}. The user chooses a password and sets up MFA on first sign-in.` }),
            h("div", { class: "linkbox" }, box, copy),
          ),
        );
        username.value = name.value = email.value = "";
        userOffset = 0;
        load();
        box.select();
      });
    });
    newUserPanel.replaceChildren(form);
    username.focus();
  }

  async function load() {
    const data = await api("GET", `/users?q=${encodeURIComponent(userQuery)}&limit=${PAGE}&offset=${userOffset}`);
    tbody.replaceChildren(...data.users.map((u) =>
      h("tr", { class: "clickable", onclick: () => (location.hash = "#/users/" + u.id) },
        h("td", {}, h("div", { text: u.display_name }), h("div", { class: "muted small", text: "@" + u.username })),
        h("td", {}, contactCell(u.email, u.email_verified)),
        h("td", {}, contactCell(u.phone, u.phone_verified)),
        h("td", {}, mfaChip(u)),
        h("td", {}, statusChip(u)),
        h("td", { class: "small", text: fmtDate(u.last_active) }),
      )));
    if (!data.users.length) tbody.append(h("tr", {}, h("td", { colSpan: 6, class: "muted", text: "No users match." })));
    const from = data.total ? userOffset + 1 : 0;
    const to = Math.min(userOffset + PAGE, data.total);
    const prev = h("button", { text: "Previous", disabled: userOffset === 0, onclick: () => { userOffset -= PAGE; load(); } });
    const next = h("button", { text: "Next", disabled: to >= data.total, onclick: () => { userOffset += PAGE; load(); } });
    pager.replaceChildren(h("span", { class: "muted", text: `${from}–${to} of ${data.total}` }), prev, next);
  }
  let t;
  search.addEventListener("input", () => {
    clearTimeout(t);
    t = setTimeout(() => { userQuery = search.value; userOffset = 0; load(); }, 250);
  });
  await load();
  search.focus();
}

async function userPage(id) {
  let u = await api("GET", "/users/" + id);
  const page = h("div");
  shell("#/users", h("p", {}, h("a", { href: "#/users", text: "← All users" })), page);

  /** Runs a user action and redraws with the result. */
  function act(btn, errEl, method, path, body, done) {
    return busy(btn, errEl, async () => {
      u = await api(method, "/users/" + id + path, body);
      render();
      if (done) toast(done);
    });
  }

  function render() {
    page.replaceChildren(
      h("h1", {}, u.display_name, " ", h("span", { class: "muted", text: "@" + u.username })),
      h("div", { class: "grid2" }, profileCard(), accessCard()),
      h("div", { class: "grid2" }, contactCard("email", "Email", u.email, u.email_verified), contactCard("phone", "Phone (SMS)", u.phone, u.phone_verified)),
      h("div", { class: "grid2" }, mfaCard(), passwordCard()),
    );
  }

  function profileCard() {
    const name = textInput(u.display_name, { maxLength: 64 });
    const err = h("p", { class: "error", role: "alert" });
    const save = h("button", { class: "primary", text: "Save", onclick: () => act(save, err, "PATCH", "", { display_name: name.value }, "Display name saved.") });
    return h("section", { class: "card" },
      h("h2", { text: "Profile" }),
      h("dl", { class: "kv" },
        h("dt", { text: "Username" }), h("dd", { class: "mono", text: u.username }),
        h("dt", { text: "User ID" }), h("dd", { class: "mono small", text: u.id }),
        h("dt", { text: "Created" }), h("dd", { text: fmtDate(u.created_at) }),
        h("dt", { text: "Last active" }), h("dd", { text: fmtDate(u.last_active) }),
      ),
      field("Display name", name),
      err,
      h("div", { class: "actions" }, save),
    );
  }

  function accessCard() {
    const err = h("p", { class: "error", role: "alert" });
    const revoke = h("button", {
      text: "Sign out everywhere",
      onclick: () => confirm(`Sign @${u.username} out of every device?`) && act(revoke, err, "POST", "/sessions/revoke", undefined, "Signed out everywhere."),
    });
    const toggle = u.disabled_at
      ? h("button", { text: "Enable account", onclick: () => act(toggle, err, "POST", "/enable", undefined, "Account enabled.") })
      : h("button", {
          class: "danger", text: "Disable account",
          onclick: () => confirm(`Disable @${u.username}? They're signed out and can't sign in until you enable the account again.`) &&
            act(toggle, err, "POST", "/disable", undefined, "Account disabled."),
        });
    return h("section", { class: "card" },
      h("h2", { text: "Access" }),
      h("dl", { class: "kv" },
        h("dt", { text: "Status" }), h("dd", {}, statusChip(u), u.disabled_at ? h("span", { class: "muted small", text: " since " + fmtDate(u.disabled_at) }) : null),
        h("dt", { text: "Signed-in devices" }), h("dd", { text: String(u.sessions) }),
      ),
      h("p", { class: "hint", text: "Sign-outs take effect within the access token lifetime, when each device next refreshes." }),
      err,
      h("div", { class: "actions" }, revoke, toggle),
    );
  }

  function contactCard(kind, title, value, verified) {
    const input = textInput(value, { type: kind === "email" ? "email" : "text", placeholder: kind === "email" ? "name@example.org" : "+15551234567" });
    const markVerified = h("input", { type: "checkbox", checked: true });
    const err = h("p", { class: "error", role: "alert" });
    const save = h("button", {
      class: "primary", text: value ? "Change" : "Add",
      onclick: () => act(save, err, "PUT", "/contact/" + kind, { value: input.value, verified: markVerified.checked },
        markVerified.checked ? "Saved as verified." : "Saved. A verification code was sent."),
    });
    const remove = value
      ? h("button", {
          class: "danger", text: "Remove",
          onclick: () => confirm(`Remove this ${kind === "email" ? "email address" : "phone number"}? Password reset won't be able to use it.`) &&
            act(remove, err, "DELETE", "/contact/" + kind, undefined, "Removed."),
        })
      : null;
    return h("section", { class: "card" },
      h("div", { class: "head" }, h("h2", { text: title }), value ? (verified ? chip("verified", "ok") : chip("unverified", "warn")) : chip("none")),
      field(kind === "email" ? "Email address" : "Phone number (international format)", input),
      h("label", { class: "inline" }, markVerified, "Mark as verified"),
      h("div", { class: "hint", text: "Unchecked: the user is sent a code to confirm it in the app. If an old verified address is replaced, it's notified." }),
      err,
      h("div", { class: "actions" }, save, remove),
    );
  }

  function mfaCard() {
    const policy = select(u.mfa_policy, [
      ["inherit", "Use the server default"],
      ["required", "Always require"],
      ["exempt", "Don't require"],
    ]);
    const err = h("p", { class: "error", role: "alert" });
    const save = h("button", { class: "primary", text: "Save policy", onclick: () => act(save, err, "PUT", "/mfa-policy", { policy: policy.value }, "MFA policy saved.") });
    const reset = u.mfa_enabled
      ? h("button", {
          class: "danger", text: "Reset MFA",
          onclick: () => confirm(`Remove @${u.username}'s authenticator and recovery codes and sign them out? ${u.mfa_required ? "They'll set up a new one at their next sign-in." : ""}`) &&
            act(reset, err, "POST", "/mfa/reset", undefined, "MFA reset."),
        })
      : null;
    let state = "Not set up. MFA is optional for this account.";
    if (u.mfa_enabled) state = "An authenticator app is set up.";
    else if (u.mfa_required) state = "Required but not set up yet: at their next sign-in or refresh they must set up an authenticator app before they can use Parley.";
    return h("section", { class: "card" },
      h("div", { class: "head" }, h("h2", { text: "Two-factor authentication" }), mfaChip(u)),
      h("p", { class: "muted", text: state }),
      field("Require MFA", policy, "Tightening this takes effect at each device's next token refresh. Supported apps: Google Authenticator, Microsoft Authenticator, 1Password, Authy and other TOTP apps."),
      err,
      h("div", { class: "actions" }, save, reset),
    );
  }

  function passwordCard() {
    const err = h("p", { class: "error", role: "alert" });
    const out = h("div");
    const canSend = (u.email && u.email_verified) || (u.phone && u.phone_verified);
    const send = h("button", {
      text: "Send reset link", disabled: !canSend,
      onclick: () => busy(send, err, async () => {
        const r = await api("POST", `/users/${id}/password/send-link`);
        toast(`Reset link sent by ${r.channel === "sms" ? "SMS" : "email"}.`);
      }),
    });
    const create = h("button", {
      text: "Create link to hand over",
      onclick: () => busy(create, err, async () => {
        const r = await api("POST", `/users/${id}/password/create-link`);
        const box = h("input", { type: "text", readOnly: true, value: r.url });
        const copy = h("button", {
          text: "Copy",
          onclick: async () => {
            try { await navigator.clipboard.writeText(r.url); toast("Copied."); } catch { box.select(); }
          },
        });
        out.replaceChildren(
          h("div", { class: "notice ok" },
            h("p", { text: `Give this link to @${u.username} privately. It works once and expires ${fmtDate(r.expires_at)}. Anyone with it can set the password.` }),
            h("div", { class: "linkbox" }, box, copy),
          ),
        );
        box.select();
      }),
    });
    return h("section", { class: "card" },
      h("h2", { text: "Password" }),
      h("p", { class: "muted", text: "Admins never see or set passwords. The user chooses a new one on the account page; it signs them out everywhere. MFA still applies at their next sign-in." }),
      canSend ? null : h("p", { class: "hint", text: "No verified email or phone, so a link can't be sent. Create one and give it to the user." }),
      err,
      h("div", { class: "actions" }, send, create),
      out,
    );
  }

  render();
}

// --- settings ------------------------------------------------------------------

async function settingsPage() {
  let s = await api("GET", "/settings");
  const page = h("div");
  shell("#/settings", h("h1", { text: "Settings" }), page);

  function render() {
    page.replaceChildren(
      h("p", { class: "muted", text: "Changes apply to the API within about 10 seconds, without a redeploy. Groups not overridden here use the deployment's configuration (Helm values / environment)." }),
      mfaCard(),
      h("div", { class: "grid2" }, sessionsCard(), lockoutCard()),
      emailCard(),
      smsCard(),
    );
  }

  async function save(btn, err, group, body, msg) {
    await busy(btn, err, async () => {
      s = await api("PUT", "/settings/" + group, body);
      render();
      toast(msg || "Saved.");
    });
  }

  function groupHead(title, key) {
    const g = s[key];
    const revert = g.source === "override"
      ? h("button", {
          class: "link", text: "Use deployment value",
          onclick: async (e) => {
            if (!confirm("Remove this override and go back to the deployment's configuration?")) return;
            await busy(e.target, null, async () => { s = await api("DELETE", "/settings/" + key); render(); toast("Reverted."); });
          },
        })
      : null;
    return [
      h("div", { class: "head" },
        h("h2", { text: title }),
        h("span", {}, g.source === "override" ? chip("Overridden", "warn") : chip("Deployment value"), " ", revert),
      ),
      g.meta ? h("p", { class: "hint", text: `Last changed ${fmtDate(g.meta.updated_at)} by ${g.meta.updated_by}` }) : null,
      g.problem ? h("p", { class: "notice warn", text: "Not in effect, the deployment value is used instead: " + g.problem }) : null,
    ];
  }

  function mfaCard() {
    const req = h("input", { type: "checkbox", checked: s.mfa.require });
    const err = h("p", { class: "error", role: "alert" });
    const btn = h("button", {
      class: "primary", text: "Save",
      onclick: () => {
        if (req.checked && !s.mfa.require && !confirm("Require MFA for everyone on the default policy? Users without an authenticator must set one up at their next refresh before they can keep using Parley.")) return;
        save(btn, err, "mfa", { require: req.checked }, "MFA default saved.");
      },
    });
    return h("section", { class: "card" },
      h("h2", { text: "Two-factor authentication" }),
      s.mfa.meta ? h("p", { class: "hint", text: `Last changed ${fmtDate(s.mfa.meta.updated_at)} by ${s.mfa.meta.updated_by}` }) : null,
      h("label", { class: "inline" }, req, "Require MFA for all users"),
      h("p", { class: "hint", text: "Applies to users whose policy is \"Use the server default\". Per-user overrides (on each user's page) win. When on, users without an authenticator must set up Google Authenticator or another TOTP app before they can use Parley. Users on an older client than 0.12 always see the setup screen until they enroll." }),
      err,
      h("div", { class: "actions" }, btn),
    );
  }

  function sessionsCard() {
    const v = s.sessions.value;
    const access = textInput(v.access_ttl);
    const refresh = textInput(v.refresh_ttl);
    const err = h("p", { class: "error", role: "alert" });
    const btn = h("button", { class: "primary", text: "Save", onclick: () => save(btn, err, "sessions", { access_ttl: access.value, refresh_ttl: refresh.value }) });
    return h("section", { class: "card" },
      ...groupHead("Session lifetimes", "sessions"),
      field("Access token lifetime", access, "1m to 24h, e.g. 15m. Also how long a sign-out or disable can take to reach a device."),
      field("Session lifetime", refresh, "How long a device stays signed in without use, 1h to 8784h, e.g. 720h (30 days)."),
      err,
      h("div", { class: "actions" }, btn),
    );
  }

  function lockoutCard() {
    const v = s.lockout.value;
    const max = h("input", { type: "number", min: 0, max: 100, value: v.max_failures });
    const dur = textInput(v.duration);
    const win = textInput(v.window);
    const scope = select(v.scope, [["account_ip", "Per account and IP address"], ["account", "Per account"]]);
    const acct = h("input", { type: "number", min: 0, max: 1000, value: v.account_max_failures });
    const err = h("p", { class: "error", role: "alert" });
    const btn = h("button", {
      class: "primary", text: "Save",
      onclick: () => save(btn, err, "lockout", {
        max_failures: Number(max.value), duration: dur.value, window: win.value, scope: scope.value, account_max_failures: Number(acct.value),
      }),
    });
    return h("section", { class: "card" },
      ...groupHead("Sign-in lockout", "lockout"),
      h("div", { class: "row" },
        field("Failures before lockout", max, "0 turns lockout off."),
        field("Lockout length", dur, "e.g. 5m"),
      ),
      h("div", { class: "row" },
        field("Counting window", win, "Older failures are forgotten."),
        field("Count failures", scope),
      ),
      field("Account-wide cap (all IPs)", acct, "Only with per-IP counting. Slows guessing from many addresses. 0 turns it off."),
      err,
      h("div", { class: "actions" }, btn),
    );
  }

  function testBox(kind, placeholder) {
    const to = textInput("", { placeholder });
    const err = h("p", { class: "error", role: "alert" });
    const btn = h("button", {
      text: "Send test",
      onclick: () => busy(btn, err, async () => { const r = await api("POST", "/settings/test/" + kind, { to: to.value }); toast(r.message); }),
    });
    return h("div", {}, h("label", { text: "Send a test message to" }), h("div", { class: "inline-row" }, to, btn), err);
  }

  function emailCard() {
    const v = s.email.value;
    const mode = select(v.mode === "smtp" ? "smtp" : "off", [["off", "Off"], ["smtp", "SMTP"]]);
    const host = textInput(v.host);
    const port = h("input", { type: "number", min: 1, max: 65535, value: v.port || 587 });
    const tls = select(v.tls || "starttls", [["starttls", "STARTTLS (usually port 587)"], ["implicit", "Implicit TLS (usually port 465)"]]);
    const user = textInput(v.username, { autocomplete: "off" });
    const pass = h("input", { type: "password", autocomplete: "new-password", placeholder: v.has_password ? "Saved; leave blank to keep" : "" });
    const from = textInput(v.from, { placeholder: "Parley <no-reply@example.org>" });
    const err = h("p", { class: "error", role: "alert" });
    const fields = h("div", {},
      h("div", { class: "row" }, field("SMTP host", host), field("Port", port)),
      h("div", { class: "row" }, field("Encryption", tls), field("From", from)),
      h("div", { class: "row" }, field("Username", user, "Leave empty if the server doesn't need sign-in."), field("Password", pass)),
    );
    const sync = () => (fields.hidden = mode.value === "off");
    mode.addEventListener("change", sync);
    sync();
    const btn = h("button", {
      class: "primary", text: "Save",
      onclick: () => save(btn, err, "email", {
        mode: mode.value, host: host.value, port: Number(port.value), tls: tls.value, username: user.value, password: pass.value, from: from.value,
      }),
    });
    return h("section", { class: "card" },
      ...groupHead("Email delivery", "email"),
      v.mode === "log" ? h("p", { class: "notice warn", text: "The deployment uses log mode: messages, including reset links, are written to the server log instead of being sent. Don't use this on a live server." }) : null,
      s.account_url ? null : h("p", { class: "notice warn", text: "The account page isn't configured (PARLEY_ACCOUNT_PUBLIC_URL), so sign-up and reset links can't work. Enable accountWeb in the Helm values first." }),
      field("Mode", mode),
      fields,
      err,
      h("div", { class: "actions" }, btn),
      h("hr", { class: "sep" }),
      testBox("email", "you@example.org"),
    );
  }

  function smsCard() {
    const v = s.sms.value;
    const mode = select(v.mode === "twilio" ? "twilio" : "off", [["off", "Off"], ["twilio", "Twilio"]]);
    const sid = textInput(v.account_sid, { placeholder: "AC…" });
    const token = h("input", { type: "password", autocomplete: "new-password", placeholder: v.has_auth_token ? "Saved; leave blank to keep" : "" });
    const from = textInput(v.from, { placeholder: "+15551234567 or MG…" });
    const err = h("p", { class: "error", role: "alert" });
    const fields = h("div", {},
      h("div", { class: "row" }, field("Account SID", sid), field("Auth token", token)),
      field("From", from, "A Twilio number in international format, or a Messaging Service SID. US numbers need A2P 10DLC registration."),
    );
    const sync = () => (fields.hidden = mode.value === "off");
    mode.addEventListener("change", sync);
    sync();
    const btn = h("button", {
      class: "primary", text: "Save",
      onclick: () => save(btn, err, "sms", { mode: mode.value, account_sid: sid.value, auth_token: token.value, from: from.value }),
    });
    return h("section", { class: "card" },
      ...groupHead("SMS delivery", "sms"),
      v.mode === "log" ? h("p", { class: "notice warn", text: "The deployment uses log mode: texts are written to the server log instead of being sent." }) : null,
      field("Mode", mode),
      fields,
      err,
      h("div", { class: "actions" }, btn),
      testBox("sms", "+15551234567"),
    );
  }

  render();
}

// --- audit log -----------------------------------------------------------------

async function auditPage() {
  const tbody = h("tbody");
  const more = h("button", { text: "Load older" });
  shell("#/audit",
    h("h1", { text: "Audit log" }),
    h("div", { class: "card" },
      h("div", { class: "table-wrap" },
        h("table", {}, h("thead", {}, h("tr", {}, ["When", "Admin", "Action", "Target", "Details", "IP"].map((t) => h("th", { text: t })))), tbody),
      ),
      h("div", { class: "pager" }, more),
    ),
  );
  let before = 0;
  async function load() {
    const { entries } = await api("GET", "/audit" + (before ? "?before=" + before : ""));
    for (const e of entries) {
      const detail = Object.keys(e.detail || {}).length ? JSON.stringify(e.detail) : "";
      tbody.append(h("tr", {},
        h("td", { class: "small", text: fmtDate(e.at) }),
        h("td", { text: e.admin }),
        h("td", { class: "mono", text: e.action }),
        h("td", { text: e.target }),
        h("td", { class: "mono small", text: detail }),
        h("td", { class: "small", text: e.ip }),
      ));
      before = e.id;
    }
    more.hidden = entries.length < 100;
  }
  more.addEventListener("click", () => busy(more, null, load));
  await load();
}

// --- own account ---------------------------------------------------------------

function accountPage() {
  const current = h("input", { type: "password", autocomplete: "current-password" });
  const code = textInput("", { inputMode: "numeric", autocomplete: "one-time-code", maxLength: 6 });
  const next = h("input", { type: "password", autocomplete: "new-password" });
  const again = h("input", { type: "password", autocomplete: "new-password" });
  const err = h("p", { class: "error", role: "alert" });
  const btn = h("button", { class: "primary", type: "submit", text: "Change password" });
  const form = h("form", { class: "card narrow" },
    h("h2", { text: "Change your password" }),
    h("p", { class: "muted", text: "Signs out your other admin sessions. To replace a lost authenticator, run `api admin reset " + me.username + "` on the server." }),
    field("Current password", current),
    field("Authenticator code", code),
    field("New password", next, "At least 8 characters and hard to guess."),
    field("New password again", again),
    err,
    h("div", { class: "actions" }, btn),
  );
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    if (next.value !== again.value) {
      err.textContent = "The new passwords don't match.";
      return;
    }
    busy(btn, err, async () => {
      await api("POST", "/me/password", { current_password: current.value, code: code.value, new_password: next.value });
      form.reset();
      toast("Password changed.");
    });
  });
  shell("#/account", h("h1", { text: "Your account" }), form);
}

route();
