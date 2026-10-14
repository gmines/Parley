import { api, errorMessage, type Conversation, type UserRef } from "../api";
import { avatar, h } from "../dom";

/** Most people in a group, including you (the server enforces it too). */
export const MAX_GROUP = 10;

interface PickOptions {
  title: string;
  hint: string;
  submit: string;
  min: number;
  max: number;
  /** Friends who can't be picked (already in the conversation). */
  exclude?: Set<string>;
  /** Offer a group name field. */
  withName?: boolean;
  /** Does the work; throw to show the error and let the user retry. */
  run(userIds: string[], name: string): Promise<Conversation>;
}

/** Friend picker dialog. Resolves to the resulting conversation, or null if cancelled. */
async function pickFriends(o: PickOptions): Promise<Conversation | null> {
  const friends = (await api.listFriends()).filter((f) => !o.exclude?.has(f.user.id));
  return new Promise((resolve) => {
    const picked = new Set<string>();
    const dialog = h("dialog", { class: "dialog dialog-wide" });
    const name = h("input", { attrs: { placeholder: "Group name (optional)", maxlength: "64", "aria-label": "Group name" } });
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const count = h("span", { class: "hint" });
    const submit = h("button", { class: "btn btn-primary", text: o.submit, attrs: { type: "submit" } });
    const done = (c: Conversation | null) => {
      dialog.close();
      dialog.remove();
      resolve(c);
    };

    const update = () => {
      count.textContent = `${picked.size} of ${o.max} selected`;
      submit.disabled = picked.size < o.min || picked.size > o.max;
    };

    const list =
      o.max < 1
        ? h("p", { class: "hint", text: `This group is full (${MAX_GROUP} people).` })
        : friends.length < o.min
          ? h("p", { class: "hint", text: o.exclude?.size ? "All your friends are already here." : `You need at least ${o.min} friends for this.` })
          : h(
              "ul",
              { class: "pick-list" },
              ...friends.map((f) => {
                const box = h("input", { attrs: { type: "checkbox" } });
                box.addEventListener("change", () => {
                  if (box.checked && picked.size >= o.max) {
                    box.checked = false;
                    return;
                  }
                  if (box.checked) picked.add(f.user.id);
                  else picked.delete(f.user.id);
                  update();
                });
                return h(
                  "li",
                  {},
                  h(
                    "label",
                    { class: "pick" },
                    box,
                    avatar(f.user, "sm"),
                    h("span", { class: "row-text" }, h("span", { class: "row-name", text: f.user.display_name }), h("span", { class: "row-sub", text: `@${f.user.username}` })),
                  ),
                );
              }),
            );

    const form = h(
      "form",
      { class: "dialog-form" },
      h("h2", { text: o.title }),
      h("p", { class: "hint", text: o.hint }),
      o.withName ? name : null,
      list,
      error,
      h("div", { class: "dialog-actions" }, count, h("button", { class: "btn btn-ghost", text: "Cancel", attrs: { type: "button" }, on: { click: () => done(null) } }), submit),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      submit.disabled = true;
      error.textContent = "";
      try {
        done(await o.run([...picked], name.value));
      } catch (err) {
        error.textContent = errorMessage(err);
        update();
      }
    });
    dialog.addEventListener("cancel", (e) => {
      e.preventDefault();
      done(null);
    });
    dialog.append(form);
    document.body.append(dialog);
    update();
    dialog.showModal();
  });
}

/** Start a group DM with 2-9 friends. */
export function newGroupDialog(): Promise<Conversation | null> {
  return pickFriends({
    title: "New group",
    hint: `Pick 2 to ${MAX_GROUP - 1} friends.`,
    submit: "Create group",
    min: 2,
    max: MAX_GROUP - 1,
    withName: true,
    run: (ids, name) => api.createGroup(ids, name),
  });
}

/** Add friends to a group. */
export function addToGroupDialog(group: Conversation): Promise<Conversation | null> {
  const room = MAX_GROUP - group.members.length;
  return pickFriends({
    title: "Add people",
    hint: room > 0 ? `They'll see the conversation's history. Room for ${room} more.` : "",
    submit: "Add",
    min: 1,
    max: room,
    exclude: new Set(group.members.map((m) => m.id)),
    run: (ids) => api.addGroupMembers(group.id, ids),
  });
}

/** From a DM: a new group with the other person plus the friends picked (the DM stays as it is). */
export function dmToGroupDialog(peer: UserRef): Promise<Conversation | null> {
  return pickFriends({
    title: "Start a group",
    hint: `A new group with ${peer.display_name} and the friends you pick. This conversation stays as it is.`,
    submit: "Create group",
    min: 1,
    max: MAX_GROUP - 2,
    exclude: new Set([peer.id]),
    withName: true,
    run: (ids, name) => api.createGroup([peer.id, ...ids], name),
  });
}
