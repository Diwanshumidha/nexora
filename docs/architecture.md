# Architecture

```
┌────────────────────────── Nexora desktop (Go + Wails) ──────────────────────────┐
│                                                                                 │
│  frontend (React) ──bindings──▶ services ──▶ extension.Manager ──▶ Process ─┐   │
│                                     │              ▲                        │   │
│                                     ▼              │ CallTool               │   │
│                              codemode.Runtime ─────┘                        │   │
│                              (goja sandbox, only `tools`)                   │   │
└─────────────────────────────────────────────────────────────────────────────┼───┘
                       Nexora JSON-RPC over stdio                 MCP         │
             ┌───────────────────────────┬────────────────────────────────────┤
             ▼                           ▼                                    ▼
      node --permission            node --permission              MCP server: local
      extensions/excel             extensions/word                (stdio) or remote
                                                                  (streamable HTTP)
```

## Extensions

An extension is a folder with a `nexora.json` manifest and a bundled entry
file. The manager discovers them from:

1. `~/.config/nexora/extensions/<id>`: installed by the user (removable)
2. `NEXORA_DEV_EXTENSIONS`: dev folders, such as the `extensions/` folder of a
   nexora-extensions checkout (`wails3 task dev` sets this)

Users install and update extensions from a catalog (`internal/registry`): the
`index.json` of a nexora-extensions release, with each zip verified against
its SHA-256 before it is unpacked. `CatalogService` keeps the last catalog in
memory and in `catalog.json` (keyed by its URL): `List` answers from it while
it is under 15 minutes old and falls back to it, flagged `offline`, when a
fetch fails. A background check every 6 hours refreshes it, and `Updates`
reads only the saved copy, so the UI can poll for update badges without
network traffic. Package files (`.nxext`/`.zip`) go through the same
unpacking limits (`registry.OpenArchive`), accepting a single top-level
folder, and update an installed extension unless that would downgrade it.

### Isolation

- **Process per extension.** A crash or memory leak takes down one extension,
  not the app. Tool calls to a dead extension fail cleanly.
- **Node permission model.** Extensions are launched with `--permission`.
  They can read their own folder and read/write their private data dir
  (`~/.config/nexora/extension-data/<id>`). Everything else must be declared
  in the manifest under `permissions` (`fsRead`, `fsWrite`, `network`,
  `childProcess`), which the UI can show to the user before installing.
- **Minimal environment.** Only `PATH`, `HOME`, `LANG` and `TMPDIR` are passed
  through, so no secrets leak via environment variables.
- **Self-contained bundles.** Extensions are bundled with esbuild into one
  file, so they never rely on a shared `node_modules`.

### Protocol

Newline-delimited JSON-RPC 2.0 over stdin/stdout (see
`internal/protocol/protocol.go`, mirrored in the nexora-extensions repo's `sdk/extension-sdk/src/protocol.ts`).
The manifest's `protocol` field versions this contract (`extension.HostProtocol`).

| Direction   | Method       | Purpose                                         |
| ----------- | ------------ | ----------------------------------------------- |
| host → ext  | `initialize` | Handshake; extension returns its tool specs     |
| host → ext  | `tools/call` | Invoke a tool with JSON args                    |
| host → ext  | `shutdown`   | Graceful stop (host kills after 5s)             |
| ext → host  | `log`        | Structured log notification                     |

Tools are declared at runtime during `initialize`, so the manifest only holds
static metadata and permissions.

### MCP-server extensions

An extension with `"runtime": "mcp"` wraps an existing MCP server instead of
running Nexora SDK code (`internal/extension/mcpclient.go`, built on the
official Go SDK's client). Its tools appear under `tools.<namespace>.*` and
go through the same permission gate, history and approvals as any other.

```json
{
  "id": "github", "name": "GitHub", "version": "1.0.0", "runtime": "mcp",
  "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
  "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "${token}" },
  "settings": [{ "key": "token", "type": "secret", "label": "Access token", "required": true }]
}
```

- **Transports.** `command`/`args`/`env` launch a local server over stdio;
  `url`/`headers` connect to a remote one over streamable HTTP.
- **Templating.** Values may reference settings as `${key}`, plus `${HOME}`,
  `${DATA}` (private data dir) and `${DIR}` (extension dir). An argument
  that is exactly `${folders}` expands to one argument per folder. Unknown
  references are rejected when the manifest loads. Changing a setting
  restarts the server.
- **Tool mapping.** Names that aren't JS identifiers are rewritten
  (`get-issue` → `get_issue`). `readOnlyHint` maps to `ReadOnly`, so in *Ask
  first* mode read tools run without a prompt. Results: `structuredContent`
  as is, text content as a string, anything else (images, resources) as the
  list of MCP content blocks; `isError` results throw.
  `notifications/tools/list_changed` refreshes the tool list live.
- **Isolation.** A local MCP server is **not** sandboxed (it is an arbitrary
  program, not Node code we can launch with `--permission`): it runs with
  the user's privileges, gets only a minimal environment (`PATH`, `HOME`,
  `USER`, `LANG`, `TMPDIR`, plus the Windows essentials) and its manifest
  `env`, and on Linux is killed if Nexora dies. The UI says so.
- **OAuth.** A remote server without an `Authorization` header gets an
  OAuth handler (`internal/extension/mcpoauth.go`) wrapping the SDK's
  `AuthorizationCodeHandler`: protected-resource and authorization-server
  discovery, dynamic client registration, PKCE, a loopback redirect
  (`http://127.0.0.1:<random port>/callback`) and refresh. Only
  `Manager.SignIn` (the panel's **Sign in** button) may open the browser;
  on start-up, or for a 401 mid-session, the request fails with
  `ErrSignInRequired` and `Info.Auth` becomes `required`. Client and token
  live in `extension-settings/oauth/<id>.json` (0600), bound to the server
  URL, and are deleted on sign-out or removal.
- **Adding one.** The **+** panel's *Add an MCP server* form
  (`extension.NewMCPManifest`) writes a manifest-only extension. Its env
  vars and headers become settings, so tokens live in the 0600 settings
  store (masked as secrets), not in `nexora.json`.

## Code Mode

Inspired by [Cloudflare's Code Mode](https://blog.cloudflare.com/code-mode/).
LLMs are better at writing code than at chaining many tool calls, and a script
can loop, filter and combine results without sending every intermediate value
back through the model.

1. `codemode.GenerateTypes` turns every running extension's tool specs (JSON
   Schema) into a TypeScript declaration of `tools`. This is the prompt
   context the model programs against.
2. The model returns a script. `codemode.Runtime.Run` executes it as an
   `async` function body in a fresh goja VM per run, with a timeout.
3. The VM has **no** fs, network, timers or modules. Its only capabilities
   are `console` (captured) and `tools.<namespace>.<tool>()`, which proxy to
   `Manager.CallTool`.

### tools.store

`internal/cache` is a persistent key-value cache exposed to every script as
`tools.store` (the namespace is reserved). The runner routes `store` calls to
it before the permission gate, so they need no approval, but they are still
recorded in the run's timeline. Its API is generated from ordinary
`ToolSpec`s, so the model sees it in `get_api` like any extension.

Cleanup is built in rather than left to scripts: every entry has a TTL
(default 1 day, max 30 days); a write that would exceed the size or entry
budget evicts expired entries, then the least recently used; a sweeper drops
expired entries every minute; and loading drops anything that expired while
the app was closed (and clamps stored expiries to the current maximum).
Writes to `cache.json` are batched (1 s) and atomic; reads update the LRU
time in memory and are persisted with the next write. A damaged file is set
aside and the cache starts empty.

### Tool call performance

- **Parallel calls.** goja is single-threaded, so each run drives a small
  event loop: every tool call runs on its own goroutine (capped by
  `MaxConcurrency`, default 16) and posts its result back to the VM
  goroutine, which settles the promise. `Promise.all` over independent calls
  overlaps them. Benchmark: 8 calls × 10 ms finish in ~11 ms (previously ~80 ms).
- **Zero-copy IPC.** Tool args go from `JSON.stringify` in the VM straight
  into the request frame. The frame envelope is written around them without
  re-encoding, and payloads over 64 KB are written as separate pieces with
  `writev` instead of being copied. Results are handed back raw and decoded
  once by `JSON.parse` in the VM, which gives native JS objects.
- **Concurrent extensions.** Each process handles requests concurrently.
  Responses are matched by id, so slow calls don't block fast ones.
- **O(1) routing.** Namespace → extension lookup uses a map.

Run `go test -tags gtk3 -run x -bench . -benchmem ./internal/...` to measure.

## Job queue and MCP endpoint

Every script, whether from the UI or an external AI, goes through
`jobs.Queue` via `runner.Runner`:

- **Bounded FIFO with a worker pool.** `NEXORA_WORKERS` jobs run at once
  (each can still run up to 16 tool calls in parallel). Up to
  `NEXORA_MAX_QUEUED` wait in order. Beyond that, submissions fail fast with
  "queue is full" instead of piling up.
- **Lifecycle.** queued → running → succeeded / failed / cancelled. Jobs can
  be cancelled while queued or running, are capped by `NEXORA_JOB_TIMEOUT`,
  survive panics, and stay retrievable for an hour after finishing.
- **Unguessable IDs** (`job_` + 128 random bits): a job ID is the only thing
  needed to read its result.

The MCP server (`internal/mcp`) mounts the queue behind four tools:

```
AI ──run_code──▶ submit ──▶ wait ≤ inline limit ──▶ done?  ── yes ─▶ result
                                                      └─ no ─▶ {status: running, jobId}
AI ──get_job───▶ wait ≤ inline limit ──▶ result, or still running (call again)
```

The inline limit (80 s) sits below common proxy timeouts (Cloudflare drops
requests at 100 s with a 524). Clients that send a progress token also get
progress notifications every 10 s while they wait. The HTTP transport is
stateless, so a reconnect or a different proxy edge doesn't lose anything:
all state is in the queue. Auth is a 256-bit token, sent either as a bearer
header or as a secret path segment (`/mcp/<token>`) for clients that only
accept a URL.

## Network layer

The MCP endpoint only listens on `127.0.0.1`. Built-in providers in
`internal/network` make it reachable from outside, and `network.Gateway`
keeps **at most one live at a time**:

```
ChatGPT ──https──▶ tailscaled (Funnel) ─┐
                   cloudflared ─────────┴──▶ 127.0.0.1:7377/mcp/<token> ──▶ MCP server
```

- **In-process.** Providers implement `network.Provider` (`Expose`,
  `Status`, `Close`, `Available`) in Go. Tailscale uses tailscaled's
  LocalAPI client directly: typed serve config with ETag optimistic
  concurrency, no CLI subprocess. Cloudflare supervises `cloudflared`, which
  is killed by the kernel (`Pdeathsig`) if Nexora dies.
- **Contract.** `Expose` returns a base URL where `url + "/x"` reaches
  `target + "/x"`. The secret token is appended by the app; providers never
  see it.
- **One live provider.** `Activate` closes the current provider before
  exposing the new one. All provider calls are serialised.
- **Self-healing.** Every 20 s the gateway health-checks the exposure (e.g.
  someone ran `tailscale funnel reset`) and re-exposes, backing off 20 s →
  5 min on repeated failures. The active provider and its options persist in
  `network.json` and are restored on launch. Exposures are removed on quit.
- **Safety (Tailscale).** Only a dedicated path is mounted (default
  `/nexora` on port 8443). Funnel is per port, so the provider refuses to
  funnel a port that already serves other private handlers, and refuses
  tailnet mode on a port someone else funnels publicly. Leftover config from
  a crash is recognised by its target and reused.

## Roadmap

- OAuth for servers without dynamic client registration (a pre-registered
  client ID in the manifest).
- Optional OS-level sandboxing for local MCP servers (bubblewrap, sandbox-exec).
- Per-run permission prompts and an audit log of tool calls.
- Signed catalogs and packages, so a compromised download host can't serve
  a different build with matching checksums.
- Chat UI plus an LLM provider layer that drives Code Mode.
