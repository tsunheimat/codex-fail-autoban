package autoban

import "html"

// statusPage renders the browser-navigable resource page. It is unauthenticated
// itself; the API calls it makes require the CPA management key entered by the user.
// currentMode is the plugin's active mode, used to preselect the mode switch.
func statusPage(currentMode string) string {
	version := html.EscapeString(PluginVersion)
	mode := currentMode
	if mode != ModeDelete {
		mode = ModeDisable
	}
	disableSel, deleteSel := "", ""
	if mode == ModeDelete {
		deleteSel = " selected"
	} else {
		disableSel = " selected"
	}
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>codex-fail-autoban</title>
  <style>
    :root { color-scheme: light dark; font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { max-width: 1040px; margin: 32px auto; padding: 0 16px; line-height: 1.5; }
    h1 { margin-bottom: 4px; }
    .muted { color: #667085; }
    .card { border: 1px solid #d0d7de; border-radius: 12px; padding: 16px; margin: 16px 0; }
    label { display: block; font-weight: 600; margin-bottom: 6px; }
    input, select { width: min(640px, 100%); padding: 8px 10px; border: 1px solid #d0d7de; border-radius: 8px; }
    button { cursor: pointer; padding: 8px 12px; border: 1px solid #d0d7de; border-radius: 8px; margin: 4px 4px 4px 0; }
    button.primary { background: #0969da; border-color: #0969da; color: white; }
    button.danger { background: #cf222e; border-color: #cf222e; color: white; }
    table { width: 100%; border-collapse: collapse; margin-top: 12px; }
    th, td { border-bottom: 1px solid #d0d7de; padding: 8px; text-align: left; vertical-align: top; font-size: 14px; }
    code { background: rgba(127,127,127,.15); padding: 2px 4px; border-radius: 4px; word-break: break-all; }
    pre { overflow: auto; background: rgba(127,127,127,.12); padding: 12px; border-radius: 8px; }
    .tag { display: inline-block; padding: 1px 6px; border-radius: 6px; font-size: 12px; }
    .tag.ok { background: rgba(45,164,78,.18); }
    .tag.err { background: rgba(207,34,46,.18); }
  </style>
</head>
<body>
  <h1>codex-fail-autoban</h1>
  <p class="muted">v` + version + ` · Auto-disable or delete accounts whose auth token was invalidated. 认证令牌失效后自动禁用或删除账号。</p>

  <div class="card">
    <p>This resource page has no management auth. To view/clear state, enter your CPA management key; requests send <code>Authorization: Bearer &lt;key&gt;</code>. 本页不含管理鉴权，操作需填入 CPA 管理密钥。</p>
    <label for="key">CPA management key / 管理密钥</label>
    <input id="key" type="password" autocomplete="current-password" placeholder="Management key">
    <div>
      <button class="primary" onclick="refresh()">Refresh / 刷新</button>
      <button class="danger" onclick="forgetAll()">Forget all / 全部清除</button>
    </div>
    <p id="message" class="muted"></p>
  </div>

  <div class="card">
    <h2>Action mode / 动作模式</h2>
    <p class="muted">What to do to an account whose auth token is invalidated. Saving requires the management key above and applies live (no restart). 保存需填入上方管理密钥，即时生效。</p>
    <label for="mode">Mode / 模式</label>
    <select id="mode">
      <option value="disable"` + disableSel + `>disable — write disabled:true, keep the file / 禁用（保留文件）</option>
      <option value="delete"` + deleteSel + `>delete — remove the credential file / 删除凭证文件</option>
    </select>
    <div><button class="primary" onclick="saveMode()">Save mode / 保存模式</button></div>
    <p class="muted">Current: <code id="currentMode">` + mode + `</code></p>
  </div>

  <div class="card">
    <h2>Handled accounts / 已处理账号</h2>
    <p class="muted">"Forget" only clears this plugin's in-memory ban so a re-authenticated account can be scheduled again. It does not restore a deleted file. 「清除」仅解除插件内存中的封禁，不会恢复已删除的凭证文件。</p>
    <div id="list">Not loaded yet.</div>
  </div>

  <div class="card">
    <h2>API</h2>
    <pre>GET   /v0/management/plugins/codex-fail-autoban/accounts
POST  /v0/management/plugins/codex-fail-autoban/forget   {"auth_id":"..."}
POST  /v0/management/plugins/codex-fail-autoban/forget   {"all":true}
PATCH /v0/management/plugins/codex-fail-autoban/config    {"mode":"delete"}   (CPA core)</pre>
  </div>

  <script>
    const apiBase = "/v0/management/plugins/codex-fail-autoban";
    const keyInput = document.getElementById("key");
    keyInput.value = localStorage.getItem("codexFailAutobanManagementKey") || "";
    keyInput.addEventListener("change", function () {
      localStorage.setItem("codexFailAutobanManagementKey", keyInput.value);
    });

    function headers() {
      const h = {"Content-Type": "application/json"};
      if (keyInput.value) h.Authorization = "Bearer " + keyInput.value;
      return h;
    }
    function setMessage(text, isError) {
      const el = document.getElementById("message");
      el.textContent = text || "";
      el.style.color = isError ? "#cf222e" : "";
    }
    async function call(path, options) {
      const resp = await fetch(apiBase + path, Object.assign({headers: headers()}, options || {}));
      const text = await resp.text();
      let data;
      try { data = JSON.parse(text); } catch (_) { data = {raw: text}; }
      if (!resp.ok) {
        throw new Error((data && (data.message || data.error)) || ("HTTP " + resp.status));
      }
      return data;
    }
    function escapeHtml(value) {
      return String(value == null ? "" : value).replace(/[&<>"']/g, function (c) {
        return {"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c];
      });
    }
    function render(data) {
      const list = document.getElementById("list");
      if (!data || !data.accounts || data.accounts.length === 0) {
        list.innerHTML = "<p>No accounts handled. 暂无被处理的账号。</p>";
        return;
      }
      let rows = "";
      for (const a of data.accounts) {
        const state = a.error
          ? "<span class=\"tag err\">error</span>"
          : (a.dry_run ? "<span class=\"tag\">dry-run</span>" : "<span class=\"tag ok\">" + escapeHtml(a.mode) + "</span>");
        rows += "<tr>"
          + "<td><code>" + escapeHtml(a.auth_id) + "</code></td>"
          + "<td>" + escapeHtml(a.provider) + "</td>"
          + "<td>" + escapeHtml(a.file_name) + "</td>"
          + "<td>" + state + (a.error ? "<br><span class=\"muted\">" + escapeHtml(a.error) + "</span>" : "") + "</td>"
          + "<td>" + escapeHtml(a.reason) + "</td>"
          + "<td>" + (a.excluded ? "yes" : "no") + "</td>"
          + "<td><button class=\"forget-btn\" data-auth=\"" + escapeHtml(a.auth_id) + "\">Forget</button></td>"
          + "</tr>";
      }
      list.innerHTML = "<p class=\"muted\">Mode: <code>" + escapeHtml(data.mode) + "</code>"
        + (data.dry_run ? " (dry-run)" : "") + " · " + data.count + " account(s)</p>"
        + "<table><thead><tr><th>Auth ID</th><th>Provider</th><th>File</th><th>State</th><th>Reason</th><th>In ban list</th><th></th></tr></thead><tbody>"
        + rows + "</tbody></table>";
    }
    function syncMode(mode) {
      if (!mode) return;
      const sel = document.getElementById("mode");
      if (sel) sel.value = mode;
      const cur = document.getElementById("currentMode");
      if (cur) cur.textContent = mode;
    }
    async function refresh() {
      try {
        setMessage("Loading...");
        const data = await call("/accounts");
        syncMode(data && data.mode);
        render(data);
        setMessage("Loaded " + data.count + " account(s).");
      } catch (err) { setMessage(err.message, true); }
    }
    async function saveMode() {
      const mode = document.getElementById("mode").value;
      if (!confirm("Set mode to \"" + mode + "\"?" + (mode === "delete" ? " Delete permanently removes credential files." : ""))) return;
      try {
        // /config is CPA's core plugin-config endpoint (shallow-merge), not a plugin route.
        await call("/config", {method: "PATCH", body: JSON.stringify({mode: mode})});
        syncMode(mode);
        setMessage("Mode saved: " + mode + " (applies live).");
      } catch (err) { setMessage(err.message, true); }
    }
    // Delegated handler: the Forget button carries the auth id in a data- attribute
    // (HTML-escaped), avoiding any inline-JS string interpolation / attribute escape.
    document.getElementById("list").addEventListener("click", function (e) {
      const btn = e.target.closest("button.forget-btn");
      if (btn) forget(btn.getAttribute("data-auth"));
    });
    async function forget(authID) {
      if (!confirm("Forget ban state for " + authID + "?")) return;
      try {
        const data = await call("/forget", {method: "POST", body: JSON.stringify({auth_id: authID})});
        render(data.status);
        setMessage(data.removed ? "Forgot: " + authID : "Not in ban list: " + authID);
      } catch (err) { setMessage(err.message, true); }
    }
    async function forgetAll() {
      if (!confirm("Clear all ban state?")) return;
      try {
        const data = await call("/forget", {method: "POST", body: JSON.stringify({all: true})});
        render(data.status);
        setMessage("Cleared " + data.removed + " account(s).");
      } catch (err) { setMessage(err.message, true); }
    }
    refresh();
  </script>
</body>
</html>`
}
