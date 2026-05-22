package creektoml

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/BurntSushi/toml"
)

const Filename = "creek.toml"

type Config struct {
	App      AppConfig      `toml:"app"`
	Database DatabaseConfig `toml:"database"`
	Cache    CacheConfig    `toml:"cache"`
	Storage  StorageConfig  `toml:"storage"`
	Email    EmailConfig    `toml:"email"`
}

type AppConfig struct {
	Name    string `toml:"name"`
	Runtime string `toml:"runtime"`
}

type DatabaseConfig struct {
	Driver string `toml:"driver"`
}

type CacheConfig struct {
	Driver string `toml:"driver"`
}

type StorageConfig struct {
	Driver string `toml:"driver"`
}

type EmailConfig struct {
	Enabled bool `toml:"enabled"`
}

var (
	validRuntimes       = []string{"bun", "node", "deno"}
	validDatabaseDriver = []string{"sqlite", "postgres", "mysql"}
	validCacheDriver    = []string{"sqlite", "redis"}
	validStorageDriver  = []string{"fs", "s3"}
)

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("creektoml: %w", err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("creektoml: parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func Discover(dir string) (*Config, error) {
	return Load(filepath.Join(dir, Filename))
}

func (c *Config) applyDefaults() {
	if c.App.Runtime == "" {
		c.App.Runtime = "bun"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Cache.Driver == "" {
		c.Cache.Driver = "sqlite"
	}
	if c.Storage.Driver == "" {
		c.Storage.Driver = "fs"
	}
}

func (c *Config) Validate() error {
	if c.App.Runtime != "" && !slices.Contains(validRuntimes, c.App.Runtime) {
		return fmt.Errorf("creektoml: invalid runtime %q (valid: %v)", c.App.Runtime, validRuntimes)
	}
	if !slices.Contains(validDatabaseDriver, c.Database.Driver) {
		return fmt.Errorf("creektoml: invalid database.driver %q (valid: %v)", c.Database.Driver, validDatabaseDriver)
	}
	if !slices.Contains(validCacheDriver, c.Cache.Driver) {
		return fmt.Errorf("creektoml: invalid cache.driver %q (valid: %v)", c.Cache.Driver, validCacheDriver)
	}
	if !slices.Contains(validStorageDriver, c.Storage.Driver) {
		return fmt.Errorf("creektoml: invalid storage.driver %q (valid: %v)", c.Storage.Driver, validStorageDriver)
	}
	return nil
}

func (c *Config) RequiredPrimitives() []string {
	var prims []string

	prims = append(prims, "runtime-"+c.App.Runtime)

	switch c.Database.Driver {
	case "postgres":
		prims = append(prims, "postgres")
	case "mysql":
		prims = append(prims, "mysql")
	case "sqlite":
		prims = append(prims, "sqlite")
	}

	if c.Cache.Driver == "redis" {
		prims = append(prims, "redis")
	}

	if c.Storage.Driver == "s3" {
		prims = append(prims, "s3")
	}

	if c.Email.Enabled {
		prims = append(prims, "smtp")
	}

	return prims
}
