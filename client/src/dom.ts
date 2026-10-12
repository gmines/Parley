// Minimal DOM helpers. Text is always set with textContent, never innerHTML, so
// usernames and display names can't inject markup.

type Child = Node | string | null | undefined | false;

interface Props {
  class?: string;
  text?: string;
  attrs?: Record<string, string>;
  on?: { [K in keyof HTMLElementEventMap]?: (ev: HTMLElementEventMap[K]) => void };
}

export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Props = {},
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (props.class) el.className = props.class;
  if (props.text !== undefined) el.textContent = props.text;
  for (const [k, v] of Object.entries(props.attrs ?? {})) el.setAttribute(k, v);
  for (const [ev, fn] of Object.entries(props.on ?? {})) el.addEventListener(ev, fn as EventListener);
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c);
  }
  return el;
}

export function mount(root: HTMLElement, ...nodes: Node[]) {
  root.replaceChildren(...nodes);
}

/** A circle with the user's initial, colored from a hash of their ID. */
export function avatar(user: { id: string; display_name: string }, size = "md"): HTMLElement {
  let hash = 0;
  for (const ch of user.id) hash = (hash * 31 + ch.charCodeAt(0)) | 0;
  const el = h("div", { class: `avatar avatar-${size}`, text: initial(user.display_name) });
  el.style.setProperty("--hue", String(Math.abs(hash) % 360));
  el.setAttribute("aria-hidden", "true");
  return el;
}

function initial(name: string): string {
  const first = [...name.trim()][0];
  return first ? first.toUpperCase() : "?";
}

let toastTimer: number | undefined;

export function toast(message: string, kind: "info" | "error" = "info") {
  let el = document.getElementById("toast");
  if (!el) {
    el = h("div", { attrs: { id: "toast", role: "status", "aria-live": "polite" } });
    document.body.append(el);
  }
  el.textContent = message;
  el.className = `toast toast-${kind} toast-show`;
  window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => el!.classList.remove("toast-show"), 3500);
}

/** In-app confirmation dialog (native window.confirm is unreliable in webviews). */
export function confirmDialog(title: string, body: string, confirmLabel: string, danger = true): Promise<boolean> {
  return new Promise((resolve) => {
    const dialog = h("dialog", { class: "dialog" });
    const done = (ok: boolean) => {
      dialog.close();
      dialog.remove();
      resolve(ok);
    };
    dialog.append(
      h("h2", { text: title }),
      h("p", { text: body }),
      h(
        "div",
        { class: "dialog-actions" },
        h("button", { class: "btn btn-ghost", text: "Cancel", on: { click: () => done(false) } }),
        h("button", {
          class: danger ? "btn btn-danger" : "btn btn-primary",
          text: confirmLabel,
          on: { click: () => done(true) },
        }),
      ),
    );
    dialog.addEventListener("cancel", (e) => {
      e.preventDefault();
      done(false);
    });
    document.body.append(dialog);
    dialog.showModal();
  });
}
