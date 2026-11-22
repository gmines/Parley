import { api, type AppInfo, type ServerMeta } from "../api";
import { h, toast } from "../dom";

const OS_NAMES: Record<string, string> = { windows: "Windows", macos: "macOS", linux: "Linux" };

/** "0.2.0 (3a1e180df586)" */
export function versionLabel(info: AppInfo): string {
  return `${info.version} (${info.commit})`;
}

/**
 * About page: identifies the exact client and server builds, so testers can
 * say what they were running. "Copy diagnostics" puts it all on the clipboard.
 */
export function aboutView(server: string | null, username: string, realtimeState: () => string): HTMLElement {
  const body = h("section", { class: "content settings" }, h("p", { class: "hint", text: "Loading…" }));
  const el = h("div", { class: "main" }, h("header", { class: "main-header" }, h("h1", { text: "About" })), body);

  (async () => {
    const info = await api.appInfo();
    let meta: ServerMeta | null = null;
    let metaError = "";
    if (server) {
      try {
        meta = await api.serverMeta(server);
      } catch (e) {
        metaError = String((e as { message?: string }).message ?? e);
      }
    }

    const rows: [string, string][] = [
      ["Parley version", info.version],
      ["Build", info.commit],
      ["Operating system", `${OS_NAMES[info.os] ?? info.os} (${info.arch})`],
      ["WebView", info.webview_version ?? "unknown"],
      ["Tauri", info.tauri_version],
    ];
    const serverRows: [string, string][] = [
      ["Server", server ?? "not set"],
      ["Server version", meta?.server ? `${meta.server.version}${meta.server.commit ? ` (${meta.server.commit}${meta.server.modified ? "-dirty" : ""})` : ""}` : metaError || "unknown"],
      ["Realtime", `${meta?.realtime_url || "(default)"}: ${realtimeState()}`],
      ["Signed in as", `@${username}`],
    ];

    const kv = (pairs: [string, string][]) =>
      h("dl", { class: "kv" }, ...pairs.flatMap(([k, v]) => [h("dt", { text: k }), h("dd", { class: "selectable", text: v })]));

    const diagnostics = [...rows, ...serverRows].map(([k, v]) => `${k}: ${v}`).join("\n");
    const copy = h("button", {
      class: "btn btn-primary",
      text: "Copy diagnostics",
      on: {
        click: async () => {
          if (await copyText(diagnostics)) toast("Copied. Paste it into your bug report.");
          else toast("Couldn't copy; select the text above instead.", "error");
        },
      },
    });

    const logs = h("button", {
      class: "btn btn-ghost",
      text: "Open logs folder",
      on: { click: () => api.openLogs().catch((e) => toast(String((e as { message?: string }).message ?? e), "error")) },
    });

    body.replaceChildren(
      h(
        "section",
        { class: "settings-section about-hero" },
        h("img", { class: "about-logo", attrs: { src: "/logo.svg", alt: "" } }),
        h("div", {}, h("h2", { text: "Parley" }), h("p", { class: "hint", text: "Text and voice chat." }), h("p", { class: "about-version", text: `Version ${versionLabel(info)}` })),
      ),
      h("section", { class: "settings-section" }, h("h2", { text: "This app" }), kv(rows)),
      h("section", { class: "settings-section" }, h("h2", { text: "Connection" }), kv(serverRows)),
      h(
        "section",
        { class: "settings-section" },
        h("p", { class: "hint", text: "Reporting a problem? Include these details, and attach the log file (parley.log) if it's about calls." }),
        h("div", { class: "form-actions form-actions-start" }, copy, logs),
      ),
    );
  })();

  return el;
}

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Some WebViews deny the async clipboard API; fall back to a selection copy.
    const ta = h("textarea", { attrs: { readonly: "", style: "position:fixed;opacity:0" } });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    return ok;
  }
}
