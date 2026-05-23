// TypeScript side of the creekd deployment manifest contract.
//
// The canonical Go side lives at github.com/solcreek/creekd/api/manifest.
// Both validators must agree on accept/reject for every fixture in
// api/manifest/testdata/, enforced by the manifest-contract CI workflow.
//
// Adapters write a Manifest to .creek-creekd/manifest.json after a
// successful build; creekctl reads it via --from to seed SpawnRequest
// / DeployRequest.

/** Runtimes creekd can spawn. */
export type CreekdRuntime = "bun" | "node" | "deno";

/**
 * Identifies which adapter wrote the manifest. Useful for support /
 * debugging; creekd does not act on these values. Strict-validated:
 * only `name` and `version` are allowed.
 */
export interface AdapterMetadata {
  name: string;
  version: string;
}

/**
 * Process-level deployment manifest written by adapters and read by
 * creekctl. The top-level field set is strictly validated — unknown
 * keys are rejected to catch typos like `entryPont` before they cause
 * cryptic spawn-time failures.
 */
export interface CreekdDeployManifest {
  /** Manifest schema version. Increment on breaking shape changes. */
  version: 1;
  /** Deployment target. Keeps creekd manifests distinct from other targets. */
  target: "creekd";
  /** Runtime creekd should use for `entrypoint`. */
  runtime: CreekdRuntime;
  /** Entrypoint path relative to the project root. */
  entrypoint: string;
  /** Port the app process should listen on (1..65535). */
  port: number;
  /** Optional environment variables, encoded as KEY=VALUE strings. */
  env?: string[];
  /** Optional HTTP liveness probe path. */
  health_check_path?: string;
  /** Optional directories the adapter prepared for the runtime to serve/read. */
  serveDirs?: string[];

  // Informational metadata: creekd treats as opaque.
  framework?: string;
  buildId?: string;
  nextVersion?: string;
  adapter?: AdapterMetadata;
  hasMiddleware?: boolean;
  hasPrerender?: boolean;
}

// Allowed top-level keys. Mirrors the Go Manifest struct tags.
// Maintained by hand alongside the interface; CI catches drift via
// the shared testdata corpus.
const ALLOWED_TOP_LEVEL_KEYS = new Set([
  "version",
  "target",
  "runtime",
  "entrypoint",
  "port",
  "env",
  "health_check_path",
  "serveDirs",
  "framework",
  "buildId",
  "nextVersion",
  "adapter",
  "hasMiddleware",
  "hasPrerender",
]);

const ALLOWED_ADAPTER_KEYS = new Set(["name", "version"]);

const VALID_RUNTIMES: ReadonlySet<string> = new Set(["bun", "node", "deno"]);

/** Type predicate for {@link CreekdRuntime}. */
export function isCreekdRuntime(value: unknown): value is CreekdRuntime {
  return typeof value === "string" && VALID_RUNTIMES.has(value);
}

/**
 * Detailed validation result. Returned by {@link validateCreekdDeployManifest}
 * when callers want a specific error to show users. {@link isCreekdDeployManifest}
 * collapses this into a boolean for the common "is this safe to use" check.
 */
export type ManifestValidation =
  | { ok: true; manifest: CreekdDeployManifest }
  | { ok: false; reason: string };

function validateEntrypoint(ep: string): string | null {
  // Absolute path check — Posix and Windows.
  if (ep.startsWith("/") || /^[a-zA-Z]:[\\/]/.test(ep)) {
    return `entrypoint ${JSON.stringify(ep)} must be relative to the project root, not absolute`;
  }
  // Traversal check — any segment-resolved ".." that escapes.
  const segments = ep.split(/[\\/]/);
  let depth = 0;
  for (const seg of segments) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      depth--;
      if (depth < 0) {
        return `entrypoint ${JSON.stringify(ep)} escapes the project directory via ..`;
      }
    } else {
      depth++;
    }
  }
  return null;
}

/**
 * Strict validator. Returns a structured result describing why the
 * candidate isn't a valid manifest, or confirming it is. Matches the
 * Go `manifest.Load` rules exactly — top-level unknown keys are
 * rejected, runtime must be one of bun/node/deno, port must be a
 * 1..65535 integer, entrypoint must be a relative path that doesn't
 * escape its project root.
 */
export function validateCreekdDeployManifest(value: unknown): ManifestValidation {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return { ok: false, reason: "manifest must be a JSON object" };
  }
  const m = value as Record<string, unknown>;

  // Strict: reject unknown top-level keys (catches `entryPont` typos).
  for (const key of Object.keys(m)) {
    if (!ALLOWED_TOP_LEVEL_KEYS.has(key)) {
      return { ok: false, reason: `unknown top-level field ${JSON.stringify(key)}` };
    }
  }

  if (m.version !== 1) {
    return { ok: false, reason: `unsupported manifest version ${m.version} (only v1 is supported)` };
  }
  if (m.target !== "creekd") {
    return {
      ok: false,
      reason: `target=${JSON.stringify(m.target)} is not "creekd" — manifest written for a different deployment target`,
    };
  }
  if (!isCreekdRuntime(m.runtime)) {
    return { ok: false, reason: `runtime=${JSON.stringify(m.runtime)} is not "bun", "node", or "deno"` };
  }
  if (typeof m.entrypoint !== "string" || m.entrypoint.length === 0) {
    return { ok: false, reason: "missing entrypoint" };
  }
  const epErr = validateEntrypoint(m.entrypoint);
  if (epErr) return { ok: false, reason: epErr };

  if (typeof m.port !== "number" || !Number.isInteger(m.port) || m.port <= 0 || m.port > 65535) {
    return { ok: false, reason: `port=${m.port} out of range (1..65535)` };
  }

  if (m.env !== undefined) {
    if (!Array.isArray(m.env) || !m.env.every((e) => typeof e === "string")) {
      return { ok: false, reason: "env must be an array of strings" };
    }
  }
  if (m.health_check_path !== undefined && typeof m.health_check_path !== "string") {
    return { ok: false, reason: "health_check_path must be a string" };
  }
  if (m.serveDirs !== undefined) {
    if (!Array.isArray(m.serveDirs) || !m.serveDirs.every((d) => typeof d === "string")) {
      return { ok: false, reason: "serveDirs must be an array of strings" };
    }
  }
  if (m.framework !== undefined && typeof m.framework !== "string") {
    return { ok: false, reason: "framework must be a string" };
  }
  if (m.buildId !== undefined && typeof m.buildId !== "string") {
    return { ok: false, reason: "buildId must be a string" };
  }
  if (m.nextVersion !== undefined && typeof m.nextVersion !== "string") {
    return { ok: false, reason: "nextVersion must be a string" };
  }
  if (m.hasMiddleware !== undefined && typeof m.hasMiddleware !== "boolean") {
    return { ok: false, reason: "hasMiddleware must be a boolean" };
  }
  if (m.hasPrerender !== undefined && typeof m.hasPrerender !== "boolean") {
    return { ok: false, reason: "hasPrerender must be a boolean" };
  }

  if (m.adapter !== undefined) {
    if (!m.adapter || typeof m.adapter !== "object" || Array.isArray(m.adapter)) {
      return { ok: false, reason: "adapter must be a { name, version } object" };
    }
    const ad = m.adapter as Record<string, unknown>;
    for (const key of Object.keys(ad)) {
      if (!ALLOWED_ADAPTER_KEYS.has(key)) {
        return { ok: false, reason: `unknown field ${JSON.stringify(key)} in adapter metadata` };
      }
    }
    if (typeof ad.name !== "string" || ad.name.length === 0) {
      return { ok: false, reason: "adapter.name must be a non-empty string" };
    }
    if (typeof ad.version !== "string" || ad.version.length === 0) {
      return { ok: false, reason: "adapter.version must be a non-empty string" };
    }
  }

  return { ok: true, manifest: m as unknown as CreekdDeployManifest };
}

/**
 * Boolean type predicate. Equivalent to
 * `validateCreekdDeployManifest(value).ok === true` but narrows the
 * input type for downstream code.
 */
export function isCreekdDeployManifest(value: unknown): value is CreekdDeployManifest {
  return validateCreekdDeployManifest(value).ok;
}
