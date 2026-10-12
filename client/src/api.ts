// Typed wrappers around the Rust commands in src-tauri/src/commands.rs.
// The Rust core holds the tokens and talks to the server; this side only sees data.
import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";

export interface User {
  id: string;
  username: string;
  display_name: string;
  created_at: string;
  email: string;
  email_verified: boolean;
  phone: string;
  phone_verified: boolean;
}

/** Optional recovery flows the server has enabled. */
export interface Features {
  email_recovery: boolean;
  sms_recovery: boolean;
}

export type ContactKind = "email" | "phone";
export type ResetChannel = "email" | "sms";

export interface UserRef {
  id: string;
  username: string;
  display_name: string;
}

export interface Friend {
  user: UserRef;
  since: string;
}

export interface FriendRequest {
  id: string;
  from: UserRef;
  to: UserRef;
  created_at: string;
}

export interface Blocked {
  user: UserRef;
  blocked_at: string;
}

export interface Requests {
  incoming: FriendRequest[];
  outgoing: FriendRequest[];
}

export interface SendResult {
  request?: FriendRequest;
  friendship?: Friend;
}

export interface Message {
  id: string;
  conversation_id: string;
  author: UserRef | null; // null if the account was deleted
  content: string;
  created_at: string;
  nonce?: string;
}

export interface Conversation {
  id: string;
  kind: "dm" | "group";
  name?: string;
  members: UserRef[];
  created_at: string;
  last_message: Message | null;
  last_read_message_id: string | null;
  unread_count: number; // capped at 100
}

/** An event pushed by the realtime service (see api/events.md). */
export interface RealtimeEvent {
  type: string;
  data: any;
}

export type RealtimeState = "connecting" | "connected" | "disconnected" | "signed_out";

export function onRealtimeEvent(fn: (e: RealtimeEvent) => void): Promise<UnlistenFn> {
  return listen<RealtimeEvent>("realtime-event", (e) => fn(e.payload));
}

export function onRealtimeStatus(fn: (s: RealtimeState) => void): Promise<UnlistenFn> {
  return listen<{ state: RealtimeState }>("realtime-status", (e) => fn(e.payload.state));
}

/** This client build (see the About page). */
export interface AppInfo {
  version: string;
  commit: string;
  os: string;
  arch: string;
  tauri_version: string;
  webview_version: string | null;
}

/** GET /v1/meta */
export interface ServerMeta {
  server?: { version: string; commit?: string; modified?: boolean; go_version: string };
  realtime_url: string;
  email_recovery: boolean;
  sms_recovery: boolean;
}

export interface AppError {
  code: string;
  message: string;
}

export function errorMessage(e: unknown): string {
  if (e && typeof e === "object" && "message" in e) return String((e as AppError).message);
  return String(e);
}

export function errorCode(e: unknown): string | undefined {
  if (e && typeof e === "object" && "code" in e) return String((e as AppError).code);
  return undefined;
}

// Tauri maps camelCase argument names here to the snake_case Rust parameters.
export const api = {
  savedServer: () => invoke<string | null>("saved_server"),
  restoreSession: () => invoke<User | null>("restore_session"),
  register: (server: string, username: string, password: string, displayName: string, email?: string, phone?: string) =>
    invoke<User>("register", { server, username, password, displayName, email: email || null, phone: phone || null }),
  login: (server: string, username: string, password: string) =>
    invoke<User>("login", { server, username, password }),
  logout: () => invoke<void>("logout"),

  serverFeatures: (server: string) => invoke<Features>("server_features", { server }),
  requestPasswordReset: (server: string, username: string, channel: ResetChannel) =>
    invoke<void>("request_password_reset", { server, username, channel }),
  confirmPasswordReset: (server: string, username: string, code: string, newPassword: string) =>
    invoke<void>("confirm_password_reset", { server, username, code, newPassword }),
  fetchMe: () => invoke<User>("fetch_me"),
  changePassword: (currentPassword: string, newPassword: string) =>
    invoke<User>("change_password", { currentPassword, newPassword }),
  setContact: (kind: ContactKind, value: string, password: string) =>
    invoke<void>("set_contact", { kind, value, password }),
  verifyContact: (kind: ContactKind, code: string) => invoke<User>("verify_contact", { kind, code }),
  removeContact: (kind: ContactKind, password: string) => invoke<User>("remove_contact", { kind, password }),

  realtimeStart: () => invoke<void>("realtime_start"),
  appInfo: () => invoke<AppInfo>("app_info"),
  serverMeta: (server: string) => invoke<ServerMeta>("server_meta", { server }),
  listConversations: () => invoke<Conversation[]>("list_conversations"),
  getConversation: (conversationId: string) => invoke<Conversation>("get_conversation", { conversationId }),
  openDm: (userId: string) => invoke<Conversation>("open_dm", { userId }),
  createGroup: (userIds: string[], name: string) => invoke<Conversation>("create_group", { userIds, name }),
  listMessages: (conversationId: string, opts: { before?: string; after?: string } = {}) =>
    invoke<Message[]>("list_messages", { conversationId, before: opts.before ?? null, after: opts.after ?? null }),
  sendMessage: (conversationId: string, content: string, nonce: string) =>
    invoke<Message>("send_message", { conversationId, content, nonce }),
  markRead: (conversationId: string, messageId: string) => invoke<void>("mark_read", { conversationId, messageId }),

  listFriends: () => invoke<Friend[]>("list_friends"),
  removeFriend: (userId: string) => invoke<void>("remove_friend", { userId }),
  listRequests: () => invoke<Requests>("list_requests"),
  sendFriendRequest: (username: string) => invoke<SendResult>("send_friend_request", { username }),
  acceptFriendRequest: (requestId: string) => invoke<Friend>("accept_friend_request", { requestId }),
  deleteFriendRequest: (requestId: string) => invoke<void>("delete_friend_request", { requestId }),
  listBlocked: () => invoke<Blocked[]>("list_blocked"),
  blockUser: (userId: string) => invoke<void>("block_user", { userId }),
  unblockUser: (userId: string) => invoke<void>("unblock_user", { userId }),
};
