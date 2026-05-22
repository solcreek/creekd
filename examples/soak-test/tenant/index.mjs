// Soak test tenant app — works with both Bun and Node.
import { createServer } from "node:http";

const PORT = parseInt(process.env.PORT || "3000");
const APP_ID = process.env.APP_ID || "unknown";
const RUNTIME = typeof Bun !== "undefined" ? "bun" : "node";
const START = Date.now();

let allocations = [];

const server = createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${PORT}`);

  if (url.pathname === "/health") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ status: "ok" }));
    return;
  }

  if (url.pathname === "/") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({
      id: APP_ID,
      runtime: RUNTIME,
      pid: process.pid,
      uptime_ms: Date.now() - START,
    }));
    return;
  }

  if (url.pathname === "/crash") {
    res.writeHead(200);
    res.end("crashing");
    setTimeout(() => process.exit(1), 50);
    return;
  }

  if (url.pathname === "/leak") {
    const mb = parseInt(url.searchParams.get("mb") || "10");
    const chunk = Buffer.alloc(mb * 1024 * 1024, 0xAA);
    allocations.push(chunk);
    const totalMB = allocations.reduce((s, b) => s + b.length, 0) / 1024 / 1024;
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ allocated_mb: totalMB }));
    return;
  }

  res.writeHead(404);
  res.end("not found");
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`${APP_ID} (${RUNTIME}) listening on :${PORT}`);
});

process.on("SIGTERM", () => {
  server.close(() => process.exit(0));
});
