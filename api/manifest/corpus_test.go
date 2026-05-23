package manifest

import (
	"embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Executable contract corpus shared with packages/creekd-manifest/.
// Both Go and TS validators must agree on accept/reject for every
// file in testdata/. If you add a fixture here, mirror the change
// in the TS package's testdata reader (it uses the SAME files via
// a relative path, so this is just "remember to bump TS tests").
//
// Layout:
//   testdata/valid/*.json    — must Load successfully and survive
//                              parse→serialize→parse roundtrip
//                              with semantic equality.
//   testdata/invalid/*.json  — must be rejected by Load (specific
//                              error message not asserted here;
//                              manifest_test.go covers wording).

//go:embed testdata/valid/*.json testdata/invalid/*.json
var corpusFS embed.FS

func loadCorpus(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := fs.ReadDir(corpusFS, "testdata/"+dir)
	if err != nil {
		t.Fatalf("read corpus %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("corpus %s/ is empty — did you forget to add a fixture?", dir)
	}
	return names
}

// writeCorpusFixture stages the embedded JSON as a real file in the
// expected .creek-creekd/ layout so Load() — which goes through
// os.ReadFile — sees a normal manifest on disk.
func writeCorpusFixture(t *testing.T, dir, name string) (mp, projectDir string) {
	t.Helper()
	body, err := corpusFS.ReadFile("testdata/" + dir + "/" + name)
	if err != nil {
		t.Fatalf("embed read %s/%s: %v", dir, name, err)
	}
	projectDir = t.TempDir()
	manifestDir := filepath.Join(projectDir, ".creek-creekd")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mp = filepath.Join(manifestDir, "manifest.json")
	if err := os.WriteFile(mp, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return mp, projectDir
}

func TestCorpusValidAccept(t *testing.T) {
	for _, name := range loadCorpus(t, "valid") {
		t.Run(name, func(t *testing.T) {
			mp, _ := writeCorpusFixture(t, "valid", name)
			m, _, err := Load(mp)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if m == nil {
				t.Fatal("Load returned nil manifest with no error")
			}
		})
	}
}

func TestCorpusInvalidReject(t *testing.T) {
	for _, name := range loadCorpus(t, "invalid") {
		t.Run(name, func(t *testing.T) {
			mp, _ := writeCorpusFixture(t, "invalid", name)
			_, _, err := Load(mp)
			if err == nil {
				t.Fatalf("Load accepted invalid fixture %s — both Go and TS validators must reject this", name)
			}
		})
	}
}

// TestCorpusValidRoundtrip exercises the canonical roundtrip the TS
// side will mirror: parse → re-marshal → parse → semantic equality.
// We deliberately do NOT diff the serialized output against the
// original file text — Go json.Marshal whitespace / key order differ
// from hand-written JSON and that brittleness adds zero value. The
// invariant we care about is that parse(serialize(parse(x))) == parse(x).
func TestCorpusValidRoundtrip(t *testing.T) {
	for _, name := range loadCorpus(t, "valid") {
		t.Run(name, func(t *testing.T) {
			body, err := corpusFS.ReadFile("testdata/valid/" + name)
			if err != nil {
				t.Fatal(err)
			}
			var m1 Manifest
			if err := json.Unmarshal(body, &m1); err != nil {
				t.Fatalf("first parse: %v", err)
			}
			out, err := json.Marshal(m1)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m2 Manifest
			if err := json.Unmarshal(out, &m2); err != nil {
				t.Fatalf("re-parse: %v", err)
			}
			// Use stdlib reflect.DeepEqual to honour creekd's
			// stdlib-first principle. Lose readable structural
			// diff but our Manifest has ~10 plain fields — dumping
			// both via %+v on failure is enough.
			if !reflect.DeepEqual(m1, m2) {
				t.Errorf("roundtrip mismatch for %s:\nfirst:  %+v\nsecond: %+v", name, m1, m2)
			}
		})
	}
}
