# Agent Guide for creekd / creekctl

This CLI is frequently invoked by AI agents. These invariants
encode what agents cannot intuit from `--help` alone.

## Output format

- `--json` on any subcommand returns machine-readable JSON.
- Set `OUTPUT_FORMAT=json` or `NO_TTY=1` to auto-enable JSON
  without passing the flag on every call.

## Mutating commands

`up`, `rm`, `deploy` modify state. Always validate first:

```bash
creekctl up my-app --runtime bun --port 3000 --dry-run
```

`--dry-run` validates all inputs and prints what would happen
without executing. Use it before every mutating operation.

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

## Common patterns

```bash
# List apps — minimal fields for context efficiency
creekctl ps --json --fields id,status,port

# Spawn — validate first, then execute
creekctl up my-app --runtime bun --port 3000 --dry-run
creekctl up my-app --runtime bun --port 3000

# Spawn with raw JSON payload
creekctl up my-app --json-input '{"runtime":"bun","entry":"src/index.ts","port":3000}' --dry-run
creekctl up my-app --json-input '{"runtime":"bun","entry":"src/index.ts","port":3000}'

# Blue-green deploy — validate first
creekctl deploy my-app --runtime bun --port 3001 --dry-run
creekctl deploy my-app --runtime bun --port 3001

# Check resource usage — specific fields only
creekctl stats my-app --json --fields id,memory_current_bytes,oom_kills

# Introspect before calling
creekctl describe up
```
