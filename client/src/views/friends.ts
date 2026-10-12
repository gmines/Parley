import { api, errorCode, errorMessage, type Blocked, type Friend, type Requests, type UserRef } from "../api";
import { avatar, confirmDialog, h, toast } from "../dom";

type Tab = "all" | "pending" | "blocked" | "add";

// Until the realtime service exists (milestone 2), lists refresh by polling.
const POLL_MS = 15_000;

export interface FriendsView {
  el: HTMLElement;
  /** Called with the pending-incoming count whenever it changes (for the sidebar badge). */
  onPendingChange?: (n: number) => void;
  dispose(): void;
}

/** onSignedOut is called if the session expires while the view is open. */
export function friendsView(onSignedOut: (message: string) => void): FriendsView {
  let tab: Tab = "all";
  let friends: Friend[] = [];
  let requests: Requests = { incoming: [], outgoing: [] };
  let blocked: Blocked[] = [];
  let loaded = false;
  let lastAdd: { kind: "ok" | "error"; text: string } | null = null;

  const tabBar = h("nav", { class: "tabs", attrs: { role: "tablist" } });
  const content = h("section", { class: "content" });
  const el = h(
    "div",
    { class: "main" },
    h("header", { class: "main-header" }, h("h1", { text: "Friends" }), tabBar),
    content,
  );

  const view: FriendsView = { el, dispose };

  async function load() {
    try {
      [friends, requests, blocked] = await Promise.all([api.listFriends(), api.listRequests(), api.listBlocked()]);
      loaded = true;
      render();
    } catch (e) {
      handleError(e);
    }
  }

  function handleError(e: unknown) {
    const code = errorCode(e);
    if (code === "session_expired" || code === "not_logged_in") {
      onSignedOut(errorMessage(e));
      return;
    }
    toast(errorMessage(e), "error");
  }

  /** Runs a mutation, then reloads all lists. */
  async function act(fn: () => Promise<unknown>, success?: string) {
    try {
      await fn();
      if (success) toast(success);
    } catch (e) {
      handleError(e);
    }
    await load();
  }

  function render() {
    view.onPendingChange?.(requests.incoming.length);
    renderTabs();
    switch (tab) {
      case "all":
        return renderAll();
      case "pending":
        return renderPending();
      case "blocked":
        return renderBlocked();
      case "add":
        return renderAdd();
    }
  }

  function renderTabs() {
    const pending = requests.incoming.length;
    const mk = (t: Tab, label: string, extra?: Node) =>
      h(
        "button",
        {
          class: `tab${t === "add" ? " tab-accent" : ""}${tab === t ? " tab-active" : ""}`,
          attrs: { role: "tab", "aria-selected": String(tab === t) },
          on: {
            click: () => {
              tab = t;
              render();
            },
          },
        },
        label,
        extra ?? null,
      );
    tabBar.replaceChildren(
      mk("all", "All"),
      mk("pending", "Pending", pending > 0 ? h("span", { class: "badge", text: String(pending) }) : undefined),
      mk("blocked", "Blocked"),
      mk("add", "Add friend"),
    );
  }

  function renderAll() {
    if (!loaded) return content.replaceChildren(h("p", { class: "empty", text: "Loading…" }));
    if (friends.length === 0) {
      return content.replaceChildren(
        empty("No friends yet.", "Add someone by their username to get started.", "Add friend", () => {
          tab = "add";
          render();
        }),
      );
    }
    content.replaceChildren(
      sectionTitle(`All friends — ${friends.length}`),
      list(
        friends.map((f) =>
          row(
            f.user,
            null,
            actionBtn("Remove", "btn-ghost", async () => {
              const ok = await confirmDialog(
                `Remove ${f.user.display_name}?`,
                `You'll need to send a new friend request to add @${f.user.username} again.`,
                "Remove friend",
              );
              if (ok) await act(() => api.removeFriend(f.user.id), `Removed ${f.user.display_name}`);
            }),
            blockBtn(f.user),
          ),
        ),
      ),
    );
  }

  function renderPending() {
    const { incoming, outgoing } = requests;
    if (incoming.length + outgoing.length === 0) {
      return content.replaceChildren(empty("No pending requests.", "Requests you send or receive show up here."));
    }
    content.replaceChildren(
      incoming.length ? sectionTitle(`Incoming — ${incoming.length}`) : "",
      list(
        incoming.map((r) =>
          row(
            r.from,
            "Incoming friend request",
            actionBtn("Accept", "btn-primary", () =>
              act(() => api.acceptFriendRequest(r.id), `You and ${r.from.display_name} are now friends`),
            ),
            actionBtn("Decline", "btn-ghost", () => act(() => api.deleteFriendRequest(r.id))),
            blockBtn(r.from),
          ),
        ),
      ),
      outgoing.length ? sectionTitle(`Outgoing — ${outgoing.length}`) : "",
      list(
        outgoing.map((r) =>
          row(r.to, "Outgoing friend request", actionBtn("Cancel", "btn-ghost", () => act(() => api.deleteFriendRequest(r.id)))),
        ),
      ),
    );
  }

  function renderBlocked() {
    if (blocked.length === 0) {
      return content.replaceChildren(empty("You haven't blocked anyone.", "Blocked users can't message, call or friend you."));
    }
    content.replaceChildren(
      sectionTitle(`Blocked — ${blocked.length}`),
      list(
        blocked.map((b) =>
          row(b.user, "Blocked", actionBtn("Unblock", "btn-ghost", () => act(() => api.unblockUser(b.user.id), `Unblocked ${b.user.display_name}`))),
        ),
      ),
    );
  }

  function renderAdd() {
    const input = h("input", {
      attrs: { placeholder: "Enter a username", "aria-label": "Username", spellcheck: "false", autocomplete: "off" },
    });
    const send = h("button", { class: "btn btn-primary", text: "Send request", attrs: { type: "submit" } });
    const status = h("p", { class: lastAdd ? `add-status add-${lastAdd.kind}` : "add-status", text: lastAdd?.text ?? "" });
    const form = h(
      "form",
      { class: "add-form" },
      h("h2", { text: "Add friend" }),
      h("p", { class: "hint", text: "You can add friends by their username." }),
      h("div", { class: "add-row" }, input, send),
      status,
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const name = input.value.trim();
      if (!name) return;
      send.disabled = true;
      try {
        const res = await api.sendFriendRequest(name);
        lastAdd = res.friendship
          ? { kind: "ok", text: `You and ${res.friendship.user.display_name} are now friends.` }
          : { kind: "ok", text: `Friend request sent to @${res.request!.to.username}.` };
        input.value = "";
      } catch (err) {
        if (["session_expired", "not_logged_in"].includes(errorCode(err) ?? "")) return handleError(err);
        lastAdd = { kind: "error", text: errorMessage(err) };
      } finally {
        send.disabled = false;
      }
      await load();
    });
    content.replaceChildren(form);
    input.focus();
  }

  function blockBtn(user: UserRef) {
    return actionBtn("Block", "btn-ghost btn-danger-text", async () => {
      const ok = await confirmDialog(
        `Block ${user.display_name}?`,
        `@${user.username} won't be able to message, call or send you friend requests. This also removes them as a friend.`,
        "Block",
      );
      if (ok) await act(() => api.blockUser(user.id), `Blocked ${user.display_name}`);
    });
  }

  const onVisible = () => {
    if (!document.hidden) load();
  };
  const timer = window.setInterval(() => {
    if (!document.hidden) load();
  }, POLL_MS);
  document.addEventListener("visibilitychange", onVisible);

  function dispose() {
    window.clearInterval(timer);
    document.removeEventListener("visibilitychange", onVisible);
  }

  render();
  load();
  return view;
}

function row(user: UserRef, subtitle: string | null, ...actions: HTMLElement[]) {
  return h(
    "li",
    { class: "row" },
    avatar(user),
    h(
      "div",
      { class: "row-text" },
      h("span", { class: "row-name", text: user.display_name }),
      h("span", { class: "row-sub", text: subtitle ? `@${user.username} · ${subtitle}` : `@${user.username}` }),
    ),
    h("div", { class: "row-actions" }, ...actions),
  );
}

function list(items: HTMLElement[]) {
  return items.length ? h("ul", { class: "list" }, ...items) : "";
}

function sectionTitle(text: string) {
  return h("h3", { class: "section-title", text });
}

function actionBtn(label: string, cls: string, fn: () => unknown) {
  const b = h("button", { class: `btn btn-sm ${cls}`, text: label });
  b.addEventListener("click", async () => {
    b.disabled = true;
    try {
      await fn();
    } finally {
      b.disabled = false;
    }
  });
  return b;
}

function empty(title: string, body: string, cta?: string, onCta?: () => void) {
  return h(
    "div",
    { class: "empty" },
    h("p", { class: "empty-title", text: title }),
    h("p", { text: body }),
    cta && onCta ? h("button", { class: "btn btn-primary", text: cta, on: { click: onCta } }) : null,
  );
}
