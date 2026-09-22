const root = document.getElementById("root");
const dialog = document.getElementById("dialog");
const toast = document.getElementById("toast");
const drafts = new Map();
const pending = new Set();
let state = null;
let setupTab = "join";
let toastTimer = 0;
let refreshing = false;

function readAccessKey() {
  const fromLink = location.hash.match(/key=([0-9a-f]+)/);
  try {
    if (fromLink) {
      localStorage.setItem("peerly-key", fromLink[1]);
      history.replaceState(null, "", location.pathname);
    }
    return localStorage.getItem("peerly-key") || "";
  } catch (error) {
    return fromLink ? fromLink[1] : "";
  }
}

const accessKey = readAccessKey();
window.addEventListener("hashchange", () => { if (location.hash.includes("key=")) location.reload(); });

function h(tag, attrs, ...children) {
  const element = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (key.startsWith("on")) element.addEventListener(key.slice(2), value);
    else if (key === "class") element.className = value;
    else if (value === true) element.setAttribute(key, "");
    else if (value !== false && value != null) element.setAttribute(key, value);
  }
  for (const child of children.flat(Infinity)) {
    if (child == null || child === false) continue;
    element.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return element;
}

function appendAll(parent, ...children) {
  parent.append(...children.flat(Infinity).filter((child) => child != null && child !== false));
  return parent;
}

function placeToast() {
  const host = dialog.open ? dialog : document.body;
  if (toast.parentElement !== host) host.append(toast);
}

function notify(message, isError) {
  toast.replaceChildren(message, isError ? h("button", { class: "link", onclick: () => toast.classList.remove("show") }, "Dismiss") : "");
  toast.className = "show" + (isError ? " error" : "");
  placeToast();
  clearTimeout(toastTimer);
  if (!isError) toastTimer = setTimeout(() => toast.classList.remove("show"), 4000);
}

async function api(method, path, body) {
  let response;
  try {
    response = await fetch("/api" + path, {
      method,
      headers: { "Content-Type": "application/json", "X-Peerly": "1", "X-Peerly-Key": accessKey },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (error) {
    throw new Error("The peerly app on this PC is not answering. Is it still running?");
  }
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(payload.error || response.statusText);
  return payload;
}

async function act(key, work, success) {
  if (pending.has(key)) return false;
  pending.add(key);
  render();
  let succeeded = true;
  try {
    await work();
    if (success) notify(success);
  } catch (error) {
    succeeded = false;
    notify(error.message, true);
  }
  pending.delete(key);
  await refresh(true);
  return succeeded;
}

function formatTime(millis) {
  return new Date(millis).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function formatSize(bytes) {
  if (bytes < 1024 * 1024) return Math.max(1, Math.round(bytes / 1024)) + " KB";
  if (bytes < 1024 * 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + " MB";
  return (bytes / 1024 / 1024 / 1024).toFixed(2) + " GB";
}

function field(label, name, value, placeholder, options) {
  const settings = options || {};
  return h("label", {}, label,
    h("input", { name, value: value || "", placeholder: placeholder || "", type: settings.type || "text", autocomplete: "off",
      spellcheck: "false", required: settings.required === true, maxlength: settings.maxlength || 1024, "data-draft": settings.draft }),
    settings.hint ? h("span", { class: "hint" }, settings.hint) : null);
}

function formValues(form) {
  return Object.fromEntries(new FormData(form).entries());
}

function settingsPayload(form) {
  const values = formValues(form);
  return { ...values, checkpoint_minutes: Number(values.checkpoint_minutes) || 0 };
}

let liveDialog = null;
let wasPending = null;

function closeDialog() {
  liveDialog = null;
  if (dialog.open) dialog.close();
}

dialog.addEventListener("close", () => { liveDialog = null; placeToast(); });

function setDialogContent(content) {
  document.body.append(toast);
  dialog.replaceChildren(...content.flat(Infinity).filter((child) => child != null && child !== false));
  placeToast();
}

function showDialog(...content) {
  liveDialog = null;
  setDialogContent(content);
  if (!dialog.open) dialog.showModal();
}

function showLiveDialog(build) {
  showDialog(build());
  liveDialog = build;
}

function refreshDialog() {
  if (wasPending !== null && wasPending !== state.pending) closeDialog();
  wasPending = state.pending;
  if (dialog.open && liveDialog) setDialogContent([liveDialog()]);
}

function ask(title, lines, okLabel, danger, alternativeLabel) {
  return new Promise((resolve) => {
    const finish = (answer) => { dialog.removeEventListener("close", onClose); closeDialog(); resolve(answer); };
    const onClose = () => resolve(false);
    dialog.addEventListener("close", onClose, { once: true });
    showDialog(
      h("h2", {}, title),
      h("div", { class: "stack tight" }, lines.map((line) => line instanceof Node ? line : h("p", {}, line))),
      h("div", { class: "row end" },
        h("button", { onclick: () => finish(false) }, "Cancel"),
        alternativeLabel ? h("button", { onclick: () => finish("alternative") }, alternativeLabel) : null,
        h("button", { class: danger ? "danger solid" : "primary", onclick: () => finish(true) }, okLabel)),
    );
  });
}

function renderSetup() {
  const joining = setupTab === "join";
  const busy = pending.has("setup");
  const form = h("form", { class: "stack", onsubmit: (event) => {
    event.preventDefault();
    const values = formValues(form);
    act("setup", async () => {
      await api("POST", joining ? "/join" : "/create-group", values);
      drafts.clear();
    }, joining ? "Request sent, waiting for the group owner" : "Group created. Invite friends from Group and invites");
  } },
    field("Server address", "server_url", state.server_url, "saves.example.com", { required: true, draft: "setup-server", hint: "Ask the friend who runs the group's server." }),
    joining
      ? field("Invite code", "invite_code", "", "8 characters, from the group owner", { required: true, maxlength: 32, draft: "setup-code", hint: "Each code works once and for one day. After you join, the owner approves your PC." })
      : field("Group name", "name", "", "Friday crew", { required: true, maxlength: 64, draft: "setup-group" }),
    joining ? null : field("Server admin key", "admin_key", "", "printed when the server was installed", { type: "password", draft: "setup-key", hint: "Only needed to create a group, never to join one." }),
    field("Your name", "display_name", "", "how friends see you", { required: true, maxlength: 64, draft: "setup-name" }),
    h("div", { class: "row" }, h("button", { class: "primary", type: "submit", disabled: busy }, busy ? "Connecting" : joining ? "Join group" : "Create group")),
  );
  return h("div", { class: "stack" },
    h("h1", {}, "peerly"),
    h("p", { class: "muted" }, "Share one co-op world with friends. Whoever is free hosts it, and the save follows."),
    state.notice ? h("p", { class: "banner" }, state.notice) : null,
    h("div", { class: "card" },
      h("div", { class: "tabs" },
        h("button", { class: joining ? "active" : "", onclick: () => { setupTab = "join"; render(); } }, "Join a group"),
        h("button", { class: joining ? "" : "active", onclick: () => { setupTab = "create"; render(); } }, "Create a group"),
      ),
      form,
    ),
  );
}

function stopButton(hosting) {
  if (hosting.phase === "saving") {
    return h("button", { class: "danger", onclick: async () => {
      const sure = await ask("Cancel the upload?", [
        "Your progress stays on this PC and is uploaded the next time you host or sync this world.",
        "Until then your friends do not have it. If one of them hosts first, your progress ends up as a separate branch the group has to pick by hand.",
      ], "Cancel upload", true);
      if (sure) act("stop", () => api("POST", "/host/stop"));
    } }, "Cancel upload");
  }
  if (hosting.phase === "preparing") {
    return h("button", { class: "danger", onclick: () => act("stop", () => api("POST", "/host/stop")) }, "Cancel");
  }
  return h("button", { class: "danger", onclick: async () => {
    if (hosting.game_running) {
      const sure = await ask("The game is still running", [
        "Close the game first. When it closes, the save is uploaded and the world is freed by itself.",
        "If you stop now, the upload can catch the world in the middle of a save, and a friend may start hosting while you are still playing.",
      ], "Stop anyway", true);
      if (!sure) return;
    }
    act("stop", () => api("POST", "/host/stop"), "Saving and freeing the world");
  } }, "Stop hosting");
}

async function startHosting(view, syncOnly) {
  const world = view.status.world;
  if (!view.local.confirmed) {
    openSettings(view, syncOnly ? "sync" : "host");
    return;
  }
  let check;
  try {
    check = await api("GET", `/worlds/${world.id}/precheck`);
  } catch (error) {
    notify(error.message, true);
    return;
  }
  const survey = check.survey;
  if (survey.problem) {
    notify(survey.problem, true);
    return;
  }
  const found = `${survey.matched_count} file${survey.matched_count === 1 ? "" : "s"} (${formatSize(survey.matched_bytes)})`;
  let proceed = true;
  if (!check.group_has_save && survey.matched_count > 0) {
    proceed = await ask("Start the group's world from this PC?", [
      `The group has no save yet. Your world on this PC, ${found}, becomes the starting point for everyone.`,
      h("p", { class: "small muted" }, "Folder: ", h("code", {}, survey.folder)),
    ], "Upload and continue");
  } else if (!check.group_has_save) {
    proceed = await ask("No world files found yet", [
      `Nothing in the save folder matches the file filter "${check.include || "everything"}".`,
      "That is fine for a brand new world: create it in the game with exactly that name, and it is uploaded when you close the game. If the world already exists, fix the folder or the filter in Settings first.",
      h("p", { class: "small muted" }, "Folder: ", h("code", {}, survey.folder)),
    ], "Continue");
  } else if (check.never_synced && survey.matched_count > 0) {
    proceed = await ask("Replace the world files on this PC?", [
      `This PC has ${found} for this world that did not come from the group. Nothing is lost: they are copied to a backup folder and also uploaded as a separate branch the group can pick from History. Then they are replaced with the group's latest save.`,
      "If your copy is the newest one, continue, then open History and make your branch current.",
      h("p", { class: "small muted" }, "Backup folder: ", h("code", {}, view.backup_dir)),
    ], "Back up and continue");
  }
  let localChanges = "branch";
  if (proceed && check.local_progress && check.group_moved_on) {
    proceed = await ask("This PC has progress the group does not have", [
      `The world files on this PC changed since its last sync, and meanwhile ${check.last_saved_by_name} saved a newer world for the group.`,
      "Your version is uploaded as a separate branch, then this PC gets the group's current world. In History the group can decide to make your version current instead.",
    ], "Upload as a branch and continue");
  } else if (proceed && check.local_progress) {
    const answer = await ask("This PC has progress that was never uploaded", [
      "The world files on this PC changed since its last sync, for example because you played without peerly, or the app closed before it could upload.",
      "Nobody else has hosted since, so this can become the group's latest save. If you are not sure these files are the right world, keep them as a separate branch instead: nothing is lost either way.",
    ], "Make it the group's latest save", false, "Keep as a separate branch");
    proceed = answer !== false;
    if (answer === true) localChanges = "latest";
  }
  if (!proceed) return;
  act("host-" + world.id, () => api("POST", `/worlds/${world.id}/${syncOnly ? "sync" : "host"}`, { local_changes: localChanges }));
}

function renderProgress(progress) {
  const known = progress.total > 0;
  return h("div", { class: "progress" },
    h("div", { class: "row spread small" },
      h("span", {}, progress.label),
      h("span", { class: "muted" }, known ? `${formatSize(progress.done)} of ${formatSize(progress.total)}` : "working")),
    known ? h("progress", { max: progress.total, value: progress.done }) : h("progress", {}),
  );
}

function worldFacts(view) {
  const world = view.status.world;
  const lease = view.status.lease;
  const hosting = state.hosting;
  const hostingThis = hosting.world_id === world.id;
  return { world, lease, head: view.status.head, hosting, hostingThis, mine: hostingThis && hosting.active,
    heldByOther: lease && lease.holder_id !== state.member.id, starting: pending.has("host-" + world.id) };
}

function worldMainSignature(view) {
  const { lease, hosting, mine, starting } = worldFacts(view);
  return JSON.stringify([view.status.world, view.status.head, lease && [lease.holder_id, lease.holder_name], view.local, view.resolved, view.can_delete,
    mine && [hosting.sync_only, hosting.phase, hosting.game_running], hosting.active && hosting.world_name, starting, state.server_error !== "", state.member.id]);
}

function worldMain(view) {
  const { world, lease, head, hosting, mine, heldByOther, starting } = worldFacts(view);

  let badge = h("span", { class: "badge free" }, "Free to host");
  if (mine && hosting.sync_only) badge = h("span", { class: "badge mine" }, "Syncing");
  else if (mine) badge = h("span", { class: "badge mine" }, hosting.phase === "saving" ? "Uploading your save" : hosting.phase === "preparing" ? "Getting ready" : "You are hosting");
  else if (lease) badge = h("span", { class: "badge busy" }, "Hosted by " + lease.holder_name);

  let blocked = "";
  if (heldByOther) blocked = lease.holder_name + " is hosting. Join them in the game instead.";
  else if (hosting.active) blocked = "This PC is busy with " + hosting.world_name + ".";
  else if (state.server_error) blocked = "The server cannot be reached right now.";

  const actions = h("div", { class: "row" },
    mine
      ? stopButton(hosting)
      : h("button", { class: "primary", disabled: blocked !== "" || starting, title: blocked, onclick: () => startHosting(view, false) }, starting ? "Starting" : "Host"),
    mine ? null : h("button", { disabled: blocked !== "" || starting, title: blocked || "Upload progress made on this PC and download the latest save, without starting the game", onclick: () => startHosting(view, true) }, "Sync only"),
    h("button", { disabled: mine, title: mine ? "Settings cannot change while you host this world" : "", onclick: () => openSettings(view) }, "Settings"),
    h("button", { onclick: () => openHistory(view) }, "History"),
  );

  const main = h("div", { class: "stack" },
    h("div", { class: "row spread" },
      h("div", {}, h("h2", {}, world.name), h("span", { class: "muted small" }, world.game_name)),
      badge,
    ),
    h("p", { class: "small muted" }, head
      ? `Last saved by ${head.author_name}, ${formatTime(head.created_at)} (${formatSize(head.size)})${head.note === "checkpoint" ? ", a mid-session backup" : ""}`
      : "No save uploaded yet. The first person to host uploads their local world."),
    heldByOther && world.join_info ? h("p", {}, "Join them in the game: ", h("code", {}, world.join_info)) : null,
    heldByOther && !world.join_info ? h("p", { class: "small muted" }, "Ask " + lease.holder_name + " for the join code, or wait for them to share it here.") : null,
    !view.local.confirmed ? h("p", { class: "banner small" }, "Not set up on this PC yet. Press Host or Settings to check the save folder and files.") : null,
    actions,
  );

  if (mine && !hosting.sync_only && hosting.phase === "playing") {
    const inputId = "join-info-" + world.id;
    appendAll(main, h("form", { class: "row", onsubmit: (event) => {
      event.preventDefault();
      const value = document.getElementById(inputId).value;
      drafts.delete(inputId);
      act("join-info", () => api("PUT", `/worlds/${world.id}/join-info`, { join_info: value }), "Shared with the group");
    } },
      h("input", { id: inputId, "data-draft": inputId, maxlength: 200, placeholder: "Join code or address your friends need", value: drafts.get(inputId) ?? world.join_info, style: "flex:1" }),
      h("button", { type: "submit" }, "Share"),
    ));
  }
  return main;
}

function worldLiveSignature(view) {
  const { hosting, hostingThis, mine } = worldFacts(view);
  if (!hostingThis) return "";
  const last = hosting.events[hosting.events.length - 1];
  return JSON.stringify([mine && hosting.progress, hosting.events.length, last && last.at, hosting.error]);
}

function worldLive(view) {
  const { world, hosting, hostingThis, mine } = worldFacts(view);
  const live = h("div", { class: "stack" });
  if (mine && hosting.progress) appendAll(live, renderProgress(hosting.progress));
  if (hostingThis && (hosting.events.length || hosting.error)) {
    appendAll(live, h("div", { class: "log", "data-log": world.id },
      hosting.events.map((event) => h("div", { class: event.kind }, h("time", {}, new Date(event.at).toLocaleTimeString()), event.message)),
      hosting.error ? h("div", { class: "warning strong" }, hosting.error) : null,
    ));
  }
  return live;
}

function inviteText(invite) {
  return `Join my peerly group "${state.group.name}"\nServer: ${state.server_url}\nInvite code: ${invite.code}\nThe code works once and expires ${new Date(invite.expires_at).toLocaleString()}.`;
}

function copyText(text, done) {
  const fallback = () => showDialog(h("h2", {}, "Send this to your friend"), h("pre", { class: "mono" }, text), h("div", { class: "row end" }, h("button", { onclick: closeDialog }, "Close")));
  if (!navigator.clipboard) return fallback();
  navigator.clipboard.writeText(text).then(() => notify(done), fallback);
}

async function createInvite() {
  let invite;
  try {
    invite = await api("POST", "/invite");
  } catch (error) {
    notify(error.message, true);
    return;
  }
  const text = inviteText(invite);
  showDialog(
    h("h2", {}, "Invite for one friend"),
    h("p", {}, "Send this to the friend. The code works once and expires in a day. When they join, approve their PC under Group and invites."),
    h("pre", { class: "mono" }, text),
    h("div", { class: "row end" }, h("button", { onclick: closeDialog }, "Close"), h("button", { class: "primary", onclick: () => copyText(text, "Invite copied, paste it to your friend") }, "Copy invite")),
  );
}

function memberRow(member) {
  const isPending = member.status === "pending";
  const labels = [];
  if (member.id === state.group.owner_id) labels.push("owner");
  if (member.id === state.member.id) labels.push("you");
  if (member.device_name) labels.push("PC " + member.device_name);
  return h("div", { class: "row spread item" },
    h("div", {}, h("strong", {}, member.display_name), " ", h("span", { class: "muted small" }, labels.join(", "),
      isPending ? ` asked to join ${formatTime(member.joined_at)}` : "")),
    state.is_owner && member.id !== state.member.id ? h("div", { class: "row" },
      isPending ? h("button", { class: "primary", onclick: () => act("member", () => api("POST", `/members/${member.id}/approve`), member.display_name + " can now use the group") }, "Approve") : null,
      isPending ? null : h("button", { onclick: async () => {
        const sure = await ask(`Make ${member.display_name} the owner?`, [
          "They will be the only one who can invite friends, approve PCs and remove members. You stay a member.",
          "Do this before you leave the group or lose this PC, otherwise nobody can manage the group.",
        ], "Make owner");
        if (sure) act("member", () => api("POST", `/members/${member.id}/owner`), member.display_name + " is now the owner");
      } }, "Make owner"),
      h("button", { class: "danger", onclick: async () => {
        const sure = await ask(isPending ? `Turn away ${member.display_name}?` : `Remove ${member.display_name}?`, isPending
          ? ["Their PC never gets access. They need a new invite code to try again."]
          : ["Their PC loses access to the group's worlds right away. Saves they already uploaded stay."], isPending ? "Turn away" : "Remove", true);
        if (sure) act("member", () => api("DELETE", `/members/${member.id}`), member.display_name + (isPending ? " was turned away" : " was removed"));
      } }, isPending ? "Turn away" : "Remove")) : null,
  );
}

function openGroup() {
  showLiveDialog(groupDialog);
}

function groupDialog() {
  const waiting = state.members.filter((member) => member.status === "pending");
  const approved = state.members.filter((member) => member.status !== "pending");
  return [
    h("div", { class: "row spread" }, h("h2", {}, state.group.name), h("button", { onclick: closeDialog }, "Close")),
    state.is_owner
      ? h("div", { class: "stack tight" },
          h("div", { class: "row" }, h("button", { class: "primary", onclick: createInvite }, "Invite a friend")),
          h("p", { class: "small muted" }, "Each invite code is for one person and works once. Friends who join appear below, and get access when you approve their PC."))
      : h("p", { class: "small muted" }, "Only the group owner can invite friends and approve new PCs."),
    waiting.length ? h("h3", {}, "Waiting for approval") : null,
    waiting.length ? h("div", { class: "history" }, waiting.map(memberRow)) : null,
    h("h3", {}, "Members"),
    h("div", { class: "history" }, approved.map(memberRow)),
  ];
}

function renderPending() {
  return h("div", { class: "stack" },
    h("h1", {}, state.group.name),
    h("div", { class: "card stack" },
      h("h2", {}, "Waiting for the group owner"),
      h("p", {}, `You joined as ${state.member.display_name}. The group owner has to approve this PC before it can see the group's worlds. Ask them to open Group and invites and press Approve.`),
      h("p", { class: "small muted" }, "This screen updates by itself once you are approved."),
      state.server_error ? h("p", { class: "banner" }, "Cannot reach the server: " + state.server_error) : null,
    ),
    h("p", { class: "small muted" }, `Server ${state.server_url}. `,
      h("button", { class: "link", onclick: async () => {
        const sure = await ask("Leave the group on this PC?", ["Your request to join is dropped. You need a new invite code to try again."], "Leave", true);
        if (sure) act("leave", async () => { await api("POST", "/leave"); drafts.clear(); });
      } }, "Leave group on this PC")),
  );
}

function mainParts() {
  const approvedCount = state.members.filter((member) => member.status !== "pending").length || 1;
  const parts = [{
    key: "header",
    signature: JSON.stringify([state.group, state.member, state.members, state.server_error !== ""]),
    build: () => h("div", { class: "row spread" },
      h("div", {},
        h("h1", {}, state.group.name),
        h("p", { class: "muted small" }, `You are ${state.member.display_name}. ${approvedCount} member${approvedCount === 1 ? "" : "s"}.` +
          (state.is_owner && state.members.some((member) => member.status === "pending") ? " Someone is waiting for your approval." : "")),
      ),
      h("div", { class: "row" },
        h("button", { class: state.is_owner && state.members.some((member) => member.status === "pending") ? "attention" : "", onclick: openGroup }, "Group and invites"),
        h("button", { onclick: openNewWorld, disabled: state.server_error !== "" }, "Add world"),
      ),
    ),
  }];
  if (state.notice) parts.push({ key: "notice", signature: state.notice, build: () => h("p", { class: "banner" }, state.notice) });
  if (state.server_error) {
    const message = state.access_lost
      ? "The server no longer recognizes this PC. You may have been removed, or the server was reset. Leave the group on this PC (below) and join again with an invite code."
      : "Cannot reach the server: " + state.server_error + " Showing the last known state.";
    parts.push({ key: "outage", signature: message, build: () => h("p", { class: "banner" }, message) });
  }
  for (const view of state.worlds) {
    parts.push({
      key: "world-" + view.status.world.id,
      signature: "card",
      build: () => h("div", { class: "card stack" }),
      after: (card) => reconcile(card, [
        { key: "main", signature: worldMainSignature(view), build: () => worldMain(view) },
        { key: "live", signature: worldLiveSignature(view), build: () => worldLive(view) },
      ]),
    });
  }
  if (!state.worlds.length && !state.server_error) {
    parts.push({ key: "empty", signature: "empty", build: () => h("div", { class: "card muted" }, "No worlds yet. Press Add world to share the world your group plays.") });
  }
  parts.push({
    key: "footer",
    signature: state.server_url,
    build: () => h("p", { class: "small muted" }, `Server ${state.server_url}. `,
      h("button", { class: "link", onclick: async () => {
        const lines = [
          "This PC forgets the group. The group's saves stay on the server and your local game files are not touched.",
          "To come back you need an invite code again.",
        ];
        if (state.is_owner) lines.unshift(h("p", { class: "banner" }, "You are the group owner. If you leave without making someone else the owner first (Group and invites, Make owner), nobody can invite friends or approve PCs any more. Only the person running the server can then recover the group."));
        const sure = await ask("Leave the group on this PC?", lines, "Leave", true);
        if (sure) act("leave", async () => { await api("POST", "/leave"); drafts.clear(); });
      } }, "Leave group on this PC"),
      " ", h("button", { class: "link", onclick: openServerChange }, "Change server address")),
  });
  return parts;
}

function openServerChange() {
  const form = h("form", { class: "stack", onsubmit: async (event) => {
    event.preventDefault();
    const saved = await act("server-url", () => api("POST", "/server-url", formValues(form)), "Server address updated");
    if (saved) closeDialog();
  } },
    h("h2", {}, "Change server address"),
    h("p", { class: "small muted" }, "Use this when the group's server moved to a new domain. This PC keeps its membership; the new address must be the same server with the same data."),
    field("New server address", "server_url", state.server_url, "saves.example.com", { required: true }),
    h("div", { class: "row" }, h("button", { class: "primary", type: "submit" }, "Check and save"), h("button", { type: "button", onclick: closeDialog }, "Cancel")),
  );
  showDialog(form);
}

const signatures = new WeakMap();

function reconcile(parent, parts) {
  const existing = new Map();
  for (const child of [...parent.children]) existing.set(child.dataset.key, child);
  let previous = null;
  for (const part of parts) {
    let node = existing.get(part.key);
    existing.delete(part.key);
    if (!node || signatures.get(node) !== part.signature) {
      const fresh = part.build();
      fresh.dataset.key = part.key;
      signatures.set(fresh, part.signature);
      if (node) node.replaceWith(fresh);
      node = fresh;
    }
    const expectedPosition = previous ? previous.nextSibling : parent.firstChild;
    if (expectedPosition !== node) parent.insertBefore(node, expectedPosition);
    if (part.after) part.after(node);
    previous = node;
  }
  for (const stale of existing.values()) stale.remove();
}

function render() {
  if (!state) return;
  refreshDialog();
  const focused = document.activeElement;
  const focusedDraft = focused && root.contains(focused) ? focused.dataset.draft || "" : "";
  const selection = focusedDraft && typeof focused.selectionStart === "number" ? [focused.selectionStart, focused.selectionEnd] : null;
  for (const input of root.querySelectorAll("input[data-draft]")) drafts.set(input.dataset.draft, input.value);
  root.className = "stack";
  const parts = !state.configured
    ? [{ key: "setup", signature: JSON.stringify([setupTab, pending.has("setup"), state.notice, state.server_url]), build: renderSetup }]
    : state.pending
      ? [{ key: "pending", signature: JSON.stringify([state.group, state.member, state.server_error]), build: renderPending }]
      : mainParts();
  reconcile(root, parts);
  for (const log of root.querySelectorAll("[data-log]")) log.scrollTop = log.scrollHeight;
  for (const input of root.querySelectorAll("input[data-draft]")) {
    if (drafts.has(input.dataset.draft) && input.value !== drafts.get(input.dataset.draft)) input.value = drafts.get(input.dataset.draft);
  }
  if (focusedDraft && document.activeElement !== focused) {
    const target = [...root.querySelectorAll("input[data-draft]")].find((input) => input.dataset.draft === focusedDraft);
    if (target) {
      target.focus();
      if (selection) target.setSelectionRange(selection[0], selection[1]);
    }
  }
}

function checkpointSelect(current) {
  const options = [[0, "Every 15 minutes (default)"], [30, "Every 30 minutes"], [60, "Every hour"], [-1, "Off, upload only when the game closes"]];
  return h("label", {}, "Backups while you play",
    h("select", { name: "checkpoint_minutes" }, options.map(([value, label]) => h("option", { value, selected: value === (current || 0) }, label))),
    h("span", { class: "hint" }, "Protects the group if your PC dies mid-session. Uses your upload bandwidth, so pick a longer interval if friends lag when it runs."));
}

function renderSurvey(survey, target) {
  const lines = [];
  if (survey.problem) {
    lines.push(h("p", { class: "banner" }, survey.problem));
  } else if (!survey.folder_exists) {
    lines.push(h("p", { class: "banner" }, survey.parent_exists
      ? "This folder does not exist yet. That is normal if the game never saved a world on this PC."
      : "This folder does not exist, and neither does the folder above it. Is the game installed, and is the path right?"));
  } else if (survey.matched_count === 0) {
    lines.push(h("p", { class: "banner" }, "No files match the filter. Compare it with the real file names below."));
    if (survey.others.length) lines.push(h("p", { class: "small muted" }, "In this folder:"), h("ul", { class: "files" }, survey.others.map((name) => h("li", {}, name))));
  } else {
    lines.push(h("p", { class: "ok" }, `${survey.matched_count} file${survey.matched_count === 1 ? "" : "s"}, ${formatSize(survey.matched_bytes)}, belong to this world and will be shared:`));
    lines.push(h("ul", { class: "files" }, survey.matched.map((name) => h("li", {}, name)), survey.matched_count > survey.matched.length ? h("li", { class: "muted" }, `and ${survey.matched_count - survey.matched.length} more`) : null));
    if (survey.other_count) lines.push(h("p", { class: "small muted" }, `${survey.other_count} other file${survey.other_count === 1 ? " is" : "s are"} left alone, for example ${survey.others.slice(0, 3).join(", ")}.`));
  }
  lines.unshift(h("p", { class: "small muted" }, "Folder on this PC: ", h("code", {}, survey.folder || "not set")));
  target.replaceChildren(...lines);
}

function settingsForm(view, values, intent) {
  const world = view.status.world;
  const preview = h("div", { class: "preview stack tight" }, h("p", { class: "muted small" }, "Checking the folder"));
  let previewTimer = 0;
  let previewRun = 0;
  const form = h("form", { class: "stack" });
  const refreshPreview = () => {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(async () => {
      const run = ++previewRun;
      try {
        const survey = await api("POST", `/worlds/${world.id}/preview`, settingsPayload(form));
        if (run === previewRun) renderSurvey(survey, preview);
      } catch (error) {
        if (run === previewRun) preview.replaceChildren(h("p", { class: "banner" }, error.message));
      }
    }, 350);
  };
  const suggestion = view.resolved.launch_suggestion;
  appendAll(form,
    field("Save folder", "save_path", values.save_path, "C:\\Users\\you\\AppData\\...", { required: true, hint: "Where the game keeps its worlds. %LOCALAPPDATA% and similar are understood." }),
    field("Files that belong to this world", "include", values.include, "everything in the folder", { hint: "Comma separated, * is a wildcard, ! excludes. Example: MyWorld.*, !*.bak. Leave empty only if the folder holds nothing but this world." }),
    preview,
    field("Launch command or steam:// link", "launch", values.launch, "optional, leave empty to start the game yourself", {}),
    suggestion && !values.launch ? h("p", { class: "small" }, "Whoever added this world suggests: ", h("code", {}, suggestion), " ",
      h("button", { type: "button", class: "link", onclick: () => { form.elements.launch.value = suggestion; } }, "Use it"),
      h("span", { class: "muted" }, " Only accept a command you understand, it runs on your PC.")) : null,
    field("Game process name", "process", values.process, "game.exe", { hint: "How the app knows you stopped playing. Task Manager, Details tab, shows it while the game runs." }),
    checkpointSelect(values.checkpoint_minutes),
  );
  form.addEventListener("input", refreshPreview);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const saved = await act("settings", () => api("PUT", `/worlds/${world.id}/settings`, settingsPayload(form)), intent ? "" : "Settings saved");
    if (!saved) return;
    closeDialog();
    const fresh = state.worlds.find((candidate) => candidate.status.world.id === world.id);
    if (intent && fresh) startHosting(fresh, intent === "sync");
  });
  refreshPreview();
  return form;
}

function openSettings(view, intent) {
  const world = view.status.world;
  const values = view.local.confirmed ? view.local : view.resolved;
  const form = settingsForm(view, values, intent);
  appendAll(form, h("div", { class: "row spread" },
    h("div", { class: "row" },
      h("button", { class: "primary", type: "submit" }, intent ? "Looks right, continue" : "Save"),
      h("button", { type: "button", onclick: closeDialog }, "Cancel")),
    view.can_delete ? h("button", { type: "button", class: "danger", onclick: async () => {
      const sure = await ask(`Delete ${world.name} for the whole group?`, [
        "Every save of this world on the server is deleted for everyone. The game files on each PC are not touched.",
        "This cannot be undone.",
      ], "Delete world", true);
      if (sure) act("delete-world", () => api("DELETE", `/worlds/${world.id}`), world.name + " was deleted");
    } }, "Delete world") : null,
  ));
  showDialog(
    h("h2", {}, intent ? `Check ${world.name} on this PC` : `${world.name} on this PC`),
    h("p", { class: "small muted" }, intent
      ? "Before the first sync, make sure the app touches the right files. Only the files listed below are ever uploaded, backed up or replaced."
      : "These settings apply to this PC only."),
    form,
    h("p", { class: "small muted" }, "Backups of replaced files: ", h("code", {}, view.backup_dir)),
  );
}

function openNewWorld() {
  const note = h("p", { class: "small muted" });
  const presetSelect = h("select", { name: "preset" }, state.presets.map((preset, index) => h("option", { value: index }, preset.game)));
  const form = h("form", { class: "stack", onsubmit: async (event) => {
    event.preventDefault();
    const values = formValues(form);
    const worldName = values.name.trim();
    const escaped = worldName.replace(/[\\*?\[]/g, "\\$&").replaceAll(",", "?");
    let created = null;
    const succeeded = await act("new-world", async () => {
      created = await api("POST", "/worlds", {
        name: worldName,
        game_name: values.game_name,
        default_save_path: values.save_path,
        default_launch: values.launch,
        default_process: values.process,
        default_include: values.include.replaceAll("{world}", escaped),
      });
    });
    if (!succeeded) return;
    const view = state.worlds.find((candidate) => candidate.status.world.id === created.id);
    if (view) openSettings(view, "");
    else closeDialog();
  } });
  const applyPreset = () => {
    const preset = state.presets[Number(presetSelect.value)];
    note.textContent = preset.note;
    form.elements.game_name.value = preset.game === "Other game" ? "" : preset.game;
    for (const key of ["save_path", "launch", "process", "include"]) form.elements[key].value = preset[key] || "";
  };
  presetSelect.addEventListener("change", applyPreset);
  appendAll(form,
    h("h2", {}, "Add a world"),
    h("label", {}, "Game", presetSelect),
    note,
    field("World name, exactly as the save is named in the game", "name", "", "MyWorld", { required: true, maxlength: 64 }),
    field("Game name", "game_name", "", "", { maxlength: 64 }),
    field("Save folder", "save_path", "", "C:\\Users\\you\\AppData\\...", {}),
    field("Files that belong to this world", "include", "", "{world}.*", { hint: "{world} is replaced by the world name." }),
    field("Launch command or steam:// link", "launch", "", "optional", {}),
    field("Game process name", "process", "", "game.exe", {}),
    h("p", { class: "small muted" }, "These are suggestions for the group. Next you check them against the real files on this PC, and every friend does the same on theirs."),
    h("div", { class: "row" }, h("button", { class: "primary", type: "submit" }, "Add world"), h("button", { type: "button", onclick: closeDialog }, "Cancel")),
  );
  showDialog(form);
  applyPreset();
}

async function openHistory(view) {
  const world = view.status.world;
  showDialog(h("p", { class: "muted" }, "Loading history"));
  let revisions = [];
  try {
    revisions = await api("GET", `/worlds/${world.id}/revisions`);
  } catch (error) {
    closeDialog();
    notify(error.message, true);
    return;
  }
  const fresh = state.worlds.find((candidate) => candidate.status.world.id === world.id) || view;
  const headID = fresh.status.world.head_revision_id;
  const items = revisions.map((revision) => {
    const isHead = revision.id === headID;
    const isFork = revision.branch !== "main";
    const recordOnly = !!revision.pruned_at;
    return h("div", { class: "item" + (isHead ? " head" : "") + (recordOnly ? " muted" : "") },
      h("div", { class: "row spread" },
        h("div", {},
          h("strong", {}, revision.note || "save"), " ",
          isHead ? h("span", { class: "badge free" }, "current") : null,
          isFork ? h("span", { class: "badge fork", title: revision.branch }, "separate branch") : null,
          recordOnly ? h("span", { class: "badge", title: "Only the record of this session is kept, its file was removed to save space" }, "save no longer kept") : null,
          h("p", { class: "small muted" }, `${revision.author_name}, ${formatTime(revision.created_at)}` + (recordOnly ? "" : `, ${formatSize(revision.size)}`)),
        ),
        h("div", { class: "row" },
          isHead || recordOnly ? null : h("button", { onclick: async () => {
            const sure = await ask("Make this save the current world?", [
              `Everyone in the group gets ${revision.author_name}'s save from ${formatTime(revision.created_at)} the next time they host.`,
              "The present current save stays in this history, so this can be reversed.",
            ], "Make current");
            if (sure) act("history", () => api("POST", `/worlds/${world.id}/promote`, { revision_id: revision.id }), "This save is now the current world");
          } }, "Make current"),
          recordOnly ? null : h("button", { onclick: async () => {
            const sure = await ask("Put this save on this PC?", [
              "The world files on this PC are replaced with this save. If they hold progress the server does not have, they are copied to the backup folder first.",
              "This does not change anything for your friends.",
            ], "Replace local files");
            if (!sure) return;
            act("history", async () => {
              notify("Copying this save to this PC. Keep peerly open until it says it is done.");
              const result = await api("POST", `/worlds/${world.id}/pull`, { revision_id: revision.id });
              const skipped = result.skipped && result.skipped.length ? ` ${result.skipped.length} files outside your file filter were not written.` : "";
              notify((result.backup_dir ? "Done. The previous files are in " + result.backup_dir + "." : "Done, this save is now on this PC.") + skipped);
            });
          } }, "Copy to this PC"),
          isFork ? h("button", { class: "danger", onclick: async () => {
            const sure = await ask("Delete this branch save?", [`${revision.author_name}'s progress in this save is deleted from the server for good.`], "Delete", true);
            if (sure) act("history", () => api("DELETE", `/revisions/${revision.id}`), "Branch save deleted");
          } }, "Delete") : null,
        ),
      ),
    );
  });
  showDialog(
    h("div", { class: "row spread" }, h("h2", {}, world.name + " history"), h("button", { onclick: closeDialog }, "Close")),
    h("p", { class: "small muted" }, "A separate branch appears when someone played without holding the world, for example offline. Nothing is overwritten: the group decides whether to make it current. Every session stays listed for six months; only the latest save of each of the five most recent hosts keeps its file."),
    h("div", { class: "history" }, items.length ? items : h("p", { class: "muted" }, "No saves yet.")),
  );
}

async function refresh(force) {
  if (refreshing && !force) return;
  refreshing = true;
  try {
    state = await api("GET", "/state");
    render();
  } catch (error) {
    notify(error.message, true);
  }
  refreshing = false;
}

refresh();
setInterval(refresh, 4000);
