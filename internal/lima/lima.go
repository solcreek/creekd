package lima

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

const DefaultVMName = "creek-sandbox"

type VM struct {
	Name string
	Log  *slog.Logger

	// execCommand is overridable for testing.
	execCommand func(name string, arg ...string) *exec.Cmd
}

func NewVM(name string, log *slog.Logger) *VM {
	return &VM{
		Name:        name,
		Log:         log,
		execCommand: exec.Command,
	}
}

func Available() bool {
	_, err := exec.LookPath("limactl")
	return err == nil
}

func Version() (string, error) {
	out, err := exec.Command("limactl", "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("lima: version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

type limaInstance struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (vm *VM) list() ([]limaInstance, error) {
	cmd := vm.execCommand("limactl", "list", "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("lima: list: %w", err)
	}
	var instances []limaInstance
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var inst limaInstance
		if err := json.Unmarshal(line, &inst); err != nil {
			continue
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

func (vm *VM) find() (*limaInstance, error) {
	instances, err := vm.list()
	if err != nil {
		return nil, err
	}
	for _, inst := range instances {
		if inst.Name == vm.Name {
			return &inst, nil
		}
	}
	return nil, nil
}

func (vm *VM) Exists() (bool, error) {
	inst, err := vm.find()
	if err != nil {
		return false, err
	}
	return inst != nil, nil
}

func (vm *VM) Running() (bool, error) {
	inst, err := vm.find()
	if err != nil {
		return false, err
	}
	return inst != nil && inst.Status == "Running", nil
}

func (vm *VM) Create(yamlPath string) error {
	vm.Log.Info("creating sandbox VM", "name", vm.Name)
	cmd := vm.execCommand("limactl", "start", "--name", vm.Name, "--tty=false", yamlPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lima: create %s: %w", vm.Name, err)
	}
	return nil
}

func (vm *VM) Start() error {
	vm.Log.Info("starting sandbox VM", "name", vm.Name)
	cmd := vm.execCommand("limactl", "start", vm.Name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lima: start %s: %w", vm.Name, err)
	}
	return nil
}

func (vm *VM) Stop() error {
	vm.Log.Info("stopping sandbox VM", "name", vm.Name)
	cmd := vm.execCommand("limactl", "stop", vm.Name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lima: stop %s: %w", vm.Name, err)
	}
	return nil
}

func (vm *VM) Destroy() error {
	vm.Log.Info("destroying sandbox VM", "name", vm.Name)
	_ = vm.execCommand("limactl", "stop", vm.Name).Run()
	cmd := vm.execCommand("limactl", "delete", vm.Name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lima: delete %s: %w", vm.Name, err)
	}
	return nil
}

func (vm *VM) Shell(command string) (string, error) {
	cmd := vm.execCommand("limactl", "shell", vm.Name, "bash", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("lima: shell: %w: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (vm *VM) ShellRoot(command string) (string, error) {
	cmd := vm.execCommand("limactl", "shell", vm.Name, "sudo", "bash", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("lima: shell (root): %w: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
