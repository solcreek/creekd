#!/usr/bin/env bash
# Bootstrap minimal SSR fixtures for 4 stacks under ./stacks/ on a
# Linux host. The bench script then spawns N copies of each and
# samples PSS via /proc/<pid>/smaps_rollup.
#
# Idempotent — re-running skips already-built stacks. Assumes
# `bun`, `node`, `pnpm`, `npm` are on PATH. On Ubuntu / Debian:
#
#   curl -fsSL https://bun.sh/install | bash && ln -sf ~/.bun/bin/bun /usr/local/bin/bun
#   curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && apt-get install -y nodejs
#   npm install -g pnpm
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$HERE/stacks"
cd "$HERE/stacks"

# ---------- 1. Bun raw (floor reference) ----------
# No framework, no router — `Bun.serve()` directly. Establishes the
# absolute lower bound of "JS runtime + one app process". Anything
# above this is framework overhead.
if [ ! -f bun-hello/server.js ]; then
  mkdir -p bun-hello
  cat > bun-hello/server.js <<'EOF'
Bun.serve({
  port: Number(process.env.PORT) || 3000,
  fetch(req) {
    const url = new URL(req.url);
    if (url.pathname === '/healthz') return new Response('ok\n');
    return new Response('hello from bun\n');
  },
});
EOF
  echo "[bootstrap] bun-hello: ok"
fi

# ---------- 2. Hono on Bun ----------
# Minimal API framework. Hono is ~3 MB shipped — close to the floor.
if [ ! -f hono/server.js ]; then
  mkdir -p hono && cd hono
  cat > package.json <<'EOF'
{"name":"hono-bench","type":"module","version":"1.0.0","dependencies":{"hono":"^4.6.0"}}
EOF
  bun install >/dev/null 2>&1
  cat > server.js <<'EOF'
import { Hono } from 'hono';
const app = new Hono();
app.get('/', c => c.text('hello from hono'));
app.get('/healthz', c => c.text('ok'));
export default { port: Number(process.env.PORT) || 3000, fetch: app.fetch };
EOF
  echo "[bootstrap] hono: ok"
  cd ..
fi

# ---------- 3. SvelteKit (Vite-based SSR) ----------
# Representative for Svelte / Vite ecosystem. Uses adapter-node which
# is Bun-compatible. The CLI is `sv` (modern) — passing flags to skip
# interactive prompts.
if [ ! -f sveltekit/build/index.js ]; then
  npx --yes sv@latest create sveltekit \
    --template minimal --types ts --no-add-ons --install pnpm 2>&1 | tail -1 || true
  cd sveltekit
  pnpm add -D @sveltejs/adapter-node >/dev/null 2>&1
  cat > svelte.config.js <<'EOF'
import adapter from '@sveltejs/adapter-node';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';
export default {
  preprocess: vitePreprocess(),
  kit: { adapter: adapter() },
};
EOF
  mkdir -p src/routes/healthz
  cat > src/routes/healthz/+server.ts <<'EOF'
export const GET = () => new Response('ok\n', { status: 200 });
EOF
  pnpm build >/dev/null 2>&1
  echo "[bootstrap] sveltekit: ok"
  cd ..
fi

# ---------- 4. Astro SSR (Vite-based, growing ecosystem) ----------
# Configured with @astrojs/node in "standalone" mode so the output is
# a self-contained Node-compatible entrypoint (Bun runs it fine).
if [ ! -f astro/dist/server/entry.mjs ]; then
  npm create astro@latest astro -- \
    --template minimal --skip-houston --no-install --no-git --yes >/dev/null 2>&1
  cd astro
  pnpm install >/dev/null 2>&1
  pnpm add @astrojs/node >/dev/null 2>&1
  cat > astro.config.mjs <<'EOF'
import { defineConfig } from 'astro/config';
import node from '@astrojs/node';
export default defineConfig({
  output: 'server',
  adapter: node({ mode: 'standalone' }),
});
EOF
  cat > src/pages/index.astro <<'EOF'
---
const greeting = 'hello from astro';
---
<html><body>{greeting}</body></html>
EOF
  cat > src/pages/healthz.ts <<'EOF'
export const GET = () => new Response('ok\n', { status: 200 });
EOF
  pnpm build >/dev/null 2>&1
  echo "[bootstrap] astro: ok"
  cd ..
fi

echo ""
echo "fixtures ready. measure with:  ./measure.sh"
ls -la \
  bun-hello/server.js \
  hono/server.js \
  sveltekit/build/index.js \
  astro/dist/server/entry.mjs 2>/dev/null | awk '{print "  ",$NF}'
