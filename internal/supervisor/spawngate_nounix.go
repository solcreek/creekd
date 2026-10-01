//go:build !unix

package supervisor

import (
	"fmt"
	"os"
)

// runSpawnGate is never reached off Unix: startProcess only uses a gate
// on Linux. A binary started as one anyway reports it and fails.
func runSpawnGate(_ []string) int {
	fmt.Fprintln(os.Stderr, "creekd spawn gate: not supported on this platform")
	return 2
}
