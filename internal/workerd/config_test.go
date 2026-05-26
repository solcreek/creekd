package workerd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/creekd/internal/supervisor"
)

func TestGenerateConfigBasic(t *testing.T) {
	dir := t.TempDir()
	cfg := supervisor.Config{ID: "my-app", Port: 3000}
	path, err := GenerateConfig(cfg, "/data/worker.js", 3000, dir)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}
	data, _ := os.ReadFile(path)
	content := string(data)

	checks := []string{`name = "my-app"`, `embed "/data/worker.js"`, `address = "*:3000"`, `name = "PORT"`}
	for _, c := range checks {
		if !strings.Contains(content, c) {
			t.Errorf("missing %q", c)
		}
	}
}

func TestGenerateConfigWithEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := supervisor.Config{ID: "api", Port: 8080, Env: []string{"DB_URL=sqlite:///app.db", "APP=my-api"}}
	path, _ := GenerateConfig(cfg, "/w.js", 8080, dir)
	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, `name = "DB_URL"`) || !strings.Contains(content, `text = "sqlite:///app.db"`) {
		t.Error("missing DB_URL binding")
	}
	if !strings.Contains(content, `name = "APP"`) {
		t.Error("missing APP binding")
	}
}

func TestGenerateConfigErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateConfig(supervisor.Config{}, "/w.js", 3000, dir); err == nil {
		t.Error("expected error for empty ID")
	}
	if _, err := GenerateConfig(supervisor.Config{ID: "x"}, "", 3000, dir); err == nil {
		t.Error("expected error for empty entry")
	}
}

func TestGenerateConfigFileName(t *testing.T) {
	dir := t.TempDir()
	path, _ := GenerateConfig(supervisor.Config{ID: "test-app", Port: 3000}, "/w.js", 3000, dir)
	if path != filepath.Join(dir, "test-app.capnp") {
		t.Errorf("path = %q", path)
	}
}

func TestGenerateConfigBalancedParens(t *testing.T) {
	dir := t.TempDir()
	cfg := supervisor.Config{ID: "full", Port: 3000, Env: []string{"A=1", "B=2"}}
	path, _ := GenerateConfig(cfg, "/w.js", 3000, dir)
	data, _ := os.ReadFile(path)
	content := string(data)
	if strings.Count(content, "(") != strings.Count(content, ")") {
		t.Error("unbalanced parentheses in generated capnp")
	}
}
