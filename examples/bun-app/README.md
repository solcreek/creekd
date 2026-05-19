# bun-app

A real Bun HTTP server running under creekd via the `--runtime bun --entry server.ts` path. Proves the multi-runtime dispatch story is wired all the way through: `creekctl up`, dispatch routing, log capture, cgroup limits, the lot.

## What it shows

- **`--runtime bun --entry <path>`** resolves through `internal/runtime/runtime.go: Command()` to `bun server.ts`. No prebuilt binary — creekd spawns Bun, Bun runs the script.
- **`Bun.serve()`** for HTTP, **`bun:sqlite`** for an in-memory visit counter, **SSE streaming** at `/events` — all the things that exist *because* it's Bun and not Node.
- **Long-lived streaming through the dispatch listener.** The dispatch reverse proxy forwards `/events` SSE for as long as the client holds the connection — no buffering, no timeout-mid-stream.

## Run it

```bash
./up.sh
```

Then:

```bash
curl -H 'X-Creek-App: bun-demo' http://127.0.0.1:9000/
# hello from bun 1.3.14 (version=664a10f, pid=...)

curl -H 'X-Creek-App: bun-demo' http://127.0.0.1:9000/db
# {"runtime":"bun","bun":"1.3.14","visits":2,"version":"664a10f"}

# Live SSE stream — emits one event per second. Ctrl-C to stop.
curl -N -H 'X-Creek-App: bun-demo' http://127.0.0.1:9000/events
# data: {"tick":0,"ts":1715998800123}
# data: {"tick":1,"ts":1715998801124}
# ...
```

Tear down with `./down.sh`.

## What changes here vs the other examples

The previous examples (`pm2-replacement`, `sandboxed-eval`, `review-apps`) all use a Go-built toy. They prove cgroup, sandbox, dispatch, deploy — but they don't prove the multi-runtime story. This example uses the **same runtime-detection + spawn path** the production deploy flow does, with no fakery: Bun runs the TypeScript directly, no transpile step.

A user with a Bun app can take the `creekctl up --runtime bun --entry path/to/main.ts --port 3000` line, swap in their own entry, and they're done.

The Node and Deno paths use the same shape (`--runtime node --entry main.js`, `--runtime deno --entry main.ts`) — see `internal/runtime/runtime.go` for the exec resolution.
