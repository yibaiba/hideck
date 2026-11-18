package phone

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/pkg/logger"
)

func (s *Service) handleIncoming(incoming voicehost.IncomingCall) {
	if strings.TrimSpace(incoming.CallID) == "" {
		return
	}
	startedAt := incoming.ReceivedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	call := s.newIncomingCall(incoming, startedAt)
	s.mu.Lock()
	if _, duplicate := s.calls[incoming.CallID]; duplicate {
		s.mu.Unlock()
		return
	}
	if existingID, busy := s.deviceCalls[incoming.DeviceID]; busy {
		existing := s.calls[existingID]
		if existing != nil && !existing.terminal && existing.view.Status == StatusConnected &&
			strings.TrimSpace(s.deviceWaiting[incoming.DeviceID]) == "" {
			s.calls[incoming.CallID] = call
			s.deviceWaiting[incoming.DeviceID] = incoming.CallID
			call.view.Status, call.record.Status = StatusWaiting, StatusWaiting
			pending, alreadyEnded := s.takePendingLocked(incoming.CallID)
			s.mu.Unlock()
			if !alreadyEnded {
				s.persist(call.record)
				s.publish("call_waiting", call)
				if s.notifier != nil {
					go s.notifier.NotifyIncomingCall(incoming.DeviceID, incoming.Caller, incoming.Callee)
				}
			}
			for _, event := range pending {
				s.dispatchCallEvent(event)
			}
			return
		}
		s.mu.Unlock()
		err := s.gateway.RejectIncomingCall(voicehost.RejectRequest{
			DeviceID: incoming.DeviceID, CallID: incoming.CallID, StatusCode: 486,
		})
		s.recordBusyResult(voicehost.CallEvent{
			Type: "CallBusy", DeviceID: incoming.DeviceID, CallID: incoming.CallID,
			Caller: incoming.Caller, Time: startedAt,
		}, err)
		return
	}
	s.calls[incoming.CallID] = call
	s.deviceCalls[incoming.DeviceID] = incoming.CallID
	pending, alreadyEnded := s.takePendingLocked(incoming.CallID)
	s.mu.Unlock()
	if !alreadyEnded {
		if err := s.gateway.StartCallCapture(incoming.DeviceID, incoming.CallID, call.recordingBase); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			s.mu.Lock()
			call.record.RecordingError = err.Error()
			call.view.RecordingError = err.Error()
			s.mu.Unlock()
		}
		s.mu.RLock()
		record := call.record
		s.mu.RUnlock()
		s.persist(record)
		s.publish("incoming_call", call)
		if s.notifier != nil {
			go s.notifier.NotifyIncomingCall(incoming.DeviceID, incoming.Caller, incoming.Callee)
		}
		s.scheduleAutoAnswer(call)
	}
	for _, event := range pending {
		s.dispatchCallEvent(event)
	}
}

func (s *Service) handleCallWaiting(event voicehost.CallEvent) {
	s.mu.RLock()
	existing := s.calls[event.CallID]
	s.mu.RUnlock()
	if existing != nil {
		s.publish("call_waiting", existing)
		return
	}
	s.handleIncoming(voicehost.IncomingCall{
		DeviceID: event.DeviceID, CallID: event.CallID,
		Caller: event.Caller, Callee: event.Callee, ReceivedAt: event.Time,
	})
}

func (s *Service) newIncomingCall(incoming voicehost.IncomingCall, startedAt time.Time) *activeCall {
	peer := strings.TrimSpace(incoming.Caller)
	view := CallView{
		CallID: incoming.CallID, DeviceID: incoming.DeviceID, Direction: "inbound",
		Peer: peer, Status: StatusRinging, StartedAt: startedAt, ReadOnly: true,
	}
	record := CallRecord{
		CallID: incoming.CallID, DeviceID: incoming.DeviceID, ICCID: s.iccid(incoming.DeviceID),
		Direction: "inbound", Peer: peer, Status: StatusRinging, StartedAt: startedAt,
	}
	return &activeCall{
		view: view, record: record, incomingSDP: incoming.OfferSDP,
		recordingBase: s.captureBase(incoming.DeviceID, startedAt),
		terminalDone:  make(chan struct{}), finalizedDone: make(chan struct{}),
	}
}

func (s *Service) handleCallEvent(event voicehost.CallEvent) {
	if s.bufferPendingCallEvent(event) {
		return
	}
	s.dispatchCallEvent(event)
}

func (s *Service) bufferPendingCallEvent(event voicehost.CallEvent) bool {
	if event.CallID == "" || event.Type == "CallBusy" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls[event.CallID] != nil {
		return false
	}
	reservedCallID, reserved := s.deviceCalls[event.DeviceID]
	deviceReservedEmpty := reserved && reservedCallID == ""
	if !deviceReservedEmpty && !earlyCallEvent(event.Type) {
		return false
	}
	s.pendingEvents[event.CallID] = append(s.pendingEvents[event.CallID], event)
	return true
}

func earlyCallEvent(eventType string) bool {
	switch eventType {
	case "CallEnded", "CallFailed", "CallCanceled", "CallRinging", "CallAnswered", "CallFinalized":
		return true
	default:
		return false
	}
}

func (s *Service) takePendingLocked(callID string) ([]voicehost.CallEvent, bool) {
	pending := s.pendingEvents[callID]
	delete(s.pendingEvents, callID)
	alreadyEnded := false
	if _, ok := s.terminalSeen[callID]; ok {
		alreadyEnded = true
	}
	for _, event := range pending {
		switch event.Type {
		case "CallEnded", "CallFailed", "CallCanceled":
			alreadyEnded = true
		}
	}
	return pending, alreadyEnded
}

func (s *Service) dispatchCallEvent(event voicehost.CallEvent) {
	switch event.Type {
	case "CallRinging":
		s.updateRinging(event.CallID)
	case "CallAnswered":
		s.updateAnswered(event)
	case "CallEnded", "CallFailed", "CallCanceled":
		s.finishCall(event)
	case "CallBusy":
		s.recordBusyResult(event, nil)
	case "CallWaiting":
		s.handleCallWaiting(event)
	case "CallFinalized":
		s.finalizeRecording(event)
	case "CallMediaUpdated":
		s.publishMediaUpdate(event)
	}
}

func (s *Service) updateRinging(callID string) {
	s.mu.Lock()
	call := s.calls[callID]
	if call == nil || call.terminal || call.view.Status == StatusConnected {
		s.mu.Unlock()
		return
	}
	// A waiting offer can fall back to alerting when the network withdraws it,
	// for example while the operator hangs another call up. It is answerable
	// again, so it must leave "waiting" or the page keeps offering a media
	// recovery that can never answer it.
	if s.deviceWaiting[call.view.DeviceID] == call.view.CallID {
		delete(s.deviceWaiting, call.view.DeviceID)
	}
	call.view.Status, call.record.Status = StatusRinging, StatusRinging
	record := call.record
	s.mu.Unlock()
	s.persist(record)
	s.publish("call_ringing", call)
}

func (s *Service) updateAnswered(event voicehost.CallEvent) {
	s.mu.Lock()
	call := s.calls[event.CallID]
	if call == nil || call.terminal || call.view.AnsweredAt != nil {
		s.mu.Unlock()
		return
	}
	answeredAt := event.Time
	if answeredAt.IsZero() {
		answeredAt = time.Now()
	}
	call.view.Status, call.record.Status = StatusConnected, StatusConnected
	call.view.AnsweredAt, call.record.AnsweredAt = &answeredAt, &answeredAt
	if codec := strings.TrimSpace(event.AudioCodec); codec != "" {
		call.view.Codec, call.record.Codec = codec, codec
	}
	mediaID, direction, deviceID := call.mediaID, call.view.Direction, call.view.DeviceID
	s.mu.Unlock()
	if direction == "outbound" {
		if media := s.attachOutboundMedia(event.CallID, deviceID, mediaID); media != nil {
			s.startMixedRecording(call, media)
		}
	}
	s.mu.RLock()
	record := call.record
	s.mu.RUnlock()
	s.persist(record)
	s.publish("call_answered", call)
}

func (s *Service) attachOutboundMedia(callID, deviceID, mediaID string) *MediaSession {
	media := s.media.Get(mediaID)
	snapshot := s.gateway.ActiveCall(deviceID)
	if media == nil || snapshot == nil || snapshot.ClientSDP == "" {
		s.failMediaAttachment(callID, errors.New("phone: negotiated outbound media endpoint is unavailable"))
		return nil
	}
	endpoint, err := parseRTPEndpoint(snapshot.ClientSDP)
	if err == nil {
		s.mu.Lock()
		if call := s.calls[callID]; call != nil {
			call.view.Codec, call.record.Codec = endpoint.Codec, endpoint.Codec
		}
		s.mu.Unlock()
	}
	if err = media.Attach(snapshot.ClientSDP); err != nil {
		s.failMediaAttachment(callID, err)
		return nil
	}
	return media
}

func (s *Service) failMediaAttachment(callID string, err error) {
	s.mu.Lock()
	call := s.calls[callID]
	if call == nil || call.terminal {
		s.mu.Unlock()
		return
	}
	call.view.EndReason, call.record.EndReason = err.Error(), err.Error()
	deviceID, resolvedCallID := call.view.DeviceID, call.view.CallID
	s.mu.Unlock()
	s.publish("media_error", call)
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	if hangupErr := s.gateway.HangupCall(ctx, deviceID, resolvedCallID); hangupErr != nil {
		logger.Error("媒体桥接失败后挂断失败", "device_id", deviceID, "call_id", resolvedCallID, "err", hangupErr)
	}
}

func (s *Service) finishCall(event voicehost.CallEvent) {
	s.mu.Lock()
	call := s.calls[event.CallID]
	if call == nil {
		s.terminalSeen[event.CallID] = struct{}{}
		s.pendingEvents[event.CallID] = append(s.pendingEvents[event.CallID], event)
		s.mu.Unlock()
		return
	}
	if call.terminal {
		s.mu.Unlock()
		return
	}
	endedAt := event.Time
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	status := terminalStatus(call, event)
	call.terminal, call.view.Status, call.record.Status = true, status, status
	call.view.EndedAt, call.record.EndedAt = &endedAt, &endedAt
	call.view.EndReason, call.record.EndReason = event.Reason, event.Reason
	call.record.DurationSeconds = callDurationSeconds(call.record, endedAt)
	if call.disconnectTimer != nil {
		call.disconnectTimer.Stop()
	}
	if call.autoAnswerTimer != nil {
		call.autoAnswerTimer.Stop()
		call.autoAnswerTimer = nil
	}
	if s.deviceCalls[call.view.DeviceID] == call.view.CallID {
		delete(s.deviceCalls, call.view.DeviceID)
	}
	if s.deviceWaiting[call.view.DeviceID] == call.view.CallID {
		delete(s.deviceWaiting, call.view.DeviceID)
	}
	delete(s.mediaCalls, call.mediaID)
	s.clearPendingMediaDropLocked(call.mediaID)
	s.terminalSeen[event.CallID] = struct{}{}
	mediaID := call.mediaID
	// One browser owns one media session, and call waiting hands the same
	// session to the second call. Ending the first call must not tear that
	// session down while the other call still depends on it.
	mediaShared := mediaID != "" && s.mediaUsedByOtherCallLocked(mediaID, call.view.CallID)
	deviceID, peer, direction := call.view.DeviceID, call.view.Peer, call.view.Direction
	s.mu.Unlock()
	defer call.terminalOnce.Do(func() { close(call.terminalDone) })
	s.stopMixedRecording(call)
	if mediaID != "" && !mediaShared {
		s.media.Remove(mediaID)
	} else if mediaShared {
		logger.Info("媒体仍被其他通话使用，保留会话", "media_id", mediaID, "ended_call", call.view.CallID)
	}
	s.mu.RLock()
	record := call.record
	s.mu.RUnlock()
	s.persist(record)
	s.publish("call_ended", call)
	if s.notifier != nil {
		go s.notifier.NotifyCallResult(deviceID, peer, direction, status, event.Reason, endedAt)
	}
}

func terminalStatus(call *activeCall, event voicehost.CallEvent) string {
	if call.view.AnsweredAt != nil || call.view.Status == StatusConnected {
		return StatusCompleted
	}
	if call.view.Direction != "inbound" {
		return StatusFailed
	}
	if call.userRejected {
		return StatusRejected
	}
	reason := strings.ToLower(strings.TrimSpace(event.Reason))
	if event.Type == "CallCanceled" || remoteCancelReason(reason) {
		return StatusMissed
	}
	return StatusFailed
}

func remoteCancelReason(reason string) bool {
	if reason == "" {
		return false
	}
	switch reason {
	case "normal", "remote_cancel", "remote_bye", "canceled", "cancelled":
		return true
	}
	return strings.Contains(reason, "timed out") ||
		strings.Contains(reason, "timeout") ||
		strings.Contains(reason, "canceled by ims") ||
		strings.Contains(reason, "cancelled by ims")
}

func callDurationSeconds(record CallRecord, endedAt time.Time) int64 {
	start := record.StartedAt
	if record.AnsweredAt != nil {
		start = *record.AnsweredAt
	}
	if start.IsZero() || endedAt.Before(start) {
		return 0
	}
	return int64(endedAt.Sub(start) / time.Second)
}

func (s *Service) publishMediaUpdate(event voicehost.CallEvent) {
	s.mu.Lock()
	call := s.calls[event.CallID]
	if call != nil && !call.terminal {
		call.view.Held = event.Held
	}
	s.mu.Unlock()
	if call != nil && !call.terminal {
		s.publish("media_updated", call)
	}
}
