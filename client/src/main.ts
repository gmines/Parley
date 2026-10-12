import { api, errorMessage, type Features, type User } from "./api";
import { avatar, h, mount } from "./dom";
import { accountView } from "./views/account";
import { authView } from "./views/auth";
import { friendsView, type FriendsView } from "./views/friends";
import { resetView } from "./views/reset";

const root = document.getElementById("app")!;
let friends: FriendsView | null = null;

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

function stopFriends() {
  friends?.dispose();
  friends = null;
}

function showAuth(server: string | null, opts: { username?: string; notice?: string } = {}) {
  stopFriends();
  mount(root, authView({ server, ...opts, onAuthed: showApp, onForgot: showReset }));
}

function showReset(server: string, username: string, features: Features) {
  stopFriends();
  mount(root, resetView(server, username, features, (opts) => showAuth(server, opts)));
}

async function signedOut(message: string) {
  showAuth(await api.savedServer(), { notice: message });
}

function showApp(initialUser: User) {
  let user = initialUser;
  let page: "friends" | "account" = "friends";

  const badge = h("span", { class: "badge", attrs: { hidden: "" } });
  const friendsNav = h("button", { class: "side-item", on: { click: () => go("friends") } }, "Friends", badge);
  const accountNav = h("button", { class: "side-item", text: "Account", on: { click: () => go("account") } });
  const meName = h("span", { class: "me-name" });
  const meSub = h("span", { class: "me-sub" });
  const meAvatar = h("span");
  const renderMe = () => {
    meName.textContent = user.display_name;
    meSub.textContent = `@${user.username}`;
    meAvatar.replaceChildren(avatar(user, "sm"));
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
    h("nav", { class: "side-nav" }, friendsNav, accountNav),
    h("div", { class: "spacer" }),
    h("div", { class: "me" }, meAvatar, h("div", { class: "me-text" }, meName, meSub), logout),
  );
  const pane = h("div", { class: "pane" });
  mount(root, h("div", { class: "shell" }, sidebar, pane));
  renderMe();

  async function go(next: typeof page) {
    page = next;
    for (const [btn, p] of [[friendsNav, "friends"], [accountNav, "account"]] as const) {
      btn.classList.toggle("side-item-active", page === p);
      if (page === p) btn.setAttribute("aria-current", "page");
      else btn.removeAttribute("aria-current");
    }
    stopFriends();
    if (page === "friends") {
      friends = friendsView(signedOut);
      friends.onPendingChange = (n) => {
        badge.hidden = n === 0;
        badge.textContent = String(n);
      };
      pane.replaceChildren(friends.el);
    } else {
      const server = await api.savedServer();
      pane.replaceChildren(
        accountView(user, server, (u) => {
          user = u;
          renderMe();
        }, signedOut),
      );
    }
  }
  go("friends");
}

start();
