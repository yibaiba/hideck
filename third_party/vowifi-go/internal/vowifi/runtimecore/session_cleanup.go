package runtimecore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"go.uber.org/zap"
)

const sessionCleanupTimeout = 5 * time.Second

func defaultSessionStarter(ctx context.Context, cfg SessionConfig) (*SessionResult, error) {
	return RunSession(ctx, cfg)
}

// StopSession releases every resource owned by a completed session start.
func StopSession(ctx context.Context, result *SessionResult) {
	defaultStopSession(ctx, result)
}

func defaultStopSession(ctx context.Context, result *SessionResult) {
	if result == nil {
		return
	}
	if result.IMSService != nil {
		if err := result.IMSService.Stop(ctx); err != nil {
			zap.S().Warnw("failed to stop IMS service", "device", result.DeviceID, "error", err)
		}
	}
	var waitEPDG func(context.Context) error
	if result.EPDGMgr != nil {
		wait, err := result.EPDGMgr.StopAsync(result.DeviceID)
		if err != nil && !strings.Contains(err.Error(), "session id") {
			zap.S().Warnw("failed to stop ePDG session", "device", result.DeviceID, "error", err)
		}
		waitEPDG = wait
	}
	if result.XCAPNetwork != nil && result.XCAPNetwork != result.IMSNetwork {
		_ = result.XCAPNetwork.Close()
	}
	if result.IMSNetwork != nil {
		_ = result.IMSNetwork.Close()
	}
	// Wait after the networks are closed, as before; the runner exits only
	// once the SWu session goroutines and their local cleanup are done.
	if waitEPDG != nil {
		if err := waitEPDG(context.Background()); err != nil {
			zap.S().Warnw("SWu session cleanup failed", "device", result.DeviceID, "error", err)
			result.cleanupErr = errors.Join(result.cleanupErr, err)
		}
	}
	waitSessionCleanup(result.Session, result.DeviceID)
	CleanupDataplaneInterface(result.DeviceID, result.Snapshot.TUNName)
}

func waitSessionCleanup(session *swu.Session, deviceID string) {
	if session == nil {
		return
	}
	timer := time.NewTimer(sessionCleanupTimeout)
	defer timer.Stop()
	select {
	case <-sessionDoneChan(session):
	case <-timer.C:
		zap.S().Warnw("SWu session cleanup timed out", "device", strings.TrimSpace(deviceID))
	}
}

func sessionDoneChan(session *swu.Session) <-chan struct{} {
	if session == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		session.WaitDone()
		close(done)
	}()
	return done
}
