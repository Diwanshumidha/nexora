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
                                    JSON-RPC over stdio                       │
             ┌───────────────────────────┬────────────────────────────────────┤
             ▼                           ▼                                    ▼
      node --permission            node --permission                  (planned) MCP
      extensions/excel             extensions/word                    server adapter
```

## Extensions

An extension is a folder with a `nexora.json` manifest and a bundled entry
file. The manager discovers them from:

1. `~/.config/nexora/extensions/<id>`: installed by the user (removable)
2. `NEXORA_DEV_EXTENSIONS`: dev folders, such as the `extensions/` folder of a
   nexora-extensions checkout (`wails3 task dev` sets this)

Users install and update extensions from a catalog (`internal/registry`): the
`index.json` of a nexora-extensions release, with each zip verified against
its SHA-256 before it is unpacked.

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

- MCP client: `runtime: "mcp"` extensions wrapping external MCP servers.
- Per-run permission prompts and an audit log of tool calls.
- Install extensions from a registry or an archive (`.nxext`), with signature checks.
- Chat UI plus an LLM provider layer that drives Code Mode.
