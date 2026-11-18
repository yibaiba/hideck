package volte

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"github.com/yibaiba/hideck/pkg/logger"
)

// qmiVoiceHost is implemented by hosts that carry voice over QMI. Backends
// without QMI (an AT-only device) leave it unimplemented on purpose: there is
// no QMI call state to reconcile, so a handoff must not try to query it.
type qmiVoiceHost interface {
	HasQMIVoice(deviceID string) bool
}

// qmiVoiceAvailable reports whether this device can be asked about QMI call
// state at all. A session that failed to bring up IMS clients (the
// "no QMI" path) is treated as unreconcilable for the same reason: its call
// state was never owned by QMI, so querying it can only fail.
func (c *Controller) qmiVoiceAvailable(deviceID string, s *session) bool {
	if host, ok := c.host.(qmiVoiceHost); ok && !host.HasQMIVoice(deviceID) {
		return false
	}
	if s != nil && s.status.QMIIMSUnavailable {
		return false
	}
	return true
}

// DisableForHandoff confirms physical calls have ended before another backend
// takes ownership. Control failure is not evidence that the modem hung up.
func (c *Controller) DisableForHandoff(ctx context.Context, deviceID string) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("volte: handoff context is required")
	}
	deviceID = strings.TrimSpace(deviceID)
	return c.withDevice(deviceID, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		s := c.sess[deviceID]
		c.mu.Unlock()
		if s == nil {
			return nil
		}
		if !c.qmiVoiceAvailable(deviceID, s) {
			// Nothing was ever dialled through QMI on this device, so there is
			// no call to confirm. Drop the session instead of reporting a
			// handoff failure that the operator cannot act on.
			logger.Warn("无 QMI 语音能力，直接丢弃 VoLTE 会话", "device", deviceID)
			c.mu.Lock()
			s.gen++
			delete(c.sess, deviceID)
			c.mu.Unlock()
			return nil
		}
		if err := c.endCallsForHandoff(ctx, deviceID); err != nil {
			return err
		}
		if err := c.host.ReleaseIMSClients(deviceID); err != nil {
			return fmt.Errorf("volte: release IMS clients for handoff: %w", err)
		}
		c.mu.Lock()
		s.gen++
		delete(c.sess, deviceID)
		c.mu.Unlock()
		return nil
	})
}

func (c *Controller) endCallsForHandoff(ctx context.Context, deviceID string) error {
	info, err := c.callsForHandoff(ctx, deviceID)
	if err != nil {
		return err
	}
	for _, call := range info.Calls {
		if call.State == qmiCallIdle || call.State == qmiCallEnd {
			continue
		}
		err := c.host.VOICEHangup(ctx, deviceID, call.ID)
		if err != nil && !qmi.VoiceCallAlreadyGone(err) {
			return fmt.Errorf("volte: hang up call %d for handoff: %w", call.ID, err)
		}
	}
	// An accepted hangup alone is insufficient: pending disconnects or new
	// incoming calls leave ownership here until an explicit retry succeeds.
	info, err = c.callsForHandoff(ctx, deviceID)
	if err != nil {
		return err
	}
	for _, call := range info.Calls {
		if call.State != qmiCallIdle && call.State != qmiCallEnd {
			return fmt.Errorf("volte: call %d is still active; retry switching after it ends", call.ID)
		}
	}
	if vs := c.sessionVoice(deviceID); vs != nil {
		c.handleVoiceInfo(deviceID, vs, info)
	}
	return nil
}

func (c *Controller) callsForHandoff(ctx context.Context, deviceID string) (*qmi.VoiceAllCallInfo, error) {
	if c.host == nil {
		return nil, errors.New("volte: controller is not configured")
	}
	info, err := c.host.VOICEGetAllCallInfo(ctx, deviceID)
	if err != nil {
		return nil, fmt.Errorf("volte: query calls for handoff: %w", err)
	}
	if info == nil {
		return nil, errors.New("volte: empty call query response during handoff")
	}
	return info, nil
}
