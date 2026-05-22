package creektoml

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	toml := `
[app]
name = "my-blog"
runtime = "node"

[database]
driver = "postgres"

[cache]
driver = "redis"

[storage]
driver = "fs"
`
	path := writeTempFile(t, "creek.toml", toml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.App.Name != "my-blog" {
		t.Errorf("App.Name = %q, want %q", cfg.App.Name, "my-blog")
	}
	if cfg.App.Runtime != "node" {
		t.Errorf("App.Runtime = %q, want %q", cfg.App.Runtime, "node")
	}
	if cfg.Database.Driver != "postgres" {
		t.Errorf("Database.Driver = %q, want %q", cfg.Database.Driver, "postgres")
	}
	if cfg.Cache.Driver != "redis" {
		t.Errorf("Cache.Driver = %q, want %q", cfg.Cache.Driver, "redis")
	}
}

func TestLoadDefaults(t *testing.T) {
	toml := `
[app]
name = "minimal"
`
	path := writeTempFile(t, "creek.toml", toml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.App.Runtime != "bun" {
		t.Errorf("default runtime = %q, want %q", cfg.App.Runtime, "bun")
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("default database = %q, want %q", cfg.Database.Driver, "sqlite")
	}
	if cfg.Cache.Driver != "sqlite" {
		t.Errorf("default cache = %q, want %q", cfg.Cache.Driver, "sqlite")
	}
	if cfg.Storage.Driver != "fs" {
		t.Errorf("default storage = %q, want %q", cfg.Storage.Driver, "fs")
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	toml := `
[app]
name = "discover-test"
runtime = "deno"
`
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if cfg.App.Name != "discover-test" {
		t.Errorf("App.Name = %q, want %q", cfg.App.Name, "discover-test")
	}
	if cfg.App.Runtime != "deno" {
		t.Errorf("App.Runtime = %q, want %q", cfg.App.Runtime, "deno")
	}
}

func TestDiscoverNotFound(t *testing.T) {
	_, err := Discover(t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing creek.toml")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "valid full",
			cfg: Config{
				App:      AppConfig{Runtime: "bun"},
				Database: DatabaseConfig{Driver: "postgres"},
				Cache:    CacheConfig{Driver: "redis"},
				Storage:  StorageConfig{Driver: "fs"},
			},
		},
		{
			name: "valid minimal defaults",
			cfg: Config{
				App:      AppConfig{Runtime: "bun"},
				Database: DatabaseConfig{Driver: "sqlite"},
				Cache:    CacheConfig{Driver: "sqlite"},
				Storage:  StorageConfig{Driver: "fs"},
			},
		},
		{
			name: "invalid runtime",
			cfg: Config{
				App:      AppConfig{Runtime: "python"},
				Database: DatabaseConfig{Driver: "sqlite"},
				Cache:    CacheConfig{Driver: "sqlite"},
				Storage:  StorageConfig{Driver: "fs"},
			},
			wantErr: true,
		},
		{
			name: "valid mysql driver",
			cfg: Config{
				App:      AppConfig{Runtime: "bun"},
				Database: DatabaseConfig{Driver: "mysql"},
				Cache:    CacheConfig{Driver: "sqlite"},
				Storage:  StorageConfig{Driver: "fs"},
			},
		},
		{
			name: "valid s3 storage",
			cfg: Config{
				App:      AppConfig{Runtime: "bun"},
				Database: DatabaseConfig{Driver: "sqlite"},
				Cache:    CacheConfig{Driver: "sqlite"},
				Storage:  StorageConfig{Driver: "s3"},
			},
		},
		{
			name: "invalid database driver",
			cfg: Config{
				App:      AppConfig{Runtime: "bun"},
				Database: DatabaseConfig{Driver: "mongodb"},
				Cache:    CacheConfig{Driver: "sqlite"},
				Storage:  StorageConfig{Driver: "fs"},
			},
			wantErr: true,
		},
		{
			name: "invalid cache driver",
			cfg: Config{
				App:      AppConfig{Runtime: "bun"},
				Database: DatabaseConfig{Driver: "sqlite"},
				Cache:    CacheConfig{Driver: "memcached"},
				Storage:  StorageConfig{Driver: "fs"},
			},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRequiredPrimitives(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "full stack",
			cfg: Config{
				App:      AppConfig{Runtime: "bun"},
				Database: DatabaseConfig{Driver: "postgres"},
				Cache:    CacheConfig{Driver: "redis"},
			},
			want: []string{"runtime-bun", "postgres", "redis"},
		},
		{
			name: "minimal sqlite only",
			cfg: Config{
				App:      AppConfig{Runtime: "node"},
				Database: DatabaseConfig{Driver: "sqlite"},
				Cache:    CacheConfig{Driver: "sqlite"},
			},
			want: []string{"runtime-node", "sqlite"},
		},
		{
			name: "deno with postgres no redis",
			cfg: Config{
				App:      AppConfig{Runtime: "deno"},
				Database: DatabaseConfig{Driver: "postgres"},
				Cache:    CacheConfig{Driver: "sqlite"},
			},
			want: []string{"runtime-deno", "postgres"},
		},
		{
			name: "mysql with redis and s3",
			cfg: Config{
				App:      AppConfig{Runtime: "bun"},
				Database: DatabaseConfig{Driver: "mysql"},
				Cache:    CacheConfig{Driver: "redis"},
				Storage:  StorageConfig{Driver: "s3"},
			},
			want: []string{"runtime-bun", "mysql", "redis", "s3"},
		},
		{
			name: "full stack with email",
			cfg: Config{
				App:      AppConfig{Runtime: "node"},
				Database: DatabaseConfig{Driver: "postgres"},
				Cache:    CacheConfig{Driver: "redis"},
				Storage:  StorageConfig{Driver: "s3"},
				Email:    EmailConfig{Enabled: true},
			},
			want: []string{"runtime-node", "postgres", "redis", "s3", "smtp"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.cfg.RequiredPrimitives()
			if len(got) != len(c.want) {
				t.Fatalf("RequiredPrimitives() = %v, want %v", got, c.want)
			}
			for i, g := range got {
				if g != c.want[i] {
					t.Errorf("RequiredPrimitives()[%d] = %q, want %q", i, g, c.want[i])
				}
			}
		})
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	path := writeTempFile(t, "creek.toml", "this is not valid toml [[[")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
