// Density bench: spawn N idle Next.js apps, measure per-app and
// total resident memory.
//
// Compares two scenarios:
//
//   - bare:    direct `bun run server.js` per app (lower bound — this is
//              what creekd would supervise; creekd itself adds no per-
//              app overhead, only a one-time supervisor RSS).
//   - docker:  `docker run -d` of a pre-built image holding the same
//              standalone. Each container brings its own containerd-shim
//              and engine accounting.
//
// Usage:
//
//	./up.sh
//	go run ./bench -n 10              # both scenarios, 10 apps each
//	go run ./bench -n 50 -scenario docker
//	./down.sh
//
// The bare scenario needs `bun` on PATH. The docker scenario needs the
// image `creekd-nextjs-density:bench` (built by ./up.sh).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	bareBasePort   = 19200
	dockerBasePort = 19300
)

func main() {
	n := flag.Int("n", 10, "number of apps per scenario")
	scenario := flag.String("scenario", "all", "which scenario to run: bare, docker, all")
	settleSec := flag.Int("settle", 5, "seconds to wait after all apps are healthy before sampling RSS")
	flag.Parse()

	exDir := findExampleDir()
	if exDir == "" {
		fmt.Fprintln(os.Stderr, "could not find examples/nextjs-density — run from inside that dir")
		os.Exit(1)
	}
	standalonePath := filepath.Join(exDir, "app", ".next", "standalone", "server.js")
	if _, err := os.Stat(standalonePath); err != nil {
		fmt.Fprintf(os.Stderr, "missing %s — run ./up.sh first\n", standalonePath)
		os.Exit(1)
	}

	fmt.Printf("density bench — N=%d apps per scenario, %ds settle, host %s/%s\n\n",
		*n, *settleSec, runtime.GOOS, runtime.GOARCH)

	var bareResult, dockerResult *Result

	if *scenario == "bare" || *scenario == "all" {
		if _, err := exec.LookPath("bun"); err != nil {
			fmt.Fprintln(os.Stderr, "bare scenario needs `bun` on PATH")
			os.Exit(1)
		}
		bareResult = runBare(standalonePath, *n, *settleSec)
		printResult("bare bun", bareResult)
	}

	if *scenario == "docker" || *scenario == "all" {
		if !imageExists("creekd-nextjs-density:bench") {
			fmt.Fprintln(os.Stderr, "docker scenario needs image creekd-nextjs-density:bench — run ./up.sh first")
			os.Exit(1)
		}
		dockerResult = runDocker(*n, *settleSec)
		printResult("docker run", dockerResult)
	}

	if bareResult != nil && dockerResult != nil {
		fmt.Println()
		fmt.Println("ratio (docker / bare):")
		fmt.Printf("  per-app RSS p50: %.2fx\n",
			float64(dockerResult.PerAppP50)/float64(bareResult.PerAppP50))
		fmt.Printf("  total RSS:       %.2fx\n",
			float64(dockerResult.TotalRSS)/float64(bareResult.TotalRSS))
	}
}

// Result is a per-scenario measurement bundle. All RSS values in KB
// (matching `ps axo rss` and `docker stats --format json` after
// conversion).
type Result struct {
	N            int
	SpawnAllMs   float64
	HealthyAllMs float64
	PerAppP50    int
	PerAppP95    int
	PerAppMin    int
	PerAppMax    int
	TotalRSS     int
}

func runBare(serverJS string, n, settleSec int) *Result {
	pids := make([]int, 0, n)
	defer func() {
		for _, pid := range pids {
			_ = exec.Command("kill", strconv.Itoa(pid)).Run()
		}
	}()

	t0 := time.Now()
	for i := 0; i < n; i++ {
		port := bareBasePort + i
		cmd := exec.Command("bun", "run", serverJS)
		cmd.Env = append(os.Environ(),
			"BENCH_BARE=1",
			fmt.Sprintf("PORT=%d", port),
		)
		// Detach stdio so a slow piped read can't block the process.
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "bare: bun start #%d: %v\n", i, err)
			continue
		}
		pids = append(pids, cmd.Process.Pid)
	}
	spawnMs := elapsedMs(t0)

	// Wait for every port to answer 200 on /healthz.
	t1 := time.Now()
	for i := 0; i < n; i++ {
		port := bareBasePort + i
		waitHealthz(fmt.Sprintf("http://127.0.0.1:%d/healthz", port), 30*time.Second)
	}
	healthyMs := elapsedMs(t1)

	time.Sleep(time.Duration(settleSec) * time.Second)

	rsss := make([]int, 0, n)
	for _, pid := range pids {
		if r := pidRSS(pid); r > 0 {
			rsss = append(rsss, r)
		}
	}
	return summarize(n, spawnMs, healthyMs, rsss)
}

func runDocker(n, settleSec int) *Result {
	ids := make([]string, 0, n)
	defer func() {
		for _, id := range ids {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}()

	// Pre-clean any bench-docker-* leftovers from a previous run (a
	// killed process leaves stopped containers whose names collide).
	if leftovers, err := exec.Command("docker", "ps", "-aq",
		"--filter", "name=^bench-docker-",
	).Output(); err == nil {
		for _, id := range strings.Fields(string(leftovers)) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}

	t0 := time.Now()
	for i := 0; i < n; i++ {
		port := dockerBasePort + i
		name := fmt.Sprintf("bench-docker-%d", i)
		out, err := exec.Command("docker", "run", "-d",
			"--name", name,
			"-p", fmt.Sprintf("%d:3000", port),
			"creekd-nextjs-density:bench",
		).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "docker: run #%d: %v\n%s\n", i, err, out)
			continue
		}
		ids = append(ids, strings.TrimSpace(string(out)))
	}
	spawnMs := elapsedMs(t0)

	t1 := time.Now()
	for i := 0; i < n; i++ {
		port := dockerBasePort + i
		waitHealthz(fmt.Sprintf("http://127.0.0.1:%d/healthz", port), 60*time.Second)
	}
	healthyMs := elapsedMs(t1)

	time.Sleep(time.Duration(settleSec) * time.Second)

	// `docker stats --no-stream` prints "MEM USAGE / LIMIT". The first
	// field is bytes-with-unit; parseHumanBytes turns "123MiB" → KB.
	rsss := make([]int, 0, n)
	out, err := exec.Command("docker", "stats", "--no-stream",
		"--format", "{{.Name}}\t{{.MemUsage}}",
	).Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "bench-docker-") {
				continue
			}
			fields := strings.SplitN(line, "\t", 2)
			if len(fields) < 2 {
				continue
			}
			// MemUsage is "123MiB / 7GiB" — take the first half.
			used := strings.SplitN(fields[1], "/", 2)[0]
			if kb := parseHumanBytes(strings.TrimSpace(used)); kb > 0 {
				rsss = append(rsss, kb)
			}
		}
	}
	return summarize(n, spawnMs, healthyMs, rsss)
}

func summarize(n int, spawnMs, healthyMs float64, rsss []int) *Result {
	if len(rsss) == 0 {
		return &Result{N: n, SpawnAllMs: spawnMs, HealthyAllMs: healthyMs}
	}
	sort.Ints(rsss)
	total := 0
	for _, r := range rsss {
		total += r
	}
	return &Result{
		N:            n,
		SpawnAllMs:   spawnMs,
		HealthyAllMs: healthyMs,
		PerAppP50:    rsss[len(rsss)*50/100],
		PerAppP95:    rsss[min(len(rsss)*95/100, len(rsss)-1)],
		PerAppMin:    rsss[0],
		PerAppMax:    rsss[len(rsss)-1],
		TotalRSS:     total,
	}
}

func printResult(label string, r *Result) {
	fmt.Printf("%-12s N=%d\n", label+":", r.N)
	fmt.Printf("  spawn all   : %7.0f ms\n", r.SpawnAllMs)
	fmt.Printf("  all healthy : %7.0f ms (after spawn loop)\n", r.HealthyAllMs)
	fmt.Printf("  per-app RSS : p50 %s   p95 %s   min %s   max %s\n",
		fmtKB(r.PerAppP50), fmtKB(r.PerAppP95),
		fmtKB(r.PerAppMin), fmtKB(r.PerAppMax))
	fmt.Printf("  total RSS   : %s\n\n", fmtKB(r.TotalRSS))
}

func waitHealthz(url string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func pidRSS(pid int) int {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return v
}

// parseHumanBytes understands the units docker stats emits ("MiB",
// "MB", "GiB", "KB"...). Returns KB (1024-based).
func parseHumanBytes(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var num string
	for i, r := range s {
		if (r >= '0' && r <= '9') || r == '.' {
			num = s[:i+1]
		} else {
			break
		}
	}
	if num == "" {
		return 0
	}
	val, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	unit := strings.ToLower(strings.TrimSpace(s[len(num):]))
	switch unit {
	case "b":
		return int(val / 1024)
	case "kb", "kib":
		return int(val)
	case "mb", "mib":
		return int(val * 1024)
	case "gb", "gib":
		return int(val * 1024 * 1024)
	}
	return 0
}

func fmtKB(kb int) string {
	if kb >= 1024*1024 {
		return fmt.Sprintf("%.2f GB", float64(kb)/(1024*1024))
	}
	if kb >= 1024 {
		return fmt.Sprintf("%.1f MB", float64(kb)/1024)
	}
	return fmt.Sprintf("%d KB", kb)
}

func elapsedMs(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}

func imageExists(ref string) bool {
	out, err := exec.Command("docker", "image", "inspect", ref).Output()
	if err != nil {
		return false
	}
	return len(out) > 0
}

// findExampleDir walks up from cwd until it sees app/.next next to
// itself (or returns cwd if the marker is already adjacent). Allows
// running via either `go run ./bench` or directly from bench/.
func findExampleDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "app", "next.config.ts")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
