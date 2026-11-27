package runtimehost

import (
	"context"

	"github.com/iniwex5/vowifi-go/internal/vowifi/runtimecore"
)

// StartWithRunnerForTest launches an instance whose runtime core is replaced
// by run, so host tests can keep the runner alive after cancellation.
func StartWithRunnerForTest(ctx context.Context, deviceID string, run func(context.Context) error) *Instance {
	instance, _, _ := launchRuntimeCore(ctx, runtimecore.RuntimeStartRequest{DeviceID: deviceID}, runtimeLaunchOptions{
		runner: func(ctx context.Context, _ runtimecore.RuntimeStartRequest) (StartResult, error) {
			return StartResult{}, run(ctx)
		},
	})
	return instance
}
