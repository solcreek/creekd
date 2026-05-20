package supervisor

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Supervisor benchmarks intentionally spawn real `sleep` processes
// — the lifecycle they measure is dominated by fork/exec/wait
// syscalls, so mocking them out would defeat the purpose. The
// `sleep` argument is long enough that no benchmarked process will
// exit on its own; cleanup is explicit via sup.Stop.
//
// Run individually:
//   go test -bench=BenchmarkSpawnStopSerial -benchmem -run=^$ ./internal/supervisor/
// Or every supervisor bench:
//   go test -bench=Benchmark -benchmem -benchtime=3s -run=^$ ./internal/supervisor/

// BenchmarkSpawnStopSerial is the canonical lifecycle benchmark: how
// long does it take to bring an app up and tear it down end-to-end?
// fork/exec dominates so improvements to in-process code paths
// (registry mutation, watch goroutine startup) won't move this much,
// but a regression that adds another syscall or lock will.
func BenchmarkSpawnStopSerial(b *testing.B) {
	sup := newTestSupervisor()
	// Fast restart in case any test logic introduces one.
	sup.InitialBackoff = 5 * time.Millisecond
	sup.MaxBackoff = 10 * time.Millisecond

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("bs%d", i)
		app, err := sup.Spawn(Config{
			ID: id, Command: "sleep", Args: []string{"30"}, Port: 30000 + i,
		})
		if err != nil {
			b.Fatalf("Spawn: %v", err)
		}
		if err := sup.Stop(app.ID); err != nil {
			b.Fatalf("Stop: %v", err)
		}
	}
}

// BenchmarkSpawnOnly isolates the Spawn cost. Apps stay alive until
// the post-loop cleanup; ns/op here is the steady-state per-app
// onboarding cost, useful when measuring the "100 apps coming up at
// startup" scenario.
func BenchmarkSpawnOnly(b *testing.B) {
	sup := newTestSupervisor()
	ids := make([]string, b.N)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("bo%d", i)
		ids[i] = id
		if _, err := sup.Spawn(Config{
			ID: id, Command: "sleep", Args: []string{"30"}, Port: 31000 + i,
		}); err != nil {
			b.Fatalf("Spawn: %v", err)
		}
	}
	b.StopTimer()
	for _, id := range ids {
		_ = sup.Stop(id)
	}
}

// BenchmarkRestart measures the in-place process cycle: SIGTERM the
// current process and wait for the watch goroutine's restart logic
// to land a new PID. This is the cost the operator pays on
// `creekctl restart`.
func BenchmarkRestart(b *testing.B) {
	sup := newTestSupervisor()
	sup.InitialBackoff = 5 * time.Millisecond
	sup.MaxBackoff = 10 * time.Millisecond
	// High threshold so the bench's many restarts don't trip
	// crash-loop suspension.
	sup.CrashLoopThreshold = 1_000_000
	sup.RestartWindow = time.Hour

	app, err := sup.Spawn(Config{
		ID: "br", Command: "sleep", Args: []string{"30"}, Port: 32000,
	})
	if err != nil {
		b.Fatalf("Spawn: %v", err)
	}
	b.Cleanup(func() { _ = sup.Stop("br") })

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sup.Restart(app.ID, 5*time.Second); err != nil {
			b.Fatalf("Restart: %v", err)
		}
	}
}

// BenchmarkConcurrentSpawn10 stresses the registry mutex with
// concurrent Spawn calls — the "10 deploys land at the same time"
// scenario.
func BenchmarkConcurrentSpawn10(b *testing.B) {
	benchConcurrentSpawn(b, 10, 33000)
}

// BenchmarkConcurrentSpawn100 — same idea, 10x the parallelism.
// Useful for catching contention regressions but slow to run, so
// skip in -short mode.
func BenchmarkConcurrentSpawn100(b *testing.B) {
	if testing.Short() {
		b.Skip("skipped in -short")
	}
	benchConcurrentSpawn(b, 100, 34000)
}

func benchConcurrentSpawn(b *testing.B, parallelism, portBase int) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sup := newTestSupervisor()
		var wg sync.WaitGroup
		errCh := make(chan error, parallelism)
		b.StartTimer()
		for k := 0; k < parallelism; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				_, err := sup.Spawn(Config{
					ID:      fmt.Sprintf("c%d-%d", i, k),
					Command: "sleep",
					Args:    []string{"30"},
					Port:    portBase + k,
				})
				if err != nil {
					errCh <- err
				}
			}(k)
		}
		wg.Wait()
		b.StopTimer()
		close(errCh)
		for err := range errCh {
			b.Fatalf("Spawn error: %v", err)
		}
		// Cleanup before next iteration; cost excluded from ns/op.
		for k := 0; k < parallelism; k++ {
			_ = sup.Stop(fmt.Sprintf("c%d-%d", i, k))
		}
	}
}

// BenchmarkList measures the registry snapshot cost as the app
// count grows. ListAll renders the admin API's GET /v1/apps; if it
// becomes O(N²) we want to know.
func BenchmarkList(b *testing.B) {
	cases := []int{1, 10, 100}
	for _, n := range cases {
		b.Run(fmt.Sprintf("apps=%d", n), func(b *testing.B) {
			sup := newTestSupervisor()
			for i := 0; i < n; i++ {
				if _, err := sup.Spawn(Config{
					ID:      fmt.Sprintf("l%d", i),
					Command: "sleep",
					Args:    []string{"30"},
					Port:    35000 + i,
				}); err != nil {
					b.Fatalf("Spawn: %v", err)
				}
			}
			b.Cleanup(func() {
				for i := 0; i < n; i++ {
					_ = sup.Stop(fmt.Sprintf("l%d", i))
				}
			})

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = sup.List()
			}
		})
	}
}
