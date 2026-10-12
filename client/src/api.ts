// Typed wrappers around the Rust commands in src-tauri/src/commands.rs.
// The Rust core holds the tokens and talks to the server; this side only sees data.
import { invoke } from "@tauri-apps/api/core";

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
