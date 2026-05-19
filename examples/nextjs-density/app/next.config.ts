import { createRequire } from "node:module";

const require = createRequire(import.meta.url);

const config = {
  // The adapter sets output: 'standalone' itself in modifyConfig and
  // writes .creek-creekd/manifest.json in onBuildComplete. Wiring it
  // here via Next.js's official NextAdapter extension point is the
  // entire user-facing surface.
  adapterPath: require.resolve("@solcreek/adapter-creekd"),
  // Pin Turbopack's workspace root so it doesn't walk up and pick a
  // parent lockfile (e.g. /Users/<me>/pnpm-lock.yaml from a side
  // project), which would cause the standalone tree to be emitted at
  // a wrong path inside .next/standalone/.
  turbopack: { root: import.meta.dirname },
};

export default config;
