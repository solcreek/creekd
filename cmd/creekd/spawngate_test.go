package main

import (
	"os"
	"testing"

	"github.com/solcreek/creekd/internal/supervisor"
)

// TestMain lets this test binary serve as the spawn gate: apps these
// tests start re-execute it (supervisor.startProcess).
func TestMain(m *testing.M) {
	supervisor.RunSpawnGateIfRequested()
	os.Exit(m.Run())
}
