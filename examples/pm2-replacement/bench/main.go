// Bench tool that times spawn-to-healthz and measures supervisor RSS
// for creekd vs pm2 on the same toy app.
//
// Usage:
//
//	go run ./bench -n 20         # 20 samples per supervisor
//
// Assumes:
//   - creekd is running on 127.0.0.1:9080 (admin) + 127.0.0.1:9000 (dispatch).
//   - pm2 is installed on PATH and started its daemon (any prior pm2 cmd does this).
//   - ./bin/toy and ./bin/creekctl are built (./up.sh handles this).
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

func main() {
	n := flag.Int("n", 20, "samples per scenario")
	flag.Parse()

	// Walk up from cwd until we find a sibling `bin/creekctl`. This
	// keeps the tool runnable from either ./bench or ../ — and from
	// `go run ./bench` (which leaves cwd at the example root).
	exDir := findBinDir()
	if exDir == "" {
		fmt.Fprintln(os.Stderr, "could not find bin/creekctl — run ../up.sh first")
		os.Exit(1)
	}
	creekctl := filepath.Join(exDir, "bin", "creekctl")
	toy := filepath.Join(exDir, "bin", "toy")

	for _, bin := range []string{creekctl, toy} {
		if _, err := os.Stat(bin); err != nil {
			fmt.Fprintf(os.Stderr, "missing %s — run ../up.sh first\n", bin)
			os.Exit(1)
		}
	}
	if _, err := exec.LookPath("pm2"); err != nil {
		fmt.Fprintf(os.Stderr, "pm2 not on PATH — `npm install -g pm2` first\n")
		os.Exit(1)
	}

	fmt.Printf("creekd vs pm2 — spawn-to-healthz, %d samples each\n\n", *n)

	cs := benchCreekd(creekctl, toy, *n)
	ps := benchPM2(toy, *n)

	fmt.Println("spawn-to-healthz (ms):")
	printRow("creekd", cs)
	printRow("pm2", ps)
	fmt.Printf("  ratio:  pm2 / creekd = %.1fx\n", median(ps)/median(cs))

	fmt.Println()
	fmt.Println("supervisor resident memory (KB):")
	cRSS := supervisorRSS("creekd")
	pRSS := supervisorRSS("PM2 v")
	fmt.Printf("  creekd: %d KB\n", cRSS)
	fmt.Printf("  pm2:    %d KB\n", pRSS)
	if cRSS > 0 && pRSS > 0 {
		fmt.Printf("  ratio:  pm2 / creekd = %.1fx\n", float64(pRSS)/float64(cRSS))
	}
	fmt.Printf("\nhost: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

func benchCreekd(creekctl, toy string, n int) []float64 {
	samples := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bench-c-%d", i)
		port := 19600 + i
		t0 := time.Now()
		out, err := exec.Command(creekctl, "up", id,
			"--command", toy,
			"--env", "APP_NAME="+id,
			"--env", fmt.Sprintf("PORT=%d", port),
			"--port", strconv.Itoa(port),
		).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "creekctl up: %v\n%s\n", err, out)
			continue
		}
		waitHealthz(fmt.Sprintf("http://127.0.0.1:9000/healthz"), id)
		ms := float64(time.Since(t0).Microseconds()) / 1000.0
		samples = append(samples, ms)
		_ = exec.Command(creekctl, "rm", id).Run()
	}
	return samples
}

func benchPM2(toy string, n int) []float64 {
	samples := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bench-p-%d", i)
		port := 19500 + i
		t0 := time.Now()
		cmd := exec.Command("pm2", "start", toy,
			"--name", id,
			"--no-autorestart",
			"--silent",
		)
		cmd.Env = append(os.Environ(),
			"APP_NAME="+id,
			fmt.Sprintf("PORT=%d", port),
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pm2 start: %v\n%s\n", err, out)
			continue
		}
		waitHealthz(fmt.Sprintf("http://127.0.0.1:%d/healthz", port), "")
		ms := float64(time.Since(t0).Microseconds()) / 1000.0
		samples = append(samples, ms)
		_ = exec.Command("pm2", "delete", id, "--silent").Run()
	}
	return samples
}

func waitHealthz(url, hostHeader string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
		if hostHeader != "" {
			req.Header.Set("X-Creek-App", hostHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func printRow(name string, s []float64) {
	if len(s) == 0 {
		fmt.Printf("  %-8s (no samples)\n", name)
		return
	}
	fmt.Printf("  %-8s p50 %6.1f   p95 %6.1f   min %6.1f   max %6.1f   n=%d\n",
		name, percentile(s, 50), percentile(s, 95), minOf(s), maxOf(s), len(s))
}

func percentile(s []float64, p int) float64 {
	if len(s) == 0 {
		return 0
	}
	sorted := append([]float64(nil), s...)
	sort.Float64s(sorted)
	idx := (len(sorted) - 1) * p / 100
	return sorted[idx]
}

func median(s []float64) float64 { return percentile(s, 50) }

func minOf(s []float64) float64 {
	m := s[0]
	for _, v := range s[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxOf(s []float64) float64 {
	m := s[0]
	for _, v := range s[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

// supervisorRSS finds the RSS (KB) of the process whose argv contains
// the given marker. For pm2 the daemon is called "PM2 v..."; for
// creekd it's just "creekd". Returns 0 if not found.
func supervisorRSS(marker string) int {
	out, err := exec.Command("ps", "axo", "pid,rss,command").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 {
			continue
		}
		rss, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		// First hit wins. We don't try to exclude grep/this binary
		// itself; the bench tool doesn't match either marker.
		return rss
	}
	return 0
}

// findBinDir walks up from cwd looking for a sibling `bin/creekctl`.
// Returns "" if nothing matches within five levels.
func findBinDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "bin", "creekctl")); err == nil {
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
