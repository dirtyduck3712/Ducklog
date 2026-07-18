---
name: ducklog-integration
description: Wire a Go service's logs into ducklog (→ VictoriaLogs) so they're queryable by the ducklog MCP tools. Use when the user asks to "接 ducklog" / "integrate ducklog" / "send logs to ducklog or VictoriaLogs" / "hook up ducklog logging" in a Go project. Detects whether the service uses log/slog or go.uber.org/zap and applies the correct wiring, gated behind a DUCKLOG_VL_URL env toggle. Also migrates an existing OLD-style integration (local replace / bare `ducklog/client` import / removed `APIKey` field) to the published `go get` modules — use when the user says "migrate ducklog", "更新 ducklog 接法", "改 go get", or a rebuild fails with `unknown field APIKey`. Also covers registering the ducklog MCP server in Claude Code.
tools: Read, Glob, Grep, Bash, Edit
---

# ducklog Integration

Wire a Go service into ducklog so its logs land in VictoriaLogs and become queryable by AI via the ducklog MCP tools.

Architecture: `service ──log transport──▶ VictoriaLogs ◀──LogsQL── ducklog-mcp ◀── Claude Code`

The authoritative reference (paths, field mapping, gotchas, FAQ) is `docs/INTEGRATION.md` in the ducklog repo. Read it if anything here is unclear.

**Fresh integration or migration?** If the service already wires ducklog the
old way (local `replace`, bare `import "ducklog/client"`, or an `APIKey` field),
go to **§Migrate**. Otherwise continue with the fresh-install steps below.

## Locate ducklog

Find the ducklog repo (contains `client/`, `zapsink/`, `cmd/ducklog-mcp/`). Default local path: `/home/dva/workspace/ducklog`. If not there, ask the user for the path. You need it to build the MCP server (Step 4); the transport modules themselves are `go get`-able (Step 2), no local path required.

## Migrate — an existing (old-style) integration

Use this **instead of Steps 1–2** when the service was integrated the old way.
Detect it first, in the service's module root:
```bash
grep -rn '"ducklog/client"\|"ducklog/zapsink"' --include='*.go' .  # bare imports (not github.com/...)
grep -n  'ducklog/client\|ducklog/zapsink' go.mod                 # old replace/require wiring
grep -rn 'APIKey' --include='*.go' .                              # the breaking field
```

**Safety gate:** if the wiring is non-standard — multiple `RemoteConfig{}`
sites, custom fields, or a *vendored copy* of `client/` rather than a
`replace` — STOP and confirm the plan with the user, showing the intended
diff, before editing.

Do both (full migration):

1. **Remove the `APIKey` field — REQUIRED.** ducklog removed
   `RemoteConfig.APIKey`; any literal that still sets it fails to compile with
   `unknown field APIKey in struct literal of type client.RemoteConfig`. Delete
   the `APIKey: ...` line from every `RemoteConfig{...}`. Do **not** add
   `Username`/`Password` unless the service actually needs Basic Auth against an
   auth-enabled VL.

2. **Move module resolution from local `replace` to `go get`.** In the service's
   module root:
   ```bash
   go mod edit -dropreplace=ducklog/client -droprequire=ducklog/client
   go get github.com/dirtyduck3712/ducklog/client@latest
   # zap services also:
   go mod edit -dropreplace=ducklog/zapsink -droprequire=ducklog/zapsink
   go get github.com/dirtyduck3712/ducklog/zapsink@latest
   ```
   Then update imports: `ducklog/client` → `github.com/dirtyduck3712/ducklog/client`
   (same for `zapsink`). `zapsink` needs Go ≥ 1.24 — if the service is on an
   older Go, flag it to the user; do not bump their toolchain silently.

Then run **Step 3** (verify build/vet) and **Step 4** (run & verify).

> The module rename **alone** does NOT break old `replace`-based builds — a
> filesystem `replace` maps the import to the local dir regardless of that dir's
> `go.mod` module name (`go build`/`go mod tidy`/`go vet` all pass). The only
> rebuild-breaker is the removed `APIKey` field. Already-deployed binaries are
> unaffected either way — this migration is for the *next* rebuild, not a
> running-prod emergency.

## Step 1 — Detect the service's logging library

In the target Go service (its own module):
- `grep -rl 'log/slog' --include='*.go'` and `grep -rl 'go.uber.org/zap' --include='*.go'`
- Find the **main entrypoint** where the logger is constructed (`cmd/*/main.go`). That's the single wiring site.
- If it uses **stdlib slog** → §2a. If it uses **zap** → §2b. If both (zap main + scattered slog), wire the **main logger** (§2b) and note the scattered slog calls also flow once you `slog.SetDefault` (optional, §2a).

## Step 2 — Module wiring (go.mod)

The transport modules are published on a public repo — `go get`, no copy/replace:
```
go get github.com/dirtyduck3712/ducklog/client@latest
```
For zap services also:
```
go get github.com/dirtyduck3712/ducklog/zapsink@latest
```
Go version: `client` needs Go ≥ 1.22, `zapsink` needs Go ≥ 1.24.

## Step 2a — slog service

At the top of the startup function (before the first log), add:
```go
import "github.com/dirtyduck3712/ducklog/client"

if vlURL := os.Getenv("DUCKLOG_VL_URL"); vlURL != "" {
    h := client.NewRemoteHandler(client.RemoteConfig{
        Endpoint: vlURL + "/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service",
        Service:  "<service-name>",
        Fallback: os.Stderr,
    })
    defer h.Close()
    slog.SetDefault(slog.New(h))
}
```

## Step 2b — zap service

Wrap the zap core via `zap.WrapCore` where the logger is built. Make the constructor return a cleanup func the caller defers:
```go
import (
    "github.com/dirtyduck3712/ducklog/client"
    "github.com/dirtyduck3712/ducklog/zapsink"
    "go.uber.org/zap"
    "go.uber.org/zap/zapcore"
)

var opts []zap.Option
cleanup := func() {}
if vlURL := os.Getenv("DUCKLOG_VL_URL"); vlURL != "" {
    opts = append(opts, zap.WrapCore(func(base zapcore.Core) zapcore.Core {
        core, stop := zapsink.Tee(base, client.RemoteConfig{
            Endpoint: vlURL + "/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service",
            Service:  "<service-name>",
            Fallback: io.Discard, // zap console is already the local net; avoid double stdout
        })
        cleanup = stop
        return core
    }))
}
logger, err := cfg.Build(opts...)   // then: caller does `defer cleanup()`
```
Warn the user: zap `logger.Fatal` calls `os.Exit`, skipping the deferred cleanup — the last line before a Fatal may not flush (rely on the 1s ticker, or shut down gracefully).

## Step 3 — Verify the build

Run the service's own build/vet for the changed package (e.g. `go build ./cmd/<svc>/ && go vet ./cmd/<svc>/`). It must be clean. Do NOT run the full service unless it's a lightweight one with no side effects — heavy services connect to real infra and may start background workers.

## Step 4 — Tell the user how to run & verify

- Run the service normally with `DUCKLOG_VL_URL=http://127.0.0.1:9428` (unset = unchanged behavior).
- Verify in vmui: `http://127.0.0.1:9428/select/vmui`, query `service:=<name>`. **Remind them: vmui's default time range is 5 minutes — widen it or they'll see nothing.**
- Register the MCP once: `go build -o ~/bin/ducklog-mcp <ducklog>/cmd/ducklog-mcp` then `claude mcp add ducklog --env VL_URL=http://127.0.0.1:9428 -- ~/bin/ducklog-mcp`.

## Guardrails

- Everything is gated on `DUCKLOG_VL_URL` — never change logging behavior when it's unset.
- Only touch the one logger-construction site + go.mod. Don't rewrite the service's logging calls.
- Never run a heavy/production service just to "see logs" without the user's OK — it may hit real DBs and start workers.
- The transport already coerces non-serializable attr values to strings and never drops a batch on one bad entry (v1 fix) — don't add your own sanitizing.
