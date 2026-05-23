# @solcreek/creekd-manifest

TypeScript types and strict validator for the **creekd deployment manifest** — the contract that `.creek-creekd/manifest.json` files written by deployment adapters must satisfy for `creekctl up --from` to spawn the supervised process.

This package is the TypeScript side of a cross-language contract. The canonical Go side lives at [`github.com/solcreek/creekd/api/manifest`](https://github.com/solcreek/creekd/tree/main/api/manifest) inside the same repo, and both validators run against the same shared testdata corpus in CI so the two languages **can't drift**.

## Install

```bash
pnpm add @solcreek/creekd-manifest
```

## Usage

Adapters writing manifest files:

```ts
import {
  type CreekdDeployManifest,
  isCreekdDeployManifest,
} from "@solcreek/creekd-manifest";

const manifest: CreekdDeployManifest = {
  version: 1,
  target: "creekd",
  runtime: "node",
  entrypoint: "build/index.js",
  port: 3000,
  env: ["NODE_ENV=production"],
  health_check_path: "/_creek/health",
  framework: "sveltekit",
  adapter: { name: "@my/adapter", version: "1.0.0" },
};

if (!isCreekdDeployManifest(manifest)) {
  throw new Error("manifest contract violation");
}
await fs.writeFile(
  ".creek-creekd/manifest.json",
  JSON.stringify(manifest, null, 2) + "\n",
);
```

Tools consuming user-written manifests (give a useful error, not just `false`):

```ts
import { validateCreekdDeployManifest } from "@solcreek/creekd-manifest";

const result = validateCreekdDeployManifest(parsed);
if (!result.ok) {
  console.error(`invalid manifest: ${result.reason}`);
  process.exit(1);
}
// result.manifest is fully typed as CreekdDeployManifest here
```

## Manifest shape

| Field | Type | Required | Notes |
|---|---|---|---|
| `version` | `1` | ✓ | Schema version. Currently only v1. |
| `target` | `"creekd"` | ✓ | Distinguishes from manifests for other deployment targets. |
| `runtime` | `"bun" \| "node" \| "deno"` | ✓ | Runtime creekd spawns for the entrypoint. |
| `entrypoint` | `string` | ✓ | Relative path from project root. No absolute paths, no `..` traversal. |
| `port` | `number` (1..65535) | ✓ | TCP port the process listens on. |
| `env` | `string[]` | — | KEY=VALUE strings. |
| `health_check_path` | `string` | — | HTTP path for creekd's liveness probe. |
| `serveDirs` | `string[]` | — | Directories the runtime serves (informational; reserved for future creekd fast-path). |
| `framework` | `string` | — | Informational (e.g. "nextjs", "sveltekit"). |
| `buildId` | `string` | — | Informational, useful in logs. |
| `nextVersion` | `string` | — | Informational, set by Next.js adapters. |
| `hasMiddleware` | `boolean` | — | Informational. |
| `hasPrerender` | `boolean` | — | Informational. |
| `adapter` | `{ name, version }` | — | Identifies the adapter that wrote the manifest. Strict-validated: only `name` and `version` are allowed. |

## Unknown-field policy

**Strict at the top level.** Unknown top-level keys are rejected with the offending field name in the error, so a typo like `entryPont` fails immediately at validation time rather than silently producing `entrypoint: ""` that explodes downstream:

```ts
validateCreekdDeployManifest({
  version: 1,
  target: "creekd",
  runtime: "node",
  entryPont: "server.js",  // typo
  port: 3000,
});
// → { ok: false, reason: 'unknown top-level field "entryPont"' }
```

This applies recursively: the `adapter` object is also strict (only `name` and `version` are accepted).

**Adding new top-level fields requires a coordinated rollout.** Under strict mode, an older `creekd` rejects manifests that contain a field it doesn't know about, so the order is: bump `creekd` to a version that recognises the new field, then bump adapters to a `@solcreek/creekd-manifest` version that writes it. The reverse order leaves a window where adapters produce manifests `creekd` refuses to load.

If you need to attach truly adapter-private extension data, the recommended approach is to put it elsewhere (a separate file in `.creek-creekd/`, environment variables, etc.) until a future `metadata: Record<string, unknown>` extension slot is added to v1.

## Compatibility

| `@solcreek/creekd-manifest` | manifest schema version | minimum `creekd` (Go) |
|---|---|---|
| `0.1.x` | v1 | `≥ 0.5.0` |

When a future v2 ships, the rule will be: bump `creekd` first to a version that reads v2, then bump adapters to a `@solcreek/creekd-manifest@0.2.x` that writes v2. The reverse order leaves a window where adapters write manifests creekd can't read.

## Cross-language verification

Every fixture in [`api/manifest/testdata/`](../../api/manifest/testdata/) is run through both the Go validator (`api/manifest/manifest.go`) and this TypeScript validator. The `manifest-contract` CI workflow fails if they disagree on any file.

To add a fixture:

1. Drop a `.json` file in the appropriate `testdata/valid/` or `testdata/invalid/` directory.
2. Both Go and TS corpus tests pick it up on next run (no manifest of fixtures to maintain).

To add a new top-level field:

1. Add it to the Go struct (`api/manifest/manifest.go`) with the appropriate `json:` tag.
2. Add it to `CreekdDeployManifest` and to `ALLOWED_TOP_LEVEL_KEYS` in `src/index.ts`.
3. Add a fixture in `testdata/valid/` that exercises it.

CI will catch any divergence.

## License

Apache-2.0
