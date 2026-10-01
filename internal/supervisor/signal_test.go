package supervisor

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A SIGKILL that cannot be delivered (EPERM: a non-root creekd without
// CAP_KILL, an app under another UID) must not hang Stop — and with it
// the admin request — forever. Stop returns an error naming the PID.
func TestStopReturnsWhenSignalsCannotBeDelivered(t *testing.T) {
	sup := newTestSupervisor()
	sup.GracefulShutdownTimeout = 100 * time.Millisecond
	sup.KillWaitTimeout = 200 * time.Millisecond

	app, err := sup.Spawn(Config{ID: "unkillable", Command: "sleep", Args: []string{"30"}, Port: 19600})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	pid := app.PID()
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill() // the real signal, after signalProcess is restored
		}
	})

	orig := signalProcess
	signalProcess = func(*os.Process, os.Signal) error { return syscall.EPERM }
	t.Cleanup(func() { signalProcess = orig })

	start := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- sup.Stop("unkillable") }()
	select {
	case err := <-errc:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("Stop reported success for a process it could not signal")
		}
		// It waited out the graceful window and KillWaitTimeout first.
		if min := sup.GracefulShutdownTimeout + sup.KillWaitTimeout; elapsed < min {
			t.Errorf("Stop returned after %v, before the %v it should wait", elapsed, min)
		}
		if want := "pid " + strconv.Itoa(pid); !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung on a SIGKILL that could not be delivered")
	}

	// The process is alive: the app must still be registered and
	// stoppable, not orphaned behind a 404.
	if sup.Get("unkillable") == nil {
		t.Fatal("after a failed stop the app is no longer registered")
	}
	signalProcess = orig
	if err := sup.Stop("unkillable"); err != nil {
		t.Fatalf("retrying the stop once signals work: %v", err)
	}
}

func TestSignalFailed(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"delivered":      {nil, false},
		"already exited": {os.ErrProcessDone, false},
		"no permission":  {syscall.EPERM, true},
	}
	for name, tc := range cases {
		if got := signalFailed(tc.err); got != tc.want {
			t.Errorf("%s: signalFailed(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}

func TestParseCapBitsKill(t *testing.T) {
	for in, want := range map[string]bool{
		"CapEff:\t00000000000004c0\n": false, // v0.1.3 unit: NET_BIND_SERVICE + SETUID + SETGID
		"CapEff:\t00000000000004e0\n": true,  // + KILL
		"CapEff:\t000001ffffffffff\n": true,  // root
	} {
		if got := parseCapBits([]byte(in), 1<<capKillBit); got != want {
			t.Errorf("parseCapBits(%q, CAP_KILL) = %v, want %v", in, got, want)
		}
	}
}
