import { api, errorMessage, onRealtimeEvent, onRealtimeStatus, type Conversation, type Features, type RealtimeEvent, type User, type UserRef } from "./api";
import { avatar, h, mount, toast } from "./dom";
import { conversationTitle, dmPeer, store } from "./store";
import { aboutView, versionLabel } from "./views/about";
import { accountView } from "./views/account";
import { authView } from "./views/auth";
import { conversationView, type ConversationView } from "./views/conversation";
import { friendsView, type FriendsView } from "./views/friends";
import { newGroupDialog } from "./views/newgroup";
import { resetView } from "./views/reset";

const root = document.getElementById("app")!;

/** Teardown for the signed-in app (listeners, views). */
let teardown: (() => void) | null = null;

async function start() {
  mount(root, h("main", { class: "splash" }, h("div", { class: "brand brand-lg", text: "Parley" })));
  const server = await api.savedServer();
  try {
    const user = await api.restoreSession();
    if (user) return showApp(user);
    showAuth(server);
  } catch (e) {
    // Couldn't reach the server; the saved session (if any) is kept for next time.
    showAuth(server, { notice: `Couldn't restore your session: ${errorMessage(e)}` });
  }
}

function leaveApp() {
  teardown?.();
  teardown = null;
  store.clear();
}

function showAuth(server: string | null, opts: { username?: string; notice?: string } = {}) {
  leaveApp();
  mount(root, authView({ server, ...opts, onAuthed: showApp, onForgot: showReset }));
}

function showReset(server: string, username: string, features: Features) {
  leaveApp();
  mount(root, resetView(server, username, features, (opts) => showAuth(server, opts)));
}

async function signedOut(message: string) {
  showAuth(await api.savedServer(), { notice: message });
}

type Page = { kind: "friends" } | { kind: "account" } | { kind: "about" } | { kind: "conversation"; id: string };

function showApp(initialUser: User) {
  leaveApp();
  let user = initialUser;
  store.setMe(user);
  let page: Page = { kind: "friends" };
  let friends: FriendsView | null = null;
  let conversation: ConversationView | null = null;

  // Sidebar ----------------------------------------------------------------------
  const pendingBadge = h("span", { class: "badge", attrs: { hidden: "" } });
  const friendsNav = h("button", { class: "side-item", on: { click: () => go({ kind: "friends" }) } }, "Friends", pendingBadge);
  const accountNav = h("button", { class: "side-item", text: "Account", on: { click: () => go({ kind: "account" }) } });
  const aboutNav = h("button", { class: "side-item", text: "About", on: { click: () => go({ kind: "about" }) } });
  const version = h("button", { class: "side-version", attrs: { title: "About Parley" }, on: { click: () => go({ kind: "about" }) } });
  api.appInfo().then((i) => (version.textContent = `Parley ${versionLabel(i)}`)).catch(() => {});
  let realtimeState = "connecting";
  const dmList = h("nav", { class: "dm-list", attrs: { "aria-label": "Direct messages" } });
  const newGroup = h("button", {
    class: "icon-btn",
    text: "+",
    attrs: { title: "New group", "aria-label": "New group" },
    on: {
      click: async () => {
        const g = await newGroupDialog().catch((e) => (toast(errorMessage(e), "error"), null));
        if (g) {
          store.upsert(g);
          go({ kind: "conversation", id: g.id });
        }
      },
    },
  });
  const connection = h("span", { class: "conn-dot", attrs: { title: "Connecting…" } });
  const meName = h("span", { class: "me-name" });
  const meSub = h("span", { class: "me-sub" });
  const meAvatar = h("span", { class: "me-avatar" });
  const renderMe = () => {
    meName.textContent = user.display_name;
    meSub.textContent = `@${user.username}`;
    meAvatar.replaceChildren(avatar(user, "sm"), connection);
  };
  const logout = h("button", {
    class: "btn btn-sm btn-ghost",
    text: "Log out",
    on: {
      click: async () => {
        await api.logout();
        showAuth(await api.savedServer());
      },
    },
  });

  const sidebar = h(
    "aside",
    { class: "sidebar" },
    h("div", { class: "brand", text: "Parley" }),
    h("nav", { class: "side-nav" }, friendsNav, accountNav, aboutNav),
    h("div", { class: "side-section" }, h("span", { text: "Direct messages" }), newGroup),
    dmList,
    h("div", { class: "me" }, meAvatar, h("div", { class: "me-text" }, meName, meSub), logout),
    version,
  );
  const pane = h("div", { class: "pane" });
  mount(root, h("div", { class: "shell" }, sidebar, pane));
  renderMe();

  function renderDmList() {
    const convs = store.list();
    if (convs.length === 0) {
      dmList.replaceChildren(h("p", { class: "side-empty", text: "No conversations yet. Message a friend from the Friends page." }));
      return;
    }
    dmList.replaceChildren(
      ...convs.map((c) => {
        const active = page.kind === "conversation" && page.id === c.id;
        const peer = dmPeer(c);
        const preview = c.last_message
          ? `${c.last_message.author?.id === user.id ? "You: " : c.kind === "group" ? `${c.last_message.author?.display_name ?? "Deleted user"}: ` : ""}${c.last_message.content}`
          : c.kind === "group"
            ? `${c.members.length} members`
            : "";
        return h(
          "button",
          {
            class: `dm-item${active ? " dm-item-active" : ""}${c.unread_count ? " dm-item-unread" : ""}`,
            attrs: active ? { "aria-current": "page" } : {},
            on: { click: () => go({ kind: "conversation", id: c.id }) },
          },
          peer ? avatar(peer, "sm") : h("div", { class: "avatar avatar-sm avatar-group", text: "#", attrs: { "aria-hidden": "true" } }),
          h("span", { class: "row-text" }, h("span", { class: "row-name", text: conversationTitle(c) }), h("span", { class: "row-sub", text: preview })),
          c.unread_count ? h("span", { class: "badge", text: c.unread_count >= 100 ? "99+" : String(c.unread_count) }) : null,
        );
      }),
    );
  }
  const unsubscribeStore = store.subscribe(renderDmList);

  // Pages --------------------------------------------------------------------------
  function go(next: Page) {
    page = next;
    friendsNav.classList.toggle("side-item-active", page.kind === "friends");
    accountNav.classList.toggle("side-item-active", page.kind === "account");
    aboutNav.classList.toggle("side-item-active", page.kind === "about");
    friends?.dispose();
    conversation?.dispose();
    friends = null;
    conversation = null;

    if (page.kind === "friends") {
      friends = friendsView(signedOut, openDm);
      friends.onPendingChange = setPending;
      pane.replaceChildren(friends.el);
    } else if (page.kind === "account") {
      api.savedServer().then((server) => {
        if (page.kind !== "account") return;
        pane.replaceChildren(
          accountView(user, server, (u) => {
            user = u;
            store.setMe(u);
            renderMe();
          }, signedOut),
        );
      });
    } else if (page.kind === "about") {
      api.savedServer().then((server) => {
        if (page.kind === "about") pane.replaceChildren(aboutView(server, user.username, () => statusTitle[realtimeState] ?? realtimeState));
      });
    } else {
      const c = store.get(page.id);
      if (c) {
        conversation = conversationView(c, signedOut);
        pane.replaceChildren(conversation.el);
      }
    }
    renderDmList();
  }

  async function openDm(u: UserRef) {
    try {
      const c = await api.openDm(u.id);
      store.upsert(c);
      go({ kind: "conversation", id: c.id });
    } catch (e) {
      toast(errorMessage(e), "error");
    }
  }

  function setPending(n: number) {
    pendingBadge.hidden = n === 0;
    pendingBadge.textContent = String(n);
  }
  const refreshPending = () => api.listRequests().then((r) => setPending(r.incoming.length)).catch(() => {});

  // Realtime ----------------------------------------------------------------------
  function onEvent(e: RealtimeEvent) {
    switch (e.type) {
      case "ready":
        // (Re)connected: anything could have happened while we were away.
        store.load().catch(() => {});
        conversation?.resync();
        friends?.reload();
        refreshPending();
        break;
      case "message.created": {
        const open = conversation?.id === e.data.conversation_id;
        if (open) conversation!.addMessage(e.data);
        store.addMessage(e.data, open && document.hasFocus());
        break;
      }
      case "conversation.created":
        store.upsert(e.data as Conversation);
        break;
      case "conversation.read":
        store.markRead(e.data.conversation_id, e.data.last_read_message_id);
        break;
      case "friend_request.created":
      case "friend_request.removed":
      case "friendship.created":
      case "friendship.removed":
        if (friends) friends.reload();
        else refreshPending();
        break;
    }
  }

  const statusTitle: Record<string, string> = {
    connecting: "Connecting…",
    connected: "Connected",
    disconnected: "Offline. Reconnecting…",
    signed_out: "Signed out",
  };
  function onStatus(state: string) {
    realtimeState = state;
    connection.className = `conn-dot conn-${state}`;
    connection.title = statusTitle[state] ?? state;
    if (state === "signed_out") signedOut("Your session has expired; please log in again.");
  }

  const unlisteners = [onRealtimeEvent(onEvent), onRealtimeStatus(onStatus)];
  api.realtimeStart().catch((e) => toast(errorMessage(e), "error"));
  store.load().catch((e) => toast(errorMessage(e), "error"));

  teardown = () => {
    friends?.dispose();
    conversation?.dispose();
    unsubscribeStore();
    for (const u of unlisteners) u.then((fn) => fn());
  };

  go({ kind: "friends" });
}

start();
