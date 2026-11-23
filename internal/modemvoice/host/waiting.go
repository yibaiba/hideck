package host

import (
	"context"
	"errors"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice"
)

// Called with op held. Only the foreground call owns the hardware audio route.
func (c *Controller) applyOtherCall(d *device, change modemvoice.Change) {
	id := "modemvoice-" + change.Call.ID
	d.mu.Lock()
	other := otherCallLocked(d, change.Call.ID)
	if other != nil {
		id = other.snapshot.CallID
	}
	if change.Ended {
		delete(d.otherCalls, id)
		d.mu.Unlock()
		if other != nil {
			reason := other.endReason
			if reason == "" {
				reason = "remote_hangup"
			}
			c.publishCallEnd(d, other, reason)
		}
		return
	}
	if other == nil {
		if !change.Call.Call.Inbound || change.Call.Call.State != modemvoice.Waiting {
			d.mu.Unlock()
			return
		}
		other = &call{trackedID: change.Call.ID, snapshot: voicehost.CallSnapshot{CallID: id, DeviceID: d.id, Direction: "inbound", Peer: change.Call.Call.Number, State: "waiting", StartTime: time.Now()}}
		if d.otherCalls == nil {
			d.otherCalls = make(map[string]*call)
		}
		d.otherCalls[id] = other
		d.mu.Unlock()
		c.publish(notification{event: voicehost.CallEvent{Type: "CallWaiting", DeviceID: d.id, CallID: id, Caller: other.snapshot.Peer, Direction: "inbound", State: "waiting", Time: time.Now()}})
		return
	}
	previousState := other.snapshot.State
	held := change.Call.Call.State == modemvoice.Held
	other.snapshot.Held = held
	switch change.Call.Call.State {
	case modemvoice.Active, modemvoice.Held:
		other.snapshot.State = "connected"
	case modemvoice.Waiting:
		other.snapshot.State = "waiting"
	case modemvoice.Incoming:
		// A call waiting offer the network withdrew and alerts again is
		// answerable, not held: reporting it as "waiting" would keep the
		// browser on a recovery panel that can restore audio but never
		// answer the call.
		other.snapshot.State = "ringing"
	}
	snapshot := other.snapshot
	d.mu.Unlock()
	kind := "CallMediaUpdated"
	switch {
	case previousState != "connected" && snapshot.State == "connected":
		kind = "CallAnswered"
	case previousState != "ringing" && snapshot.State == "ringing":
		kind = "CallRinging"
	}
	c.publish(notification{event: voicehost.CallEvent{Type: kind, DeviceID: d.id, CallID: id, State: snapshot.State, Held: held, Time: time.Now()}})
}

func (c *Controller) publishCallEnd(d *device, current *call, reason string) {
	event := voicehost.CallEvent{Type: "CallEnded", DeviceID: d.id, CallID: current.snapshot.CallID, Direction: current.snapshot.Direction, Reason: reason, Time: time.Now()}
	c.publish(notification{event: event})
	event.Type = "CallFinalized"
	c.publish(notification{event: event})
}

type waitingAnswerRequest struct {
	current *call
	sdp     string
}

func (c *Controller) answerWaiting(ctx context.Context, d *device, request waitingAnswerRequest) (voicehost.AnswerResult, error) {
	current, sdp := request.current, request.sdp
	_, err := relay(sdp)
	if err != nil {
		return voicehost.AnswerResult{}, err
	}
	update, answerErr := d.session.Answer(ctx, current.trackedID)
	applyErr := c.apply(d, update)
	if answerErr != nil || applyErr != nil {
		// A lost CHLD response may still have answered the physical call.
		refresh, refreshErr := d.session.Refresh(d.ctx)
		applyErr = errors.Join(applyErr, refreshErr, c.apply(d, refresh))
		if !trackedCallActive(d, current.trackedID) || !ownsCall(d, current) {
			return voicehost.AnswerResult{}, errors.Join(answerErr, applyErr)
		}
	}
	err = c.transferWaitingAudio(d, current, sdp)
	if err != nil {
		return voicehost.AnswerResult{}, errors.Join(answerErr, applyErr, err)
	}
	// CLCC, not CHLD acceptance, confirms answer and hold states.
	refresh, refreshErr := d.session.Refresh(ctx)
	return voicehost.AnswerResult{CallID: current.snapshot.CallID, OfferSDP: current.snapshot.ClientSDP, State: "answering"}, errors.Join(answerErr, applyErr, refreshErr, c.apply(d, refresh))
}

func (c *Controller) transferWaitingAudio(d *device, current *call, sdp string) error {
	remote, err := relay(sdp)
	if err != nil {
		return err
	}
	// Preserve one audio owner. The old call stays addressable while held.
	d.mu.Lock()
	previous := d.call
	if previous != nil && previous != current {
		current.conn, current.bridge, current.cancelMedia = previous.conn, previous.bridge, previous.cancelMedia
		current.snapshot.ClientSDP = previous.snapshot.ClientSDP
		previous.conn, previous.bridge, previous.cancelMedia = nil, nil, nil
		d.otherCalls[previous.snapshot.CallID] = previous
	}
	delete(d.otherCalls, current.snapshot.CallID)
	d.call = current
	d.mu.Unlock()
	if current.bridge != nil {
		err = current.bridge.SetRemote(remote)
	} else {
		err = c.prepareAnswerMedia(d, current, sdp)
	}
	return err
}

func ownsCall(d *device, current *call) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.call == current || d.otherCalls[current.snapshot.CallID] == current
}

// Caller holds d.mu. Outbound application IDs differ from CLCC tracker IDs.
func otherCallLocked(d *device, trackedID string) *call {
	for _, other := range d.otherCalls {
		if other.trackedID == trackedID {
			return other
		}
	}
	return nil
}

func trackedCallActive(d *device, id string) bool {
	for _, tracked := range d.session.Calls() {
		if tracked.ID == id {
			return tracked.Call.State == modemvoice.Active
		}
	}
	return false
}
