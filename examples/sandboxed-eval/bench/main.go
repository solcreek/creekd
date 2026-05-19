// Bench tool that times creekd-sandboxed-spawn vs `docker run` for
// the same toy binary on the same Linux host.
//
// Designed to run inside ../bench/run.sh which:
//   - Builds an image with go + docker-cli + util-linux + cgroup tools
//   - Mounts /var/run/docker.sock so `docker run` calls land on the
//     host daemon
//   - Mounts the repo at /work, cgroup at /sys/fs/cgroup rw
//   - Runs --privileged + --cgroupns=host so creekd's chroot +
//     namespaces + cgroup all actually engage
//
// Direct invocation works too on a Linux host with creekd already
// up and docker available.
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
	creekDispatch = "http://127.0.0.1:9000"
)

func main() {
	n := flag.Int("n", 10, "samples per scenario")
	flag.Parse()

	exDir := findBinDir()
	if exDir == "" {
		fmt.Fprintln(os.Stderr, "could not find bin/creekctl — run ../up.sh first")
		os.Exit(1)
	}
	creekctl := filepath.Join(exDir, "bin", "creekctl")
	toy := filepath.Join(exDir, "bin", "toy")
	rootfs := filepath.Join(exDir, "rootfs")

	for _, bin := range []string{creekctl, toy} {
		if _, err := os.Stat(bin); err != nil {
			fmt.Fprintf(os.Stderr, "missing %s — run ../up.sh first\n", bin)
			os.Exit(1)
		}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintf(os.Stderr, "docker not on PATH — install docker-cli\n")
		os.Exit(1)
	}

	// The toy image is pre-built on the host by run.sh — we just
	// reference it here. Doing the build inside this process would
	// hit the socket-mounted host daemon with a build context that
	// the host can't see (mismatched paths between in-container
	// /tmp and host /tmp).
	imgTag := os.Getenv("BENCH_TOY_IMG")
	if imgTag == "" {
		imgTag = "creekd-sandbox-bench-toy:latest"
	}
	if out, err := exec.Command("docker", "image", "inspect", imgTag).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "toy image %q not found — run ./bench/run.sh which pre-builds it\n%s\n", imgTag, out)
		os.Exit(1)
	}

	fmt.Printf("\ncreekd sandbox vs docker run — spawn-to-healthz, %d samples each\n\n", *n)

	cs := benchCreekd(creekctl, rootfs, *n)
	ds := benchDocker(imgTag, *n)

	fmt.Println("spawn-to-healthz (ms):")
	printRow("creekd", cs)
	printRow("docker", ds)
	if len(cs) > 0 && len(ds) > 0 {
		fmt.Printf("  ratio:  docker / creekd = %.1fx\n", median(ds)/median(cs))
	}

	fmt.Printf("\nhost: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

func benchCreekd(creekctl, rootfs string, n int) []float64 {
	samples := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bench-sb-%d", i)
		port := 19700 + i
		t0 := time.Now()
		out, err := exec.Command(creekctl, "up", id,
			"--command", "/bin/toy",
			"--env", fmt.Sprintf("PORT=%d", port),
			"--port", strconv.Itoa(port),
			"--chroot", rootfs,
			"--pid-namespace", "--mount-namespace", "--uts-namespace",
			"--memory-max", "64M", "--pids-max", "32",
		).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "creekctl up: %v\n%s\n", err, out)
			continue
		}
		waitHealthz(creekDispatch+"/healthz", id)
		ms := float64(time.Since(t0).Microseconds()) / 1000.0
		samples = append(samples, ms)
		_ = exec.Command(creekctl, "rm", id).Run()
	}
	return samples
}

func benchDocker(imgTag string, n int) []float64 {
	samples := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("bench-sb-d-%d", i)
		port := 19800 + i
		t0 := time.Now()
		cmd := exec.Command("docker", "run", "--rm", "-d",
			"--name", name,
			"--memory=64m",
			"--pids-limit=32",
			"--security-opt=no-new-privileges",
			"-p", fmt.Sprintf("%d:%d", port, port),
			"-e", fmt.Sprintf("PORT=%d", port),
			imgTag,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "docker run: %v\n%s\n", err, out)
			continue
		}
		waitHealthz(fmt.Sprintf("http://127.0.0.1:%d/healthz", port), "")
		ms := float64(time.Since(t0).Microseconds()) / 1000.0
		samples = append(samples, ms)
		_ = exec.Command("docker", "stop", name).Run()
	}
	return samples
}


func waitHealthz(url, hostHeader string) {
	deadline := time.Now().Add(10 * time.Second)
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
		time.Sleep(5 * time.Millisecond)
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

// keep strings import used (was a tabular formatter idea, left for future)
var _ = strings.Fields
