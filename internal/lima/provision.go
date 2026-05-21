package lima

import (
	_ "embed"
	"fmt"
)

//go:embed scripts/postgres.sh
var scriptPostgres string

//go:embed scripts/redis.sh
var scriptRedis string

//go:embed scripts/sqlite.sh
var scriptSQLite string

//go:embed scripts/runtime-bun.sh
var scriptBun string

//go:embed scripts/runtime-node.sh
var scriptNode string

//go:embed scripts/runtime-deno.sh
var scriptDeno string

var scripts = map[string]string{
	"postgres":     scriptPostgres,
	"redis":        scriptRedis,
	"sqlite":       scriptSQLite,
	"runtime-bun":  scriptBun,
	"runtime-node": scriptNode,
	"runtime-deno": scriptDeno,
}

const markerDir = "/var/creek-sandbox"

func IsProvisioned(vm *VM, primitive string) (bool, error) {
	marker := fmt.Sprintf("%s/.provisioned-%s", markerDir, primitive)
	_, err := vm.ShellRoot(fmt.Sprintf("test -f %s", marker))
	return err == nil, nil
}

func MarkProvisioned(vm *VM, primitive string) error {
	marker := fmt.Sprintf("%s/.provisioned-%s", markerDir, primitive)
	_, err := vm.ShellRoot(fmt.Sprintf("mkdir -p %s && touch %s", markerDir, marker))
	return err
}

func Provision(vm *VM, primitive string) error {
	script, ok := scripts[primitive]
	if !ok {
		return fmt.Errorf("lima: unknown primitive %q", primitive)
	}

	provisioned, _ := IsProvisioned(vm, primitive)
	if provisioned {
		vm.Log.Info("primitive already provisioned", "primitive", primitive)
		return nil
	}

	vm.Log.Info("provisioning primitive", "primitive", primitive)
	if _, err := vm.ShellRoot(script); err != nil {
		return fmt.Errorf("lima: provision %s: %w", primitive, err)
	}

	return MarkProvisioned(vm, primitive)
}

func AvailablePrimitives() []string {
	out := make([]string, 0, len(scripts))
	for k := range scripts {
		out = append(out, k)
	}
	return out
}
