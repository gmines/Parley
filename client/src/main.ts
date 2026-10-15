import { api, errorMessage, onRealtimeEvent, onRealtimeStatus, onVoiceEvent, type Conversation, type Features, type RealtimeEvent, type User, type UserRef } from "./api";
import { avatar, h, mount, toast } from "./dom";
import { conversationTitle, dmPeer, store } from "./store";
import { callBar, disposeWidget, voice } from "./voice";
import { aboutView, versionLabel } from "./views/about";
import { accountView } from "./views/account";
import { authView } from "./views/auth";
import { conversationView, type ConversationView } from "./views/conversation";
import { friendsView, type FriendsView } from "./views/friends";
import { newCallDialog, newGroupDialog } from "./views/newgroup";
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
  voice.setMe(user.id);
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
  const newCall = h("button", {
    class: "icon-btn",
    text: "📞",
    attrs: { title: "New call", "aria-label": "New call" },
    on: {
      click: async () => {
        const c = await newCallDialog().catch((e) => (toast(errorMessage(e), "error"), null));
        if (c) {
          store.upsert(c);
          voice.join(c.id);
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

  const bar = callBar((id) => go({ kind: "conversation", id }));
  const sidebar = h(
    "aside",
    { class: "sidebar" },
    h("div", { class: "brand", text: "Parley" }),
    h("nav", { class: "side-nav" }, friendsNav, accountNav, aboutNav),
    h("div", { class: "side-section" }, h("span", { text: "Direct messages" }), h("span", { class: "side-actions" }, newCall, newGroup)),
    dmList,
    bar,
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
        const live = voice.call(c.id);
        const here = voice.inCall(c.id);
        return h(
          "div",
          { class: `dm-row${active ? " dm-item-active" : ""}` },
          h(
            "button",
            {
              class: `dm-item${c.unread_count ? " dm-item-unread" : ""}`,
              attrs: active ? { "aria-current": "page" } : {},
              on: { click: () => go({ kind: "conversation", id: c.id }) },
            },
            peer ? avatar(peer, "sm") : h("div", { class: "avatar avatar-sm avatar-group", text: "#", attrs: { "aria-hidden": "true" } }),
            h("span", { class: "row-text" }, h("span", { class: "row-name", text: conversationTitle(c) }), h("span", { class: "row-sub", text: preview })),
            live ? h("span", { class: "dm-call", text: "🔊", attrs: { title: `Call in progress (${live.participants.length})` } }) : null,
            c.unread_count ? h("span", { class: "badge", text: c.unread_count >= 100 ? "99+" : String(c.unread_count) }) : null,
          ),
          here
            ? null
            : h("button", {
                class: "dm-call-btn",
                text: "📞",
                attrs: { title: live ? "Join call" : "Call", "aria-label": `${live ? "Join call in" : "Call"} ${conversationTitle(c)}` },
                on: { click: () => voice.join(c.id) },
              }),
        );
      }),
    );
  }
  const unsubscribeStore = store.subscribe(() => {
    // Left the open group (here or on another device): nothing to show.
    if (page.kind === "conversation" && !store.get(page.id)) go({ kind: "friends" });
    else renderDmList();
  });
  const unsubscribeVoice = voice.subscribe(renderDmList);

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
      friends = friendsView(signedOut, openDm, callUser);
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
        conversation = conversationView(c, signedOut, (g) => go({ kind: "conversation", id: g.id }));
        pane.replaceChildren(conversation.el);
      }
    }
    renderDmList();
  }

  /** Calls a friend from wherever we are (their DM's call), without opening it. */
  async function callUser(u: UserRef) {
    try {
      const c = await api.openDm(u.id);
      store.upsert(c);
      await voice.join(c.id);
    } catch (e) {
      toast(errorMessage(e), "error");
    }
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
        voice.resync();
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
        // We may have been added to a group with a call already going.
        voice.resync();
        break;
      case "conversation.updated": {
        const id = e.data.conversation_id as string;
        store.setMembers(id, e.data.name || undefined, e.data.members);
        const c = store.get(id);
        if (c && conversation?.id === id) conversation.update(c);
        break;
      }
      case "conversation.removed": {
        const id = e.data.conversation_id as string;
        if (voice.inCall(id)) voice.leave().catch(() => {});
        store.remove(id);
        break;
      }
      case "conversation.read":
        store.markRead(e.data.conversation_id, e.data.last_read_message_id);
        break;
      case "call.started":
      case "call.updated":
      case "call.ended":
      case "call.declined":
        voice.handleRealtime(e);
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

  const unlisteners = [onRealtimeEvent(onEvent), onRealtimeStatus(onStatus), onVoiceEvent((e) => voice.handleVoiceEvent(e))];
  api.realtimeStart().catch((e) => toast(errorMessage(e), "error"));
  store.load().catch((e) => toast(errorMessage(e), "error"));

  teardown = () => {
    friends?.dispose();
    conversation?.dispose();
    unsubscribeStore();
    unsubscribeVoice();
    disposeWidget(bar);
    voice.reset();
    for (const u of unlisteners) u.then((fn) => fn());
  };

  go({ kind: "friends" });
}

start();
