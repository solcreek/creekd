// Traffic-density bench: measure per-app PSS at five phases of
// traffic — idle, warm, sustained, burst, cooldown — across the
// five stacks already bootstrapped by ../stack-density and
// ../nextjs-density.
//
// Idle PSS is the floor (what the stack-density bench reports).
// Sustained / burst PSS sets the realistic and worst-case ceiling
// for capacity-planning math (see product-planning/density-economics.md
// in the private docs repo). The inflation ratio between idle and
// burst is the headline number this bench produces.
//
// Linux-only — needs /proc/<pid>/smaps_rollup for PSS.
//
// Usage:
//
//	../stack-density/bootstrap.sh      # one-time
//	../nextjs-density/up.sh            # one-time
//	./bootstrap.sh                     # idempotent check
//	go run ./bench                     # default: all 5 stacks, N=5 apps each
//	N=10 go run ./bench                # higher density per stack
//	STACK=hono go run ./bench          # one stack only
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Phase ordering matters: bench applies them sequentially per stack,
// sampling PSS at the end of each.
type Phase string

const (
	PhaseIdle      Phase = "idle"
	PhaseWarm      Phase = "warm"      // after 100 hits to /
	PhaseSustained Phase = "sustained" // 1 rps × duration
	PhaseBurst     Phase = "burst"     // 100 rps × duration
	PhaseCooldown  Phase = "cooldown"  // after 60s with no traffic
)

var allPhases = []Phase{PhaseIdle, PhaseWarm, PhaseSustained, PhaseBurst, PhaseCooldown}

// Stack defines one fixture: a JS file the bench launches via `bun
// run <entry>`. Port is assigned dynamically per N (basePort + i).
type Stack struct {
	Name      string
	Entry     string // absolute path to server.js entrypoint
	Runtime   string // "bun" — only one used today
	BasePort  int
	WarmReqs  int           // # of warm-up requests to /
	BurstRPS  int           // rps for burst phase
	BurstDur  time.Duration // burst duration
	LowRPS    int           // rps for sustained phase
	LowDur    time.Duration // sustained duration
	CooldownT time.Duration // cooldown wait
}

func main() {
	n := flag.Int("n", 5, "apps per stack")
	stackFilter := flag.String("stack", "", "run only one stack by name (default: all five)")
	flag.Parse()

	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "traffic-density bench is Linux-only (needs /proc/<pid>/smaps_rollup).")
		fmt.Fprintln(os.Stderr, "On macOS, ssh into a Linux host and run there.")
		os.Exit(2)
	}

	stacks := discoverStacks()
	if *stackFilter != "" {
		stacks = filterStacks(stacks, *stackFilter)
		if len(stacks) == 0 {
			fmt.Fprintf(os.Stderr, "no stack matches %q\n", *stackFilter)
			os.Exit(2)
		}
	}

	fmt.Printf("traffic-density bench — N=%d per stack, %s/%s\n",
		*n, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("phases: %s\n\n", joinPhases(allPhases))

	for _, st := range stacks {
		runStack(st, *n)
	}
}

// discoverStacks resolves the five known fixture entrypoints,
// skipping any that aren't built yet (bootstrap.sh would have built
// them; if it didn't, we surface that as a per-stack skip).
func discoverStacks() []Stack {
	exDir := findExamplesDir()
	if exDir == "" {
		fmt.Fprintln(os.Stderr, "could not locate creekd/examples/ — run from inside the repo")
		os.Exit(2)
	}
	defaults := struct {
		WarmReqs  int
		BurstRPS  int
		BurstDur  time.Duration
		LowRPS    int
		LowDur    time.Duration
		CooldownT time.Duration
	}{
		WarmReqs:  100,
		BurstRPS:  100,
		BurstDur:  60 * time.Second,
		LowRPS:    1,
		LowDur:    60 * time.Second,
		CooldownT: 60 * time.Second,
	}
	candidates := []struct {
		Name     string
		Entry    string
		BasePort int
	}{
		{"bun-hello", filepath.Join(exDir, "stack-density", "stacks", "bun-hello", "server.js"), 21000},
		{"hono", filepath.Join(exDir, "stack-density", "stacks", "hono", "server.js"), 21100},
		{"sveltekit", filepath.Join(exDir, "stack-density", "stacks", "sveltekit", "build", "index.js"), 21200},
		{"astro", filepath.Join(exDir, "stack-density", "stacks", "astro", "dist", "server", "entry.mjs"), 21300},
		{"nextjs", filepath.Join(exDir, "nextjs-density", "app", ".next", "standalone", "server.js"), 21400},
	}
	out := []Stack{}
	for _, c := range candidates {
		if _, err := os.Stat(c.Entry); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s (no fixture at %s)\n", c.Name, c.Entry)
			continue
		}
		out = append(out, Stack{
			Name:      c.Name,
			Entry:     c.Entry,
			Runtime:   "bun",
			BasePort:  c.BasePort,
			WarmReqs:  defaults.WarmReqs,
			BurstRPS:  defaults.BurstRPS,
			BurstDur:  defaults.BurstDur,
			LowRPS:    defaults.LowRPS,
			LowDur:    defaults.LowDur,
			CooldownT: defaults.CooldownT,
		})
	}
	return out
}

func filterStacks(stacks []Stack, name string) []Stack {
	out := []Stack{}
	for _, s := range stacks {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// Sample is one PSS reading bundle taken at the end of a phase.
type Sample struct {
	Phase            Phase
	PerAppPssP50KB   int
	PerAppPssP95KB   int
	TotalPssKB       int
	MemAvailDeltaKB  int
	HTTPLatP50Ms     float64 // 0 when no traffic phase
	HTTPLatP99Ms     float64
	RequestsFinished int64
}

func runStack(st Stack, n int) {
	fmt.Printf("=== stack=%s entry=%s ===\n", st.Name, filepath.Base(filepath.Dir(st.Entry))+"/"+filepath.Base(st.Entry))

	pids, ports, cleanup := spawnApps(st, n)
	defer cleanup()

	memAvailStart := memAvailableKB()

	var samples []Sample

	// PhaseIdle — already there. Settle 5s for any post-spawn JIT.
	time.Sleep(5 * time.Second)
	samples = append(samples, samplePhase(PhaseIdle, pids, memAvailStart, nil))

	// PhaseWarm — N warm-up hits per app, no rate limiting.
	warm := generateLoad(ports, st.WarmReqs, 0, 0) // count-bounded, max rate
	samples = append(samples, samplePhase(PhaseWarm, pids, memAvailStart, &warm))

	// PhaseSustained — low rps for duration.
	sustained := generateLoad(ports, 0, st.LowRPS, st.LowDur)
	samples = append(samples, samplePhase(PhaseSustained, pids, memAvailStart, &sustained))

	// PhaseBurst — high rps for duration.
	burst := generateLoad(ports, 0, st.BurstRPS, st.BurstDur)
	samples = append(samples, samplePhase(PhaseBurst, pids, memAvailStart, &burst))

	// PhaseCooldown — wait, then sample again to see how much the
	// kernel reclaims after traffic stops. Critical for review-app
	// use case.
	time.Sleep(st.CooldownT)
	samples = append(samples, samplePhase(PhaseCooldown, pids, memAvailStart, nil))

	printStackTable(st, samples)
	fmt.Println()
}

func printStackTable(st Stack, samples []Sample) {
	fmt.Printf("  %-10s  %-12s  %-12s  %-14s  %-14s  %s\n",
		"phase", "PSS p50", "PSS p95", "Total PSS", "MemAvail Δ", "HTTP lat p50/p99 (req)")
	for _, s := range samples {
		latStr := "—"
		if s.RequestsFinished > 0 {
			latStr = fmt.Sprintf("%.1f / %.1f ms (%d)", s.HTTPLatP50Ms, s.HTTPLatP99Ms, s.RequestsFinished)
		}
		fmt.Printf("  %-10s  %-12s  %-12s  %-14s  %-14s  %s\n",
			s.Phase,
			fmtKB(s.PerAppPssP50KB),
			fmtKB(s.PerAppPssP95KB),
			fmtKB(s.TotalPssKB),
			"-"+fmtKB(s.MemAvailDeltaKB),
			latStr)
	}

	// Inflation summary: idle → burst, idle → cooldown.
	idle := findPhase(samples, PhaseIdle)
	burst := findPhase(samples, PhaseBurst)
	cool := findPhase(samples, PhaseCooldown)
	if idle != nil && burst != nil && idle.PerAppPssP50KB > 0 {
		fmt.Printf("  → burst inflation:    %.2f× per-app PSS, %.2f× system MemAvail Δ\n",
			float64(burst.PerAppPssP50KB)/float64(idle.PerAppPssP50KB),
			float64(burst.MemAvailDeltaKB)/float64(maxInt(idle.MemAvailDeltaKB, 1)))
	}
	if idle != nil && cool != nil && idle.PerAppPssP50KB > 0 {
		fmt.Printf("  → cooldown reclaim:   %.2f× idle PSS (1.0× = full reclaim, >1.0 = retained)\n",
			float64(cool.PerAppPssP50KB)/float64(idle.PerAppPssP50KB))
	}
}

func findPhase(samples []Sample, p Phase) *Sample {
	for i := range samples {
		if samples[i].Phase == p {
			return &samples[i]
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// joinPhases is just strings.Join over the typed slice. Kept for
// readability at the call site.
func joinPhases(phases []Phase) string {
	parts := make([]string, len(phases))
	for i, p := range phases {
		parts[i] = string(p)
	}
	return strings.Join(parts, " → ")
}

// fmtKB renders KB as MB/GB with one decimal, matching the convention
// in the other density benches.
func fmtKB(kb int) string {
	if kb >= 1024*1024 {
		return fmt.Sprintf("%.2f GB", float64(kb)/(1024*1024))
	}
	if kb >= 1024 {
		return fmt.Sprintf("%.1f MB", float64(kb)/1024)
	}
	return fmt.Sprintf("%d KB", kb)
}

// findExamplesDir walks up looking for the examples/ dir that
// contains nextjs-density and stack-density.
func findExamplesDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 6; i++ {
		if fi, err := os.Stat(filepath.Join(dir, "stack-density")); err == nil && fi.IsDir() {
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

// --- the following are stubs to be filled in Commit B ------------

func spawnApps(st Stack, n int) (pids []int, ports []int, cleanup func()) {
	pids = make([]int, 0, n)
	ports = make([]int, 0, n)
	cmds := make([]*exec.Cmd, 0, n)
	for i := 0; i < n; i++ {
		port := st.BasePort + i
		cmd := exec.Command(st.Runtime, "run", st.Entry)
		cmd.Env = append(os.Environ(),
			fmt.Sprintf("PORT=%d", port),
			"HOST=127.0.0.1",
		)
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "spawn %s #%d: %v\n", st.Name, i, err)
			continue
		}
		pids = append(pids, cmd.Process.Pid)
		ports = append(ports, port)
		cmds = append(cmds, cmd)
	}
	// Wait for each to become reachable on /.
	for _, p := range ports {
		waitReachable(fmt.Sprintf("http://127.0.0.1:%d/", p), 30*time.Second)
	}
	cleanup = func() {
		for _, c := range cmds {
			if c.Process != nil {
				_ = c.Process.Kill()
			}
		}
	}
	return pids, ports, cleanup
}

func waitReachable(url string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// loadResult bundles request count + latency samples from a load
// phase, used to fill the HTTP latency columns at sample time.
type loadResult struct {
	Count       int64
	LatenciesMs []float64
}

// generateLoad emits requests across all ports. If maxRequests > 0
// the call returns after that many TOTAL requests are issued. If
// rps + duration are non-zero, it runs for `duration` at `rps`
// requests per second TOTAL (spread across all ports). Latencies
// of completed requests are recorded for percentile reporting.
func generateLoad(ports []int, maxRequests, rps int, duration time.Duration) loadResult {
	var count int64
	var mu sync.Mutex
	lats := make([]float64, 0, 4096)
	client := &http.Client{Timeout: 5 * time.Second}

	issue := func(port int) {
		start := time.Now()
		req, _ := http.NewRequestWithContext(context.Background(), "GET",
			fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
		resp, err := client.Do(req)
		ms := float64(time.Since(start).Microseconds()) / 1000.0
		if err == nil {
			// Drain so keep-alive can re-use the connection.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			mu.Lock()
			lats = append(lats, ms)
			mu.Unlock()
		}
		atomic.AddInt64(&count, 1)
	}

	if maxRequests > 0 {
		// Round-robin maxRequests across ports, concurrent.
		var wg sync.WaitGroup
		sem := make(chan struct{}, 20)
		for i := 0; i < maxRequests; i++ {
			port := ports[i%len(ports)]
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				issue(port)
			}()
		}
		wg.Wait()
	} else if rps > 0 && duration > 0 {
		// Open-loop generator. Tick at 1/rps, dispatch each request
		// to one of the ports (round robin).
		interval := time.Second / time.Duration(rps)
		if interval < time.Millisecond {
			interval = time.Millisecond
		}
		deadline := time.Now().Add(duration)
		i := 0
		var wg sync.WaitGroup
		for time.Now().Before(deadline) {
			port := ports[i%len(ports)]
			i++
			wg.Add(1)
			go func() {
				defer wg.Done()
				issue(port)
			}()
			time.Sleep(interval)
		}
		wg.Wait()
	}

	return loadResult{Count: atomic.LoadInt64(&count), LatenciesMs: lats}
}

// samplePhase reads PSS for every pid right now and packages the
// numbers (plus optional load result) into a Sample. memAvailStart
// is the baseline so we report the delta consumed by the phase.
func samplePhase(phase Phase, pids []int, memAvailStart int, load *loadResult) Sample {
	psss := make([]int, 0, len(pids))
	for _, pid := range pids {
		if p := pidPSS(pid); p > 0 {
			psss = append(psss, p)
		}
	}
	sort.Ints(psss)
	total := 0
	for _, p := range psss {
		total += p
	}
	s := Sample{Phase: phase, TotalPssKB: total}
	if len(psss) > 0 {
		s.PerAppPssP50KB = psss[len(psss)*50/100]
		idx := len(psss) * 95 / 100
		if idx >= len(psss) {
			idx = len(psss) - 1
		}
		s.PerAppPssP95KB = psss[idx]
	}
	if memAvailStart > 0 {
		if after := memAvailableKB(); after > 0 {
			s.MemAvailDeltaKB = memAvailStart - after
		}
	}
	if load != nil {
		s.RequestsFinished = load.Count
		if len(load.LatenciesMs) > 0 {
			sorted := append([]float64(nil), load.LatenciesMs...)
			sort.Float64s(sorted)
			s.HTTPLatP50Ms = sorted[len(sorted)*50/100]
			idx := len(sorted) * 99 / 100
			if idx >= len(sorted) {
				idx = len(sorted) - 1
			}
			s.HTTPLatP99Ms = sorted[idx]
		}
	}
	return s
}

func pidPSS(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Pss:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		return v
	}
	return 0
}

func memAvailableKB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		return v
	}
	return 0
}
