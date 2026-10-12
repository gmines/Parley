import { api, errorMessage, type Conversation } from "../api";
import { avatar, h } from "../dom";

const MAX_OTHERS = 9;

/** Dialog for starting a group DM with 2-9 friends. Resolves to the new group, or null if cancelled. */
export async function newGroupDialog(): Promise<Conversation | null> {
  const friends = await api.listFriends();
  return new Promise((resolve) => {
    const picked = new Set<string>();
    const dialog = h("dialog", { class: "dialog dialog-wide" });
    const name = h("input", { attrs: { placeholder: "Group name (optional)", maxlength: "64", "aria-label": "Group name" } });
    const error = h("p", { class: "form-error", attrs: { role: "alert" } });
    const count = h("span", { class: "hint" });
    const create = h("button", { class: "btn btn-primary", text: "Create group", attrs: { type: "submit" } });
    const done = (c: Conversation | null) => {
      dialog.close();
      dialog.remove();
      resolve(c);
    };

    const update = () => {
      count.textContent = `${picked.size} of ${MAX_OTHERS} selected`;
      create.disabled = picked.size < 2 || picked.size > MAX_OTHERS;
    };

    const list =
      friends.length < 2
        ? h("p", { class: "hint", text: "You need at least two friends to start a group." })
        : h(
            "ul",
            { class: "pick-list" },
            ...friends.map((f) => {
              const box = h("input", { attrs: { type: "checkbox" } });
              box.addEventListener("change", () => {
                if (box.checked && picked.size >= MAX_OTHERS) {
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
      h("h2", { text: "New group" }),
      h("p", { class: "hint", text: "Pick 2 to 9 friends." }),
      name,
      list,
      error,
      h("div", { class: "dialog-actions" }, count, h("button", { class: "btn btn-ghost", text: "Cancel", attrs: { type: "button" }, on: { click: () => done(null) } }), create),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      create.disabled = true;
      error.textContent = "";
      try {
        done(await api.createGroup([...picked], name.value));
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
