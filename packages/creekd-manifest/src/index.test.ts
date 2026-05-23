// Unit tests covering specific validator behaviour. The wider
// accept/reject contract against the shared Go testdata corpus
// lives in src/corpus.test.ts.

import { describe, expect, it } from "vitest";

import {
  isCreekdDeployManifest,
  isCreekdRuntime,
  validateCreekdDeployManifest,
  type CreekdDeployManifest,
} from "./index.js";

const goodManifest: CreekdDeployManifest = {
  version: 1,
  framework: "nextjs",
  target: "creekd",
  buildId: "test-build",
  nextVersion: "16.2.3",
  adapter: { name: "@solcreek/adapter-creekd", version: "0.1.0" },
  hasMiddleware: false,
  hasPrerender: true,
  runtime: "bun",
  entrypoint: ".next/standalone/server.js",
  port: 18900,
  serveDirs: [".next/standalone"],
};

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
      expect(result.manifest.port).toBe(18900);
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
