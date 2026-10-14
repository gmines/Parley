// Client-side state for the conversation list (sidebar), shared by the views
// and kept current by realtime events.
import { api, type Conversation, type Message, type UserRef } from "./api";

type Listener = () => void;

const conversations = new Map<string, Conversation>();
const listeners = new Set<Listener>();
let me: UserRef | null = null;

function changed() {
  for (const fn of listeners) fn();
}

export const store = {
  setMe(u: UserRef) {
    me = u;
  },

  me(): UserRef | null {
    return me;
  },

  subscribe(fn: Listener): () => void {
    listeners.add(fn);
    return () => listeners.delete(fn);
  },

  /** Conversations, most recently active first. */
  list(): Conversation[] {
    const at = (c: Conversation) => c.last_message?.created_at ?? c.created_at;
    return [...conversations.values()].sort((a, b) => at(b).localeCompare(at(a)));
  },

  get(id: string): Conversation | undefined {
    return conversations.get(id);
  },

  totalUnread(): number {
    let n = 0;
    for (const c of conversations.values()) n += c.unread_count;
    return n;
  },

  async load() {
    const list = await api.listConversations();
    conversations.clear();
    for (const c of list) conversations.set(c.id, c);
    changed();
  },

  upsert(c: Conversation) {
    const prev = conversations.get(c.id);
    // Keep per-user fields we already know; announcements carry empty ones.
    conversations.set(c.id, prev ? { ...c, unread_count: prev.unread_count, last_read_message_id: prev.last_read_message_id } : c);
    changed();
  },

  /** conversation.updated: who's in it now. */
  setMembers(conversationId: string, name: string | undefined, members: UserRef[]) {
    const c = conversations.get(conversationId);
    if (!c) {
      api.getConversation(conversationId).then((full) => store.upsert(full)).catch(() => {});
      return;
    }
    c.members = members;
    c.name = name;
    changed();
  },

  /** We left it. */
  remove(conversationId: string) {
    if (conversations.delete(conversationId)) changed();
  },

  /** A new message arrived. `seen` = the user is looking at it right now. */
  addMessage(m: Message, seen: boolean) {
    const c = conversations.get(m.conversation_id);
    if (!c) {
      // A conversation we haven't loaded yet (e.g. created while offline).
      api.getConversation(m.conversation_id).then((full) => store.upsert(full)).catch(() => {});
      return;
    }
    if (c.last_message && c.last_message.id >= m.id) return; // already have it
    c.last_message = m;
    if (m.author?.id === me?.id || seen) {
      c.unread_count = 0;
      c.last_read_message_id = m.id;
    } else {
      c.unread_count = Math.min(c.unread_count + 1, 100);
    }
    changed();
  },

  markRead(conversationId: string, messageId: string) {
    const c = conversations.get(conversationId);
    if (!c) return;
    if (c.last_read_message_id && c.last_read_message_id >= messageId) return;
    c.last_read_message_id = messageId;
    if (!c.last_message || c.last_message.id <= messageId) c.unread_count = 0;
    changed();
  },

  clear() {
    conversations.clear();
    me = null;
    changed();
  },
};

/** "Alice" for a DM, the group's name, or its members' names. */
export function conversationTitle(c: Conversation): string {
  const others = c.members.filter((m) => m.id !== me?.id);
  if (c.kind === "dm") return others[0]?.display_name ?? "Direct message";
  if (c.name) return c.name;
  return others.map((m) => m.display_name).join(", ") || "Group";
}

/** The other user in a DM. */
export function dmPeer(c: Conversation): UserRef | undefined {
  return c.kind === "dm" ? c.members.find((m) => m.id !== me?.id) : undefined;
}
