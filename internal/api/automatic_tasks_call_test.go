package api

import (
	"context"
	"errors"
	"testing"

	"github.com/yibaiba/hideck/internal/automation"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/internal/modem"
)

func TestAutomaticCellularCallRejectsUnsupportedModemBeforePreparation(t *testing.T) {
	cfg := config.DeviceConfig{ID: "ml307c", DeviceBackend: "at", ATPort: "/dev/test-ml307c"}
	manager, err := modem.NewWithATProfile(cfg, modem.ATProfileML307C)
	if err != nil {
		t.Fatal(err)
	}
	pool := device.NewPool(&config.Config{})
	pool.AttachWorkerForTest(&device.Worker{ID: cfg.ID, Config: cfg, Modem: manager})
	executor := &automaticTaskExecutor{server: &Server{pool: pool}}

	_, err = executor.Execute(context.Background(), automation.Task{
		DeviceID: cfg.ID, TaskType: automation.TaskTypeCall, Environment: automation.EnvironmentCellular,
		ProfileICCID: "8944000000000000501", ProfileAID: "A0000005591010FFFFFFFF8900000100",
		Payload: automation.Payload{Phone: "888"},
	}, func(step string) error {
		t.Fatalf("unsupported cellular call reached preparation: %s", step)
		return nil
	})
	var unsupported *device.PhoneModeUnsupportedError
	if !errors.As(err, &unsupported) || automation.IsRetryable(err) {
		t.Fatalf("Execute() error = %v, want a permanent unsupported phone mode error", err)
	}
}
