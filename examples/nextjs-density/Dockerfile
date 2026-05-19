# Minimal image for one idle Next.js standalone process under Bun.
# We assume `pnpm build` has already produced app/.next/standalone/
# (including the postbuild static + public copy). The image is a
# straight COPY + run — no install step, no compile inside Docker.
FROM oven/bun:1.3.14-alpine

WORKDIR /srv

COPY app/.next/standalone/ ./

ENV PORT=3000
EXPOSE 3000

CMD ["bun", "run", "server.js"]
