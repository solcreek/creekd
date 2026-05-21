//go:build !creekd_sandbox

package main

import (
	"context"
	"errors"
	"log/slog"
)

func runSandbox(_ context.Context, _ *slog.Logger, _ []string) error {
	return errors.New("sandbox is not available in this build; rebuild with -tags creekd_sandbox")
}
