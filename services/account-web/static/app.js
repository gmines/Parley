"use strict";
(function () {
  const $ = (id) => document.getElementById(id);
  const token = new URLSearchParams(location.hash.slice(1)).get("t") || "";
  // Keep the token out of the address bar and browser history.
  history.replaceState(null, "", location.pathname);

  const post = (path, body) =>
    fetch("/account/api/" + path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "omit",
      body: JSON.stringify(body),
    });

  async function message(resp) {
    try {
      return (await resp.json()).message || "Something went wrong.";
    } catch {
      return "Something went wrong.";
    }
  }

  function finish(title, text) {
    $("title").textContent = title;
    $("intro").textContent = text;
    $("form").hidden = true;
  }

  async function start() {
    let resp;
    try {
      resp = await post("inspect", { token });
    } catch {
      return finish("Can't reach the server", "Check your connection and open the link again.");
    }
    if (!resp.ok) return finish("This link can't be used", await message(resp));
    const info = await resp.json();
    const activating = info.purpose === "activate";
    const pickUsername = !!info.pick_username;
    if (pickUsername) {
      $("title").textContent = "Create your account";
      $("intro").textContent = "You were invited to Parley. Choose a username and a password.";
      $("namefields").hidden = false;
    } else {
      $("title").textContent = activating ? "Activate your account" : "Choose a new password";
      $("intro").textContent = "Choose a password for @" + info.username + ".";
    }
    $("form").hidden = false;
    $(pickUsername ? "username" : "password").focus();

    $("form").addEventListener("submit", async (e) => {
      e.preventDefault();
      $("error").textContent = "";
      if (pickUsername && !$("username").value.trim()) {
        $("error").textContent = "Choose a username.";
        return;
      }
      if ($("password").value !== $("confirm").value) {
        $("error").textContent = "The passwords don't match.";
        return;
      }
      $("submit").disabled = true;
      try {
        const payload = { token, new_password: $("password").value };
        if (pickUsername) {
          payload.username = $("username").value.trim().toLowerCase();
          payload.display_name = $("displayname").value.trim();
        }
        const done = await post("complete", payload);
        if (done.status === 204) {
          finish(
            activating ? "Your account is active" : "Password updated",
            "Return to the Parley app and log in. You'll be asked for your authenticator code, or to set one up.",
          );
          return;
        }
        $("error").textContent = await message(done);
      } catch {
        $("error").textContent = "Can't reach the server. Try again.";
      }
      $("submit").disabled = false;
    });
  }
  start();
})();
