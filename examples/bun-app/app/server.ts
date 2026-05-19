// A small Bun HTTP server demonstrating the kind of thing creekd is
// designed to host:
//   - Bun.serve() (native HTTP, no Express / Hono / etc. — keeps the
//     example dependency-free)
//   - A live /events SSE stream (proves the dispatch listener forwards
//     long-lived streaming responses correctly through reverse proxy)
//   - A /db endpoint backed by Bun's built-in SQLite (proves the
//     "bun-only" runtime features like bun:sqlite work end-to-end)
//   - PORT comes from the env, the canonical Bun pattern
import { Database } from "bun:sqlite";

const port = Number(process.env.PORT ?? 8080);
const buildSha = process.env.APP_VERSION ?? "dev";

// In-memory SQLite — fast, no file. Each restart starts fresh.
const db = new Database(":memory:");
db.exec(`
  CREATE TABLE visits (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    path TEXT NOT NULL
  )
`);
const recordVisit = db.prepare("INSERT INTO visits (ts, path) VALUES (?, ?)");
const countVisits = db.prepare("SELECT COUNT(*) AS n FROM visits");

Bun.serve({
  port,
  fetch(req: Request) {
    const url = new URL(req.url);
    recordVisit.run(Date.now(), url.pathname);

    if (url.pathname === "/healthz") {
      return new Response("ok\n");
    }

    if (url.pathname === "/db") {
      const row = countVisits.get() as { n: number };
      return Response.json({
        runtime: "bun",
        bun: Bun.version,
        visits: row.n,
        version: buildSha,
      });
    }

    if (url.pathname === "/events") {
      // SSE: emit a tick once a second forever, until the client closes.
      const stream = new ReadableStream({
        async start(controller) {
          const encoder = new TextEncoder();
          let i = 0;
          const interval = setInterval(() => {
            const msg =
              `data: ${JSON.stringify({ tick: i++, ts: Date.now() })}\n\n`;
            try {
              controller.enqueue(encoder.encode(msg));
            } catch {
              clearInterval(interval);
            }
          }, 1000);
          req.signal.addEventListener("abort", () => {
            clearInterval(interval);
            controller.close();
          });
        },
      });
      return new Response(stream, {
        headers: {
          "Content-Type": "text/event-stream",
          "Cache-Control": "no-cache",
          Connection: "keep-alive",
        },
      });
    }

    return new Response(
      `hello from bun ${Bun.version} (version=${buildSha}, pid=${process.pid})\n`,
    );
  },
});

console.log(`bun-app listening on :${port} (bun ${Bun.version}, pid=${process.pid})`);
