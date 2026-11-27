package runtimecore

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/iniwex5/vowifi-go/engine/swu"
)

// A stop must not hide local cleanup failures behind ctx.Err().
func TestRunLoopKeepsCleanupFailureAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cleanupErr := fmt.Errorf("%w: delete TUN: busy", swu.ErrSessionCleanup)
	err := RunLoop(ctx, nil, nil, nil, func(context.Context) error {
		cancel()
		return errors.Join(context.Canceled, cleanupErr)
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, swu.ErrSessionCleanup) {
		t.Fatalf("RunLoop err = %v, want context.Canceled and ErrSessionCleanup", err)
	}
}
