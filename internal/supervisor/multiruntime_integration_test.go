package supervisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	runtimePkg "github.com/solcreek/creekd/internal/runtime"
)

// runtimeFixture describes the per-runtime HTTP test server we ship to
// the child process. signature is whatever /health returns when the
// app is healthy — the test asserts the supervisor saw an app from the
// expected runtime, not just any 200.
type runtimeFixture struct {
	rt        runtimePkg.Runtime
	entry     string
	source    string
	signature string
}

var multiRuntimeFixtures = []runtimeFixture{
	{
		rt:        runtimePkg.Node,
		entry:     "server.js",
		signature: "node-ok",
		source: `const http = require('http');
const port = process.env.PORT;
http.createServer((req, res) => {
  if (req.url === '/health') {
    res.writeHead(200, {'Content-Type': 'text/plain'});
    res.end('node-ok\n');
    return;
  }
  res.writeHead(404);
  res.end();
}).listen(port);
`,
	},
	{
		rt:        runtimePkg.Bun,
		entry:     "server.ts",
		signature: "bun-ok",
		source: `const port = Number(process.env.PORT);
Bun.serve({
  port,
  fetch(req) {
    const url = new URL(req.url);
    if (url.pathname === '/health') {
      return new Response('bun-ok\n', { status: 200 });
    }
    return new Response(null, { status: 404 });
  },
});
`,
	},
	{
		rt:        runtimePkg.Deno,
		entry:     "server.ts",
		signature: "deno-ok",
		source: `const port = Number(Deno.env.get('PORT'));
Deno.serve({ port }, (req) => {
  const url = new URL(req.url);
  if (url.pathname === '/health') {
    return new Response('deno-ok\n', { status: 200 });
  }
  return new Response(null, { status: 404 });
});
`,
	},
}

// writeFixture materialises the runtime's source file into its own
// directory under root and returns the absolute path.
func writeFixture(t *testing.T, root string, f runtimeFixture) string {
	t.Helper()
	dir := filepath.Join(root, string(f.rt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, f.entry)
	if err := os.WriteFile(path, []byte(f.source), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// fetchSignature does GET http://127.0.0.1:<port>/health and returns
// the trimmed response body.
func fetchSignature(t *testing.T, port int) string {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// TestMultiRuntimeConcurrentDispatch is the M5.4 acceptance test:
// one Supervisor hosts a Node, Bun, and Deno server concurrently
// (skipping any whose binary is not in PATH on this machine). Each
// child resolves through runtime.Command, gets a distinct port, and
// answers /health with a runtime-specific signature. The supervisor's
// own HTTPHealthChecker probes every one of them and none restart.
func TestMultiRuntimeConcurrentDispatch(t *testing.T) {
	root := t.TempDir()

	// Which runtimes are usable on this machine?
	type live struct {
		fx   runtimeFixture
		path string
		port int
	}
	var actives []live
	for _, fx := range multiRuntimeFixtures {
		if _, err := exec.LookPath(string(fx.rt)); err != nil {
			t.Logf("skipping %s: not in PATH", fx.rt)
			continue
		}
		actives = append(actives, live{fx: fx, path: writeFixture(t, root, fx)})
	}
	if len(actives) < 2 {
		t.Skipf("need ≥2 runtimes in PATH to prove concurrent isolation; have %d", len(actives))
	}

	sup := newTestSupervisor()
	sup.HealthChecker = &HTTPHealthChecker{}
	sup.HealthCheckInterval = 200 * time.Millisecond
	sup.HealthCheckTimeout = 500 * time.Millisecond
	sup.HealthCheckFailureThreshold = 3
	// Allow real-runtime startup time before health probes can mature.
	sup.InitialBackoff = 50 * time.Millisecond
	sup.MaxBackoff = 100 * time.Millisecond
	sup.CrashLoopThreshold = 100

	apps := make(map[runtimePkg.Runtime]*App, len(actives))
	for i := range actives {
		actives[i].port = freePort(t)
		fx := actives[i].fx
		app, err := sup.Spawn(Config{
			ID:      "mr-" + string(fx.rt),
			Runtime: fx.rt,
			Entry:   actives[i].path,
			Port:    actives[i].port,
		})
		if err != nil {
			t.Fatalf("Spawn %s failed: %v", fx.rt, err)
		}
		t.Cleanup(func() { _ = sup.Stop(app.ID) })
		apps[fx.rt] = app
	}

	// Wait for every server to bind and respond. Real-runtime startup
	// can be slow on first JIT pass, especially with the race detector
	// loaded and multiple children spawning at once — be generous.
	for _, a := range actives {
		waitForHTTPReady(t, a.port, 15*time.Second)
	}

	// Capture original PIDs to detect any unwanted restart later.
	originalPIDs := make(map[runtimePkg.Runtime]int, len(actives))
	for _, a := range actives {
		originalPIDs[a.fx.rt] = apps[a.fx.rt].PID()
	}

	// Each /health must return its runtime-specific signature.
	for _, a := range actives {
		body := fetchSignature(t, a.port)
		if want := a.fx.signature; !contains(body, want) {
			t.Errorf("%s /health body = %q, want substring %q", a.fx.rt, body, want)
		}
	}

	// Verify cross-runtime isolation: stop one, others remain unaffected.
	if len(actives) >= 2 {
		victim := actives[0].fx.rt
		if err := sup.Stop(apps[victim].ID); err != nil {
			t.Fatalf("Stop %s: %v", victim, err)
		}
		for _, a := range actives[1:] {
			pid := apps[a.fx.rt].PID()
			if pid != originalPIDs[a.fx.rt] {
				t.Errorf("%s PID changed (%d → %d) after stopping %s — isolation broken",
					a.fx.rt, originalPIDs[a.fx.rt], pid, victim)
			}
			body := fetchSignature(t, a.port)
			if !contains(body, a.fx.signature) {
				t.Errorf("%s no longer healthy after stopping %s: body=%q",
					a.fx.rt, victim, body)
			}
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
