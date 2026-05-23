// Unit tests covering specific validator behaviour. The wider
// accept/reject contract against the shared Go testdata corpus
// lives in src/corpus.test.ts.

import { describe, expect, it } from "vitest";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

import {
  isCreekdDeployManifest,
  isCreekdRuntime,
  validateCreekdDeployManifest,
  type CreekdDeployManifest,
} from "./index.js";

// Same shared fixture the cross-language corpus uses. Loaded from
// api/manifest/testdata/valid/nextjs-full.json so updates to the
// canonical "full valid manifest" flow through unit tests too.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const goodManifest: CreekdDeployManifest = JSON.parse(
  fs.readFileSync(
    path.resolve(
      __dirname,
      "..",
      "..",
      "..",
      "api",
      "manifest",
      "testdata",
      "valid",
      "nextjs-full.json",
    ),
    "utf8",
  ),
);

describe("isCreekdRuntime", () => {
  it.each(["bun", "node", "deno"])("accepts %s", (r) => {
    expect(isCreekdRuntime(r)).toBe(true);
  });

  it.each(["python", "", "Bun", undefined, 0])("rejects %p", (v) => {
    expect(isCreekdRuntime(v)).toBe(false);
  });
});

describe("validateCreekdDeployManifest happy path", () => {
  it("accepts a full manifest", () => {
    const result = validateCreekdDeployManifest(goodManifest);
    expect(result.ok).toBe(true);
    if (result.ok) {
      expect(result.manifest.runtime).toBe("bun");
      expect(result.manifest.port).toBe(3000);
    }
  });

  it("isCreekdDeployManifest narrows to the right type", () => {
    const raw: unknown = goodManifest;
    expect(isCreekdDeployManifest(raw)).toBe(true);
    if (isCreekdDeployManifest(raw)) {
      // Type narrowed — these accesses must compile.
      expect(raw.runtime).toBe("bun");
      expect(raw.adapter?.name).toBe("@solcreek/adapter-creekd");
    }
  });
});

describe("validateCreekdDeployManifest rejections (descriptive errors)", () => {
  it("rejects unknown top-level fields with the offending name", () => {
    const bad = { ...goodManifest, entryPont: "server.js" } as unknown;
    const result = validateCreekdDeployManifest(bad);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.reason).toContain("entryPont");
    }
  });

  it("rejects wrong version", () => {
    const bad = { ...goodManifest, version: 999 } as unknown;
    const result = validateCreekdDeployManifest(bad);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.reason).toContain("unsupported manifest version");
    }
  });

  it("rejects wrong target", () => {
    const bad = { ...goodManifest, target: "cloudflare" } as unknown;
    const result = validateCreekdDeployManifest(bad);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.reason).toContain("creekd");
    }
  });

  it("rejects bad runtime", () => {
    const bad = { ...goodManifest, runtime: "python" } as unknown;
    const result = validateCreekdDeployManifest(bad);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.reason).toContain("bun");
    }
  });

  it("rejects port out of range", () => {
    const result = validateCreekdDeployManifest({ ...goodManifest, port: 70000 });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toContain("out of range");
  });

  it("rejects float port", () => {
    const result = validateCreekdDeployManifest({ ...goodManifest, port: 3.14 });
    expect(result.ok).toBe(false);
  });

  it("rejects missing entrypoint", () => {
    const result = validateCreekdDeployManifest({ ...goodManifest, entrypoint: "" });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toContain("missing entrypoint");
  });

  it("rejects absolute entrypoint", () => {
    const result = validateCreekdDeployManifest({ ...goodManifest, entrypoint: "/etc/passwd" });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toContain("absolute");
  });

  it("rejects Windows-style absolute entrypoint", () => {
    const result = validateCreekdDeployManifest({ ...goodManifest, entrypoint: "C:\\windows\\system32" });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toContain("absolute");
  });

  it.each([
    "../escape.js",
    "./../escape.js",
    ".next/../../escape.js",
    "..",
  ])("rejects traversal entrypoint %s", (ep) => {
    const result = validateCreekdDeployManifest({ ...goodManifest, entrypoint: ep });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toContain("escapes");
  });

  it("rejects unknown field inside adapter object", () => {
    const bad = {
      ...goodManifest,
      adapter: { name: "x", version: "y", extraField: "z" },
    } as unknown;
    const result = validateCreekdDeployManifest(bad);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toContain("extraField");
  });

  it("rejects non-object root", () => {
    expect(validateCreekdDeployManifest(null).ok).toBe(false);
    expect(validateCreekdDeployManifest("string").ok).toBe(false);
    expect(validateCreekdDeployManifest([]).ok).toBe(false);
    expect(validateCreekdDeployManifest(42).ok).toBe(false);
  });
});
