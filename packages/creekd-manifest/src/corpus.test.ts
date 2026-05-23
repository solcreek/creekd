// Executable contract corpus — the *same* JSON fixtures the Go
// validator runs against (api/manifest/testdata/), via relative
// path so neither side can drift without breaking CI.
//
// If you add or remove a fixture, both this suite and the Go
// TestCorpus* suites pick it up automatically — there's no list
// to keep in sync.

import { describe, expect, it } from "vitest";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

import {
  isCreekdDeployManifest,
  validateCreekdDeployManifest,
} from "./index.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
// packages/creekd-manifest/src → repo root → api/manifest/testdata
const TESTDATA = path.resolve(__dirname, "..", "..", "..", "api", "manifest", "testdata");

function loadCorpus(dir: "valid" | "invalid"): string[] {
  const full = path.join(TESTDATA, dir);
  const names = fs.readdirSync(full).filter((n) => n.endsWith(".json"));
  if (names.length === 0) {
    throw new Error(`corpus ${dir}/ is empty — did you forget to add a fixture?`);
  }
  return names;
}

type ParseResult = { ok: true; value: unknown } | { ok: false; error: Error };

function readFixture(dir: "valid" | "invalid", name: string): ParseResult {
  // Some invalid fixtures (e.g. trailing-content.json) aren't even
  // single-document JSON — JSON.parse throws before we get a chance
  // to call the manifest validator. That still counts as TS
  // rejection for contract purposes, so we surface parse errors
  // through ParseResult instead of letting them blow up the test
  // helper itself.
  const text = fs.readFileSync(path.join(TESTDATA, dir, name), "utf8");
  try {
    return { ok: true, value: JSON.parse(text) };
  } catch (err) {
    return { ok: false, error: err as Error };
  }
}

describe("shared contract corpus", () => {
  describe("valid/ — must accept", () => {
    for (const name of loadCorpus("valid")) {
      it(name, () => {
        const parsed = readFixture("valid", name);
        if (!parsed.ok) {
          throw new Error(`valid fixture ${name} failed to parse as JSON: ${parsed.error.message}`);
        }
        const result = validateCreekdDeployManifest(parsed.value);
        if (!result.ok) {
          throw new Error(`TS validator rejected ${name} that Go accepts: ${result.reason}`);
        }
        expect(isCreekdDeployManifest(parsed.value)).toBe(true);
      });
    }
  });

  describe("invalid/ — must reject", () => {
    for (const name of loadCorpus("invalid")) {
      it(name, () => {
        const parsed = readFixture("invalid", name);
        // Fixtures that aren't valid JSON (trailing-content.json
        // and friends) count as rejection — both Go's Decode and
        // TS's JSON.parse refuse them, which is the parity we
        // care about.
        if (!parsed.ok) return;
        const result = validateCreekdDeployManifest(parsed.value);
        if (result.ok) {
          throw new Error(`TS validator accepted ${name} that Go rejects`);
        }
        expect(isCreekdDeployManifest(parsed.value)).toBe(false);
      });
    }
  });

  // Roundtrip: parse → serialize → parse → deep-equal.
  // Deliberately does NOT compare serialized text against the
  // fixture file (whitespace / key order aren't part of contract).
  describe("valid/ — semantic roundtrip", () => {
    for (const name of loadCorpus("valid")) {
      it(name, () => {
        const text = fs.readFileSync(path.join(TESTDATA, "valid", name), "utf8");
        const m1 = JSON.parse(text);
        const m2 = JSON.parse(JSON.stringify(m1));
        expect(m2).toEqual(m1);
      });
    }
  });
});
