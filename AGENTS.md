# Agent Guide for creekd / creekctl

This CLI is frequently invoked by AI agents. These invariants
encode what agents cannot intuit from `--help` alone.

## Output format

- `--json` on any subcommand returns machine-readable JSON.
- Set `OUTPUT_FORMAT=json` or `NO_TTY=1` to auto-enable JSON
  without passing the flag on every call.

## Idempotent provisioning

**Prefer `ensure` over `up` for agent workflows.** It creates the
app if absent and no-ops if already running — no need to check `ps`
first:

```bash
creekctl ensure my-app --runtime bun --port 3000
```

This eliminates the two-step "ps → branch → up" pattern. Safe to
call repeatedly in retry loops or re-entrant pipelines.

## Mutating commands

`up`, `ensure`, `rm`, `deploy` modify state. Always validate first:

```bash
creekctl ensure my-app --runtime bun --port 3000 --dry-run
```

`--dry-run` validates all inputs and prints what would happen
without executing. Use it before every mutating operation.

## Error codes

API errors return structured JSON with a `code` field for
programmatic branching:

| Code | Meaning |
|---|---|
| `already_running` | App exists (use `ensure` to avoid this) |
| `not_found` | App ID not registered |
| `invalid_id` | ID fails grammar check |
| `port_conflict` | Port already in use |
| `deploy_unhealthy` | v2 failed health check during blue-green |
| `bad_request` | Generic validation failure |

## Schema introspection

```bash
creekctl describe          # list all commands as JSON
creekctl describe up       # full flag schema for "up"
creekctl describe deploy   # full flag schema for "deploy"
```

Use `describe` to discover accepted flags at runtime instead of
hard-coding flag lists from documentation.

## Sandbox (local development)

```bash
creekd sandbox ./my-app --non-interactive --json
```

- `--non-interactive`: provisions Lima VM without interactive prompts.
- `--json`: structured status output (VM name, ports, primitives).
- `--stop` / `--destroy`: VM lifecycle management.
- Reads `creek.toml` in the app directory for primitive declarations.

## Input rules

- App IDs: `[a-z0-9-]`, 1-63 chars, no leading hyphen.
- String flags (`--command`, `--entry`): control characters
  (< ASCII 0x20) are rejected.
- Paths: traversal (`..`) is rejected in entry points and volume
  mounts.

## Authentication

```bash
export CREEKCTL_SERVER="http://127.0.0.1:9080"
export CREEKCTL_TOKEN="your-token"
```

Use environment variables. No browser-redirect auth flows.

## Raw JSON input (agent-optimized)

For `up` and `deploy`, pass the full API payload directly instead of
individual flags. Maps 1:1 to the admin API schema — zero translation:

```bash
creekctl up my-app --json-input '{
  "runtime": "bun",
  "entry": "src/index.ts",
  "port": 3000,
  "limits": {"memory_high_bytes": 268435456}
}'
```

## Field filtering (context window discipline)

Use `--fields` on read commands to limit response size:

```bash
creekctl ps --json --fields id,status,port
creekctl get my-app --json --fields id,status,pid
creekctl stats my-app --json --fields id,memory_current_bytes,oom_kills
```

## Health-check ready event

When an app passes its first health probe after spawn, the event
stream emits a `ready` event with the URL:

```json
{"type":"ready","app_id":"my-app","status":"ready","pid":1234,"port":3000,"url":"http://127.0.0.1:3000","ts":"..."}
```

Agents should wait for this event instead of sleeping + polling.

## One-off command execution

```bash
creekctl exec -- rails console
creekctl exec -- bun run seed.ts
creekctl exec --app my-app -- psql "$DATABASE_URL"
```

Runs a command with the app's env vars injected. Equivalent to
`heroku run` or `railway run`. Inherits DATABASE_URL, REDIS_URL,
PORT from the running app.

## Database reset

```bash
creekctl db-reset --database-url "$DATABASE_URL"
```

Drops and recreates the database. Use between test runs for clean
state. Supports `--dry-run` and `--json`.

## Event stream (status monitoring)

```bash
creekctl events my-app
```

SSE stream of app state transitions. Blocks until disconnected.
Eliminates polling — the agent receives events as they happen:

```json
{"type":"status_changed","app_id":"my-app","status":"running","pid":1234,"ts":"..."}
{"type":"health_failure","app_id":"my-app","health_failures":3,"ts":"..."}
{"type":"status_changed","app_id":"my-app","status":"crashed","ts":"..."}
{"type":"status_changed","app_id":"my-app","status":"running","pid":1235,"ts":"..."}
```

Event types: `status_changed`, `health_failure`, `restart`, `oom_kill`.

Agent deploy-and-monitor pattern:
```bash
creekctl ensure my-app --runtime bun --port 3000   # spawn
creekctl events my-app                              # monitor (blocks)
```

## Common patterns

```bash
# List apps — minimal fields for context efficiency
creekctl ps --json --fields id,status,port

# Idempotent spawn (preferred for agents — safe to retry)
creekctl ensure my-app --runtime bun --port 3000

# Idempotent spawn with raw JSON
creekctl ensure my-app --json-input '{"runtime":"bun","entry":"src/index.ts","port":3000}'

# Non-idempotent spawn (errors if already running)
creekctl up my-app --runtime bun --port 3000

# Blue-green deploy — validate first
creekctl deploy my-app --runtime bun --port 3001 --dry-run
creekctl deploy my-app --runtime bun --port 3001

# Check resource usage — specific fields only
creekctl stats my-app --json --fields id,memory_current_bytes,oom_kills

# Introspect before calling
creekctl describe up
```
