package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRuntimeValid(t *testing.T) {
	cases := []struct {
		r    Runtime
		want bool
	}{
		{Bun, true},
		{Node, true},
		{Deno, true},
		{Runtime("python"), false},
		{Runtime(""), false},
	}
	for _, c := range cases {
		if got := c.r.Valid(); got != c.want {
			t.Errorf("Runtime(%q).Valid() = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Runtime
		wantErr bool
	}{
		{"bun", Bun, false},
		{"BUN", Bun, false},
		{"  node  ", Node, false},
		{"Deno", Deno, false},
		{"python", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) unexpected err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCommand(t *testing.T) {
	cases := []struct {
		name   string
		r      Runtime
		entry  string
		extra  []string
		cmd    string
		args   []string
		hasErr bool
	}{
		{
			name: "bun no extras",
			r:    Bun, entry: "server.ts",
			cmd: "bun", args: []string{"server.ts"},
		},
		{
			name: "bun with extras",
			r:    Bun, entry: "server.ts", extra: []string{"--hot"},
			cmd: "bun", args: []string{"server.ts", "--hot"},
		},
		{
			name: "node",
			r:    Node, entry: "server.js",
			cmd: "node", args: []string{"server.js"},
		},
		{
			name: "deno gets allow-all",
			r:    Deno, entry: "server.ts",
			cmd: "deno", args: []string{"run", "-A", "server.ts"},
		},
		{
			name: "deno with extras",
			r:    Deno, entry: "server.ts", extra: []string{"--watch"},
			cmd: "deno", args: []string{"run", "-A", "server.ts", "--watch"},
		},
		{
			name: "empty entry rejected",
			r:    Bun, entry: "",
			hasErr: true,
		},
		{
			name: "invalid runtime rejected",
			r:    Runtime("python"), entry: "server.py",
			hasErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, args, err := Command(c.r, c.entry, c.extra)
			if c.hasErr {
				if err == nil {
					t.Errorf("expected error, got cmd=%q args=%v", cmd, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if cmd != c.cmd {
				t.Errorf("cmd = %q, want %q", cmd, c.cmd)
			}
			if !reflect.DeepEqual(args, c.args) {
				t.Errorf("args = %v, want %v", args, c.args)
			}
		})
	}
}

func TestCommandEmptyEntryReturnsSentinel(t *testing.T) {
	_, _, err := Command(Bun, "", nil)
	if !errors.Is(err, ErrEmptyEntry) {
		t.Errorf("expected ErrEmptyEntry, got %v", err)
	}
}

func TestDetect(t *testing.T) {
	t.Run("empty dir → no signal", func(t *testing.T) {
		dir := t.TempDir()
		_, err := Detect(dir)
		if !errors.Is(err, ErrNoSignal) {
			t.Errorf("want ErrNoSignal, got %v", err)
		}
	})

	t.Run("deno.json → Deno", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "deno.json"), `{}`)
		got, err := Detect(dir)
		if err != nil {
			t.Fatalf("Detect err: %v", err)
		}
		if got != Deno {
			t.Errorf("got %q, want Deno", got)
		}
	})

	t.Run("deno.jsonc → Deno", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "deno.jsonc"), `{}`)
		got, _ := Detect(dir)
		if got != Deno {
			t.Errorf("got %q, want Deno", got)
		}
	})

	t.Run("bun.lockb → Bun", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "bun.lockb"), "")
		writeFile(t, filepath.Join(dir, "package.json"), `{}`)
		got, _ := Detect(dir)
		if got != Bun {
			t.Errorf("got %q, want Bun", got)
		}
	})

	t.Run("package.json with bun-types → Bun", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "package.json"),
			`{"devDependencies":{"bun-types":"^1.0.0"}}`)
		got, _ := Detect(dir)
		if got != Bun {
			t.Errorf("got %q, want Bun", got)
		}
	})

	t.Run("package.json without bun signal → Node", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "package.json"),
			`{"dependencies":{"express":"^4.0.0"}}`)
		got, _ := Detect(dir)
		if got != Node {
			t.Errorf("got %q, want Node", got)
		}
	})

	t.Run("deno.json takes precedence over package.json", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "deno.json"), `{}`)
		writeFile(t, filepath.Join(dir, "package.json"), `{}`)
		got, _ := Detect(dir)
		if got != Deno {
			t.Errorf("got %q, want Deno (deno.json should win)", got)
		}
	})

	t.Run("bun.lockb wins over package.json without bun deps", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "bun.lockb"), "")
		writeFile(t, filepath.Join(dir, "package.json"),
			`{"dependencies":{"express":"^4.0.0"}}`)
		got, _ := Detect(dir)
		if got != Bun {
			t.Errorf("got %q, want Bun (bun.lockb should win)", got)
		}
	})

	t.Run("invalid package.json falls through to Node", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "package.json"), `{not valid json`)
		got, err := Detect(dir)
		if err != nil {
			t.Fatalf("Detect err: %v", err)
		}
		if got != Node {
			t.Errorf("got %q, want Node (broken package.json should default)", got)
		}
	})
}

func TestAllListsRuntimes(t *testing.T) {
	all := All()
	if len(all) != 4 {
		t.Errorf("All() length = %d, want 4", len(all))
	}
	want := map[Runtime]bool{Bun: false, Node: false, Deno: false, Workers: false}
	for _, r := range all {
		want[r] = true
	}
	for r, present := range want {
		if !present {
			t.Errorf("All() missing %q", r)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
