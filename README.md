# codex-fail-autoban

A CPA (CLIProxyAPI) plugin: **when a Codex credential's auth token is invalidated, automatically disable *or* delete that account (your choice).**

一个 CPA（CLIProxyAPI）插件：**当某个 Codex 账号的认证令牌失效时，自动禁用或删除该账号（可选）。**

It reacts to exactly this kind of upstream failure:

```json
{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","type":"authentication_error","code":"auth_unavailable"}}
```

Unlike a 429 rate-limit (temporary), an invalidated token is terminal: the account cannot recover until you re-authenticate it. So this plugin removes it from rotation permanently — either by disabling it (kept for later recovery) or deleting the credential file outright.

> Companion to [`codex-429-autoban`](https://github.com/ysxk/codex-429-autoban). That one temporarily bans on 429 and auto-unbans when quota resets. This one permanently handles *auth* failures. Run both together if you want.

## What it does

1. **Detects the auth failure.** After every request, the plugin inspects the usage record. It acts only when **all** of these hold:
   - the credential's provider is in `providers` (default: `codex`);
   - the request **failed**;
   - a **specific account was selected** (the record has an `auth_id`);
   - the failure looks like a terminal auth error — HTTP status in `match-status-codes` (default `401`) **or** the failure body contains one of `match-body-substrings` (default includes `authentication_error`, `auth_unavailable`, `invalid_api_key`, …);
   - and the body does **not** contain any `ignore-body-substrings` (default guards `no auth available`).
2. **Removes it from rotation immediately.** The plugin's scheduler hook drops the account from candidate selection right away, so in-flight/parallel requests stop hitting the dead credential without waiting for anything to reload.
3. **Applies the durable action** (`mode`):
   - `disable` (default): writes `"disabled": true` into the credential's `.json` file. CPA's file watcher reloads it as a disabled auth — it stays skipped across restarts, and the file is kept so you can re-enable it later.
   - `delete`: removes the credential `.json` file. CPA's watcher evicts it from the pool.
4. **Only touches configured providers.** Non-codex credentials are never inspected or modified. As a safety net, before acting the plugin re-reads the target file's `type` and refuses to act if it is outside `providers`.

### Why it will not nuke your pool on a transient "no auth available"

CPA reuses the `auth_unavailable` **code** for two very different situations:

| Situation | `auth_id` present? | `type` | Acted on? |
|---|---|---|---|
| A specific Codex account's token was invalidated upstream | **yes** | `authentication_error` | **yes** |
| The whole pool is empty ("no auth available") | **no** (nothing was selected) | — | **no** |

The plugin requires a selected `auth_id` **and** an auth-error signature, and it vetoes on `no auth available`. So the empty-pool case can never cause an account to be banned.

## Configuration

Enable and configure it under `plugins.configs` in your CPA `config.yaml`:

```yaml
plugins:
  enabled: true
  configs:
    codex-fail-autoban:
      enabled: true
      priority: 100          # higher runs earlier; keep it high so the ban is applied before other schedulers
      mode: disable          # "disable" (default) or "delete"
      # dry-run: true        # log the decision but change nothing (great for a first run)
      # debug: true          # log every failed request the plugin sees (see Troubleshooting)

      # Advanced (all optional — sensible defaults shown):
      # providers: [codex]
      # match-status-codes: [401]
      # match-body-substrings: ["authentication_error", "auth_unavailable", "invalidated", "oauth token", "unauthorized", "expired token", "invalid_grant", ...]
      # ignore-body-substrings: ["no auth available"]
```

| Key | Default | Meaning |
|---|---|---|
| `mode` | `disable` | `disable` = write `disabled:true` into the credential file (kept, survives restart). `delete` = remove the credential file. |
| `providers` | `[codex]` | Provider keys the plugin acts on. |
| `match-status-codes` | `[401]` | HTTP failure status codes that trigger a ban. |
| `match-body-substrings` | auth signatures | Case-insensitive needles in the failure body that trigger a ban. Defaults cover the common auth wordings (`authentication_error`, `auth_unavailable`, `invalidated`, `oauth token`, `unauthorized`, `expired token`, `invalid_grant`, …). |
| `ignore-body-substrings` | `["no auth available"]` | Case-insensitive needles that veto a ban (empty-pool guard). |
| `dry-run` | `false` | When true, log the decision but never modify or delete a file. The account is still dropped from *this run's* scheduling. |
| `debug` | `false` | When true, log every failed request the plugin observes (provider, auth id, status, body, decision + reason). |

List keys accept either a YAML sequence (`[a, b]`) or a single comma-separated string (`"a,b"`). A list key **replaces** the default (it is not merged), so include every value you want. Config changes are picked up on `plugin.reconfigure` without a full restart.

### Troubleshooting — an account wasn't disabled/deleted

Set `debug: true` and reproduce. For every failed request the plugin logs a line tagged `codex-fail-autoban: [debug] observed failed request` with `provider`, `auth_id`, `status_code`, `body`, `would_act`, and `reason`. That tells you exactly why:

- **No debug line at all when the error happens** → the failing account's request never reached the plugin as a failed usage record (CPA may have retried/failed-over). 
- **`provider="…"` that isn't in your `providers`** → add that provider key to `providers`.
- **`would_act=false` with a `reason`** (e.g. `no status/body match (status=…)`) → add that status to `match-status-codes` or a needle from the body to `match-body-substrings`.
- **`auth_id` empty** → it was the pool-empty error, correctly ignored.

## Install

### 1. Prepare a C compiler (CGO is required)

CPA plugins are native shared libraries and must be built with CGO, so you need a C compiler.

On Windows, install MinGW-w64:

```powershell
winget install -e --id MartinStorsjo.LLVM-MinGW.UCRT
```

Confirm `gcc --version` works.

### 2. Build

```powershell
cd codex-fail-autoban
.\build.ps1            # Windows
# or
bash build.sh          # any platform
```

This produces `codex-fail-autoban.dll` (Windows), `.dylib` (macOS), or `.so` (Linux).

> The plugin vendors CPA's `pluginabi` / `pluginapi` contract locally under `cpasdk/` and vendors `gopkg.in/yaml.v3` under `vendor/`, so **Go 1.21+** is enough — you do **not** need the Go 1.26 that building the full CLIProxyAPI requires, and the build is fully offline.

### 3. Drop it into the CPA plugins directory

CPA searches (Windows amd64 example):

```
plugins/windows/amd64-<variant>/
plugins/windows/amd64/
plugins/
```

Put the library there (recommended: `plugins/windows/amd64/codex-fail-autoban.dll`).

**Plugin id = file name without the extension**, i.e. `codex-fail-autoban`.

### 4. Enable it in `config.yaml`

See [Configuration](#configuration) above.

> If your CPA binary was built without plugin support, the response header `X-CPA-Support-Plugin: 1` will be absent — you need a CGO-built CPA.

## Plugin store install

This repo ships a `registry.json`. After you publish a GitHub release, add the raw URL to `plugins.store-sources`, then refresh the management panel and search the store:

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/tsunheimat/codex-fail-autoban/main/registry.json
```

## Management API & resource page

A resource page (also shown in the CPA management UI plugin menu):

```text
/v0/resource/plugins/codex-fail-autoban/status
```

The page shows the active **mode** and includes a **disable / delete switch** — enter your CPA management key, pick the mode, and **Save** (it applies live via `PATCH /v0/management/plugins/codex-fail-autoban/config`, no restart). It also lists handled accounts with a per-account **Forget**.

API (requires the CPA management key; supports `Authorization: Bearer <key>`):

```bash
# List accounts the plugin has disabled/deleted
curl -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://localhost:8317/v0/management/plugins/codex-fail-autoban/accounts

# Forget one account's in-memory ban (after you re-authenticate it)
curl -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"auth_id":"<AUTH_ID>"}' \
  http://localhost:8317/v0/management/plugins/codex-fail-autoban/forget

# Forget all
curl -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"all":true}' \
  http://localhost:8317/v0/management/plugins/codex-fail-autoban/forget
```

> **`forget` only clears this plugin's in-memory ban list** so a re-authenticated account can be scheduled again in the current process. It does **not** restore a deleted credential file, and it does **not** un-set `disabled:true` on disk — to bring a disabled account back, re-enable it in the CPA management UI (or re-login). 「forget」只清除插件内存中的封禁，不会恢复已删除的文件，也不会撤销磁盘上的 `disabled:true`。

## How it works (and a design note)

```
request completes → usage.handle (plugin observes)
  ├─ not a configured provider / not failed / no auth_id / not an auth error → ignore
  └─ terminal auth failure on account X
        ├─ add X to the in-memory ban set (scheduler drops it immediately)
        ├─ resolve X's file via host.auth.get (auth_index → path + json)
        └─ mode=disable → write disabled:true to the file
           mode=delete  → remove the file
                 └─ CPA's file watcher reconciles the pool (disabled / evicted)

next request selects a credential → scheduler.pick (plugin intervenes)
  └─ drops banned account IDs from the candidate set
```

**Design note — why `disable` writes the file directly instead of calling `host.auth.save`.** The plugin host's `host.auth.save` callback rebuilds the auth record as *active* and, with the default file store, rewrites `disabled:false` in the same call — so saving `disabled:true` through it is silently undone. Writing the credential file directly (then letting CPA's watcher pick it up, exactly as it does for a manual edit) is the reliable way for a plugin to durably disable an account. The immediate scheduler-hook exclusion covers the brief window before the watcher reconciles.

## State & logging

- The in-memory ban set is per-process and starts empty on restart. Durability comes from the on-disk action: a deleted file stays gone, and a `disabled:true` file loads back as disabled.
- Actions and errors are logged through CPA's logger (`slog`), tagged `codex-fail-autoban:`.
- Nothing is sent anywhere; the plugin only reads usage records and touches credential files in your CPA auth directory.

## Files

| File | Purpose |
|---|---|
| `main.go` | cgo shim: native plugin ABI + host reverse-call wiring (thin). |
| `bridge_cgo.go` | cgo host reverse-call helpers (kept separate from the `//export` file). |
| `internal/autoban/` | All logic (detection, disable/delete, scheduler, management) — cgo-free and unit-tested. |
| `cpasdk/pluginabi`, `cpasdk/pluginapi` | Vendored CPA plugin contract (so Go 1.26 is not required). |
| `build.ps1` / `build.sh` | Build scripts. |
| `registry.json` | Plugin-store manifest. |

## License

MIT — see [LICENSE](LICENSE).
