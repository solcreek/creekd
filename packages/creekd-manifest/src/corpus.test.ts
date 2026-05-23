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

function readFixture(dir: "valid" | "invalid", name: string): unknown {
  return JSON.parse(fs.readFileSync(path.join(TESTDATA, dir, name), "utf8"));
}

describe("shared contract corpus", () => {
  describe("valid/ — must accept", () => {
    for (const name of loadCorpus("valid")) {
      it(name, () => {
        const data = readFixture("valid", name);
        const result = validateCreekdDeployManifest(data);
        if (!result.ok) {
          throw new Error(`TS validator rejected ${name} that Go accepts: ${result.reason}`);
        }
        expect(isCreekdDeployManifest(data)).toBe(true);
      });
    }
  });

  describe("invalid/ — must reject", () => {
    for (const name of loadCorpus("invalid")) {
      it(name, () => {
        const data = readFixture("invalid", name);
        const result = validateCreekdDeployManifest(data);
        if (result.ok) {
          throw new Error(`TS validator accepted ${name} that Go rejects`);
        }
        expect(isCreekdDeployManifest(data)).toBe(false);
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
