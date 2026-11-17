package host

import (
	"context"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
	"github.com/yibaiba/hideck/pkg/logger"
)

func (c *Controller) applyCall(d *device, change modemvoice.Change) error {
	d.mu.Lock()
	current := d.call
	d.mu.Unlock()
	if current == nil {
		d.mu.Lock()
		pending := otherCallLocked(d, change.Call.ID)
		if pending != nil && change.Ended {
			d.mu.Unlock()
			c.applyOtherCall(d, change)
			return nil
		}
		if pending != nil && !change.Ended {
			d.call = pending
			delete(d.otherCalls, pending.snapshot.CallID)
			current = pending
		}
		d.mu.Unlock()
	}
	if current == nil {
		if !change.Ended && change.Call.Call.Inbound {
			c.incomingCall(d, change.Call)
		}
		return nil
	}
	if current.trackedID == "" && !change.Call.Call.Inbound && !change.Ended {
		current.trackedID = change.Call.ID
	}
	if current.trackedID != change.Call.ID {
		c.applyOtherCall(d, change)
		return nil
	}
	if change.Ended {
		logger.Info("模组检测到通话结束", "device_id", d.id, "last_state", int(change.Call.Call.State), "mode", change.Call.Call.Mode, "remaining_calls", len(d.session.Calls()))
		reason := current.endReason
		if reason == "" {
			reason = "remote_hangup"
			if d.resources != nil && d.resources.Port != nil {
				ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
				response, err := d.resources.Port.ExecuteATContext(ctx, "AT+CEER", 3*time.Second)
				cancel()
				// Retain only the extended error line, never CLCC numbers or SIM IDs.
				for _, line := range strings.Split(response, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "+CEER:") {
						logger.Info("模组通话结束原因", "device_id", d.id, "reason", strings.TrimSpace(line))
					}
				}
				if err != nil {
					logger.Warn("读取模组通话结束原因失败", "device_id", d.id, "err", err)
				}
			}
		}
		if err := c.endMedia(d, reason); err != nil {
			d.setStatus("failed", err)
			return err
		}
		return nil
	}
	state, kind := "ringing", "CallRinging"
	if change.Call.Call.State == modemvoice.Active || change.Call.Call.State == modemvoice.Held {
		state, kind = "connected", "CallAnswered"
	}
	d.mu.Lock()
	previous := current.snapshot.State
	previousHeld := current.snapshot.Held
	current.snapshot.State = state
	current.snapshot.Held = change.Call.Call.State == modemvoice.Held
	snapshot := current.snapshot
	d.mu.Unlock()
	if previous != state {
		c.publish(notification{event: voicehost.CallEvent{Type: kind, DeviceID: d.id, CallID: snapshot.CallID,
			Direction: snapshot.Direction, State: state, Time: time.Now(), AudioCodec: "PCMU"}})
	}
	if previousHeld != snapshot.Held {
		c.publish(notification{event: voicehost.CallEvent{Type: "CallMediaUpdated", DeviceID: d.id, CallID: snapshot.CallID, State: state, Held: snapshot.Held, Time: time.Now()}})
	}
	return nil
}

func (c *Controller) incomingCall(d *device, tracked modemvoice.TrackedCall) {
	if tracked.Call.State != modemvoice.Incoming {
		return
	}
	conn, err := c.options.Listen()
	if err != nil {
		d.setStatus("failed", err)
		return
	}
	snapshot := voicehost.CallSnapshot{DeviceID: d.id, CallID: "modemvoice-" + tracked.ID, Direction: "inbound",
		Peer: tracked.Call.Number, State: "ringing", StartTime: time.Now(), ClientSDP: offer(conn)}
	d.mu.Lock()
	d.call = &call{snapshot: snapshot, trackedID: tracked.ID, conn: conn}
	d.mu.Unlock()
	c.publish(notification{incoming: &voicehost.IncomingCall{DeviceID: d.id, CallID: snapshot.CallID,
		Caller: snapshot.Peer, OfferSDP: snapshot.ClientSDP, ReceivedAt: snapshot.StartTime, State: "ringing"}})
}
