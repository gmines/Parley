import { api, errorCode, errorMessage, type Conversation, type Message } from "../api";
import { avatar, h } from "../dom";
import { conversationTitle, dmPeer, store } from "../store";

const MAX_CHARS = 4000;
const GROUP_GAP_MS = 5 * 60 * 1000; // consecutive messages within this merge under one header

interface Item extends Message {
  status?: "pending" | "failed";
}

export interface ConversationView {
  el: HTMLElement;
  id: string;
  /** A realtime message for this conversation. */
  addMessage(m: Message): void;
  /** After a reconnect: fetch anything missed. */
  resync(): void;
  dispose(): void;
}

export function conversationView(conv: Conversation, onSignedOut: (msg: string) => void): ConversationView {
  const me = store.me()!;
  let items: Item[] = [];
  let hasOlder = true;
  let loadingOlder = false;
  let lastMarked = "";

  const title = h("h1", { text: conversationTitle(conv) });
  const subtitle = h("span", { class: "conv-sub" });
  const list = h("div", { class: "messages", attrs: { role: "log", "aria-live": "polite" } });
  const banner = h("p", { class: "composer-error", attrs: { role: "alert" } });
  const input = h("textarea", { class: "composer-input", attrs: { rows: "1", maxlength: String(MAX_CHARS) } });
  const counter = h("span", { class: "composer-count" });
  const el = h(
    "div",
    { class: "main conversation" },
    h("header", { class: "main-header" }, title, subtitle),
    list,
    h("form", { class: "composer" }, banner, h("div", { class: "composer-row" }, input, counter)),
  );

  const peer = dmPeer(conv);
  subtitle.textContent = peer ? `@${peer.username}` : `${conv.members.length} members`;
  input.placeholder = `Message ${peer ? "@" + peer.username : conversationTitle(conv)}`;

  // Rendering -----------------------------------------------------------------

  const atBottom = () => list.scrollHeight - list.scrollTop - list.clientHeight < 40;
  const timeFmt = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });
  const dayFmt = new Intl.DateTimeFormat(undefined, { weekday: "long", month: "long", day: "numeric", year: "numeric" });

  function render(keep: "bottom" | "anchor" | "none" = "none") {
    const anchorFromBottom = list.scrollHeight - list.scrollTop;
    const nodes: Node[] = [];
    if (!hasOlder) {
      nodes.push(
        h("div", { class: "conv-start" }, h("h2", { text: conversationTitle(conv) }), h("p", { text: "This is the beginning of your conversation." })),
      );
    }
    let prev: Item | undefined;
    for (const m of items) {
      const at = new Date(m.created_at);
      const newDay = !prev || new Date(prev.created_at).toDateString() !== at.toDateString();
      if (newDay) nodes.push(h("div", { class: "day-divider" }, h("span", { text: dayFmt.format(at) })));
      const grouped =
        !newDay && prev && prev.author?.id === m.author?.id && at.getTime() - new Date(prev.created_at).getTime() < GROUP_GAP_MS;
      nodes.push(messageNode(m, !grouped, at));
      prev = m;
    }
    list.replaceChildren(...nodes);
    if (keep === "bottom") list.scrollTop = list.scrollHeight;
    else if (keep === "anchor") list.scrollTop = list.scrollHeight - anchorFromBottom;
  }

  function messageNode(m: Item, withHeader: boolean, at: Date): HTMLElement {
    const author = m.author ?? { id: "deleted", username: "deleted", display_name: "Deleted user" };
    const body = h("div", { class: "msg-body", text: m.content });
    const row = h(
      "div",
      { class: `msg${withHeader ? " msg-first" : ""}${m.status ? " msg-" + m.status : ""}`, attrs: { title: at.toLocaleString() } },
      withHeader ? avatar(author) : h("span", { class: "msg-gutter", text: timeFmt.format(at) }),
      h(
        "div",
        { class: "msg-main" },
        withHeader
          ? h("div", { class: "msg-head" }, h("span", { class: "msg-author", text: author.display_name }), h("span", { class: "msg-time", text: timeFmt.format(at) }))
          : null,
        body,
        m.status === "failed"
          ? h(
              "div",
              { class: "msg-failed" },
              "Not sent. ",
              h("button", { class: "link link-sm", attrs: { type: "button" }, text: "Retry", on: { click: () => retry(m) } }),
              h("button", { class: "link link-sm", attrs: { type: "button" }, text: "Discard", on: { click: () => discard(m) } }),
            )
          : null,
      ),
    );
    return row;
  }

  // Data ------------------------------------------------------------------------

  function merge(incoming: Message[]) {
    for (const m of incoming) {
      if (items.some((x) => x.id === m.id)) continue;
      const pending = m.nonce ? items.findIndex((x) => x.status && x.nonce === m.nonce) : -1;
      if (pending >= 0) items[pending] = m;
      else items.push(m);
    }
    // Real messages in id (= time) order; unsent ones stay at the end.
    const sent = items.filter((x) => !x.status).sort((a, b) => a.id.localeCompare(b.id));
    items = [...sent, ...items.filter((x) => x.status)];
  }

  function lastSentId(): string | undefined {
    for (let i = items.length - 1; i >= 0; i--) if (!items[i].status) return items[i].id;
    return undefined;
  }

  function fail(err: unknown) {
    const code = errorCode(err);
    if (code === "session_expired" || code === "not_logged_in") onSignedOut(errorMessage(err));
    else banner.textContent = errorMessage(err);
  }

  async function loadInitial() {
    try {
      const msgs = await api.listMessages(conv.id);
      hasOlder = msgs.length === 50;
      merge(msgs);
      render("bottom");
      maybeMarkRead();
    } catch (e) {
      fail(e);
    }
  }

  async function loadOlder() {
    const first = items.find((x) => !x.status);
    if (!hasOlder || loadingOlder || !first) return;
    loadingOlder = true;
    try {
      const msgs = await api.listMessages(conv.id, { before: first.id });
      hasOlder = msgs.length === 50;
      merge(msgs);
      render("anchor");
    } catch (e) {
      fail(e);
    } finally {
      loadingOlder = false;
    }
  }

  async function resync() {
    const last = lastSentId();
    if (!last) return loadInitial();
    try {
      const stick = atBottom();
      let after = last;
      for (;;) {
        const msgs = await api.listMessages(conv.id, { after });
        merge(msgs);
        if (msgs.length < 50) break;
        after = msgs[msgs.length - 1].id;
      }
      render(stick ? "bottom" : "none");
      maybeMarkRead();
    } catch (e) {
      fail(e);
    }
  }

  // Read markers: only when the newest message is actually on screen.
  let markTimer: number | undefined;
  function maybeMarkRead() {
    window.clearTimeout(markTimer);
    markTimer = window.setTimeout(() => {
      const last = lastSentId();
      if (!last || last === lastMarked || !document.hasFocus() || !atBottom()) return;
      lastMarked = last;
      store.markRead(conv.id, last);
      api.markRead(conv.id, last).catch(() => (lastMarked = ""));
    }, 300);
  }

  // Sending -----------------------------------------------------------------------

  async function send(content: string, existing?: Item) {
    const item: Item = existing ?? {
      id: `pending-${crypto.randomUUID()}`,
      conversation_id: conv.id,
      author: me,
      content,
      created_at: new Date().toISOString(),
      nonce: crypto.randomUUID(),
    };
    item.status = "pending";
    if (!existing) items.push(item);
    banner.textContent = "";
    render("bottom");
    try {
      const m = await api.sendMessage(conv.id, content, item.nonce!);
      merge([m]); // replaces the pending copy unless the realtime event already did
      render(atBottom() ? "bottom" : "none");
    } catch (e) {
      item.status = "failed";
      render(atBottom() ? "bottom" : "none");
      fail(e);
    }
  }

  function retry(m: Item) {
    send(m.content, m);
  }

  function discard(m: Item) {
    items = items.filter((x) => x !== m);
    render();
  }

  function submit() {
    const text = input.value.trim();
    if (!text) return;
    input.value = "";
    resize();
    send(text);
  }

  function resize() {
    input.style.height = "auto";
    input.style.height = `${Math.min(input.scrollHeight, 200)}px`;
    const n = input.value.length;
    counter.textContent = n > MAX_CHARS - 500 ? String(MAX_CHARS - n) : "";
  }

  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
  input.addEventListener("input", resize);
  el.querySelector("form")!.addEventListener("submit", (e) => {
    e.preventDefault();
    submit();
  });
  list.addEventListener("scroll", () => {
    if (list.scrollTop < 60) loadOlder();
    if (atBottom()) maybeMarkRead();
  });
  const onFocus = () => maybeMarkRead();
  window.addEventListener("focus", onFocus);

  loadInitial();
  queueMicrotask(() => input.focus());

  return {
    el,
    id: conv.id,
    addMessage(m: Message) {
      const stick = atBottom();
      merge([m]);
      render(stick ? "bottom" : "none");
      if (stick) maybeMarkRead();
    },
    resync,
    dispose() {
      window.removeEventListener("focus", onFocus);
      window.clearTimeout(markTimer);
    },
  };
}
