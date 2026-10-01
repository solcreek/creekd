package supervisor

import "os"

// SpawnGateArg is the argv[1] that makes a creekd binary act as a spawn
// gate (see startProcess) instead of doing its normal work.
const SpawnGateArg = "__spawn-gate"

// RunSpawnGateIfRequested turns this process into a spawn gate when it
// was started as one, and never returns in that case. Call it first in
// main — and in TestMain of any package whose tests spawn apps, since
// there the gate re-executes the test binary.
func RunSpawnGateIfRequested() {
	if len(os.Args) > 1 && os.Args[1] == SpawnGateArg {
		os.Exit(runSpawnGate(os.Args[2:]))
	}
}
