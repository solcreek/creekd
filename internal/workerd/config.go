// Package workerd generates Cap'n Proto configuration for the
// workerd runtime from Creek's internal supervisor.Config.
package workerd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/solcreek/creekd/internal/supervisor"
)

// GenerateConfig writes a workerd capnp configuration file for the
// given app config. Returns the path to the generated file.
func GenerateConfig(cfg supervisor.Config, entryPath string, socketPort int, outDir string) (string, error) {
	if cfg.ID == "" {
		return "", fmt.Errorf("workerd: empty app ID")
	}
	if entryPath == "" {
		return "", fmt.Errorf("workerd: empty entry path")
	}

	data := templateData{
		AppName:    cfg.ID,
		EntryPath:  entryPath,
		SocketPort: socketPort,
		Bindings:   buildBindings(cfg),
		CompatDate: "2024-01-01",
	}

	outPath := filepath.Join(outDir, cfg.ID+".capnp")
	f, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("workerd: create config: %w", err)
	}
	defer f.Close()

	if err := configTmpl.Execute(f, data); err != nil {
		os.Remove(outPath)
		return "", fmt.Errorf("workerd: render config: %w", err)
	}
	return outPath, nil
}

type templateData struct {
	AppName    string
	EntryPath  string
	SocketPort int
	Bindings   []binding
	CompatDate string
}

type binding struct {
	Name  string
	Type  string
	Value string
}

func buildBindings(cfg supervisor.Config) []binding {
	var bindings []binding
	for _, kv := range cfg.Env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			bindings = append(bindings, binding{Name: parts[0], Type: "text", Value: parts[1]})
		}
	}
	bindings = append(bindings, binding{Name: "PORT", Type: "text", Value: fmt.Sprintf("%d", cfg.Port)})
	return bindings
}

var configTmpl = template.Must(template.New("capnp").Parse(`using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    ( name = "{{.AppName}}",
      worker = (
        modules = [
          ( name = "worker", esModule = embed "{{.EntryPath}}" )
        ],
        compatibilityDate = "{{.CompatDate}}",
{{- if .Bindings}}
        bindings = [
{{- range .Bindings}}
{{- if eq .Type "text"}}
          ( name = "{{.Name}}", text = "{{.Value}}" ),
{{- else if eq .Type "service"}}
          ( name = "{{.Name}}", service = "{{.Value}}" ),
{{- end}}
{{- end}}
        ],
{{- end}}
      ),
    ),
  ],

  sockets = [
    ( name = "http",
      address = "*:{{.SocketPort}}",
      http = (),
      service = "{{.AppName}}",
    ),
  ],
);
`))
