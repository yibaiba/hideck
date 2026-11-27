package runtimecore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/epdg"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
	"go.uber.org/zap"
)

const epdgEstablishmentTimeout = 45 * time.Second

type epdgSessionManager interface {
	Start(context.Context, string, *swu.Config) (*swu.Session, error)
	Wait(context.Context, string, time.Duration) (swu.SessionSnapshot, error)
	Stop(string) error
}

func StartAndWaitEPDG(
	ctx context.Context,
	deviceID string,
	traceID string,
	config *swu.Config,
	manager epdgSessionManager,
) (*swu.Session, swu.SessionSnapshot, error) {
	if config == nil {
		return nil, swu.SessionSnapshot{}, errors.New("runtimecore: nil SWu config")
	}
	if manager == nil {
		manager = epdg.New()
	}
	session, snapshot, err := startAndWait(ctx, deviceID, config, manager)
	if epdg.ShouldRetryFreshTunnel(ctx, err) {
		logging.Info("SWu first ePDG wait timed out; retrying with a fresh session",
			"device", strings.TrimSpace(deviceID), "trace_id", strings.TrimSpace(traceID))
		session, snapshot, err = startAndWait(ctx, deviceID, config, manager)
	}
	if err == nil || !shouldRetryDeviceIdentity(config, err) {
		return session, snapshot, err
	}
	logging.Info("SWu device identity rejected; retrying without spoofed identity",
		"device", strings.TrimSpace(deviceID), "trace_id", strings.TrimSpace(traceID), "error", err)
	retryConfig := *config
	retryConfig.EnableDeviceIdentitySpoof = false
	retryConfig.DeviceIdentityIMEI = ""
	return startAndWait(ctx, deviceID, &retryConfig, manager)
}

func startAndWait(
	ctx context.Context,
	deviceID string,
	config *swu.Config,
	manager epdgSessionManager,
) (*swu.Session, swu.SessionSnapshot, error) {
	session, err := manager.Start(ctx, deviceID, config)
	if err != nil {
		return nil, swu.SessionSnapshot{}, err
	}
	snapshot, err := manager.Wait(ctx, deviceID, epdgEstablishmentTimeout)
	if err == nil {
		return session, snapshot, nil
	}
	stopErr := stopEPDGAndWait(manager, deviceID)
	if stopErr != nil && !strings.Contains(stopErr.Error(), "session id") {
		if errors.Is(stopErr, swu.ErrSessionCleanup) {
			zap.S().Warnw("failed SWu attempt left a local cleanup failure",
				"device", strings.TrimSpace(deviceID), "error", stopErr)
		}
		err = errors.Join(err, stopErr)
	}
	return nil, swu.SessionSnapshot{}, err
}

// stopEPDGAndWait returns only after the SWu session goroutine has exited, so
// a cancelled start no longer uses the SIM when the runner returns. The wait is
// unbounded here; Instance.Stop bounds and reports it on the host side.
func stopEPDGAndWait(manager epdgSessionManager, deviceID string) error {
	if waiter, ok := manager.(interface {
		StopAndWait(context.Context, string) error
	}); ok {
		return waiter.StopAndWait(context.Background(), deviceID)
	}
	zap.S().Warnw("SWu session manager cannot wait for session exit; stopping without waiting",
		"device", strings.TrimSpace(deviceID))
	return manager.Stop(deviceID)
}

func shouldRetryDeviceIdentity(config *swu.Config, err error) bool {
	if config == nil || !config.EnableDeviceIdentitySpoof || err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "auth failed") ||
		strings.Contains(message, "EAP \u8ba4\u8bc1\u5931\u8d25")
}
