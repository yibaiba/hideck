package phone

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

var calleePattern = regexp.MustCompile(`^\+?[0-9]{1,32}$`)
var dtmfPattern = regexp.MustCompile(`^[0-9*#]$`)
var errActiveCallNotFound = errors.New("phone: active call not found")

// ErrHoldUnavailable is returned when originating hold/resume is refused as not aligned.
var ErrHoldUnavailable = errors.New("保持未对齐，暂不可用")

func (s *Service) StartCall(request StartCallRequest) (CallView, error) {
	request.DeviceID, request.Callee = strings.TrimSpace(request.DeviceID), strings.TrimSpace(request.Callee)
	if request.DeviceID == "" || !calleePattern.MatchString(request.Callee) {
		return CallView{}, errors.New("phone: device and a valid callee are required")
	}
	media, reservation, err := s.reserveControlledMedia(request.Owner, request.MediaID, request.Lease)
	if err != nil {
		return CallView{}, err
	}
	defer s.releaseMediaReservation(reservation)
	request.MediaID = reservation.mediaID
	if err := s.reserveDevice(request.DeviceID); err != nil {
		return CallView{}, err
	}
	startedAt := time.Now()
	snapshot, err := s.gateway.BeginCall(s.ctx, voicehost.BeginCallRequest{
		DeviceID: request.DeviceID, Callee: request.Callee, SDP: media.PlainSDP(),
		CaptureBasePath: s.captureBase(request.DeviceID, startedAt),
	})
	// A backend may retain a physical call after a failed dial/cleanup. Keep
	// that call controllable while still returning the original failure.
	if err != nil && snapshot.CallID == "" {
		s.releaseDeviceReservation(request.DeviceID)
		return CallView{}, err
	}
	call := s.newOutboundCall(request, snapshot, startedAt)
	s.mu.Lock()
	pendingMediaDrop, bindErr := s.bindMediaLocked(call.view.CallID, reservation)
	if bindErr == nil {
		s.calls[call.view.CallID] = call
		s.deviceCalls[call.view.DeviceID] = call.view.CallID
	}
	pendingEvents := s.pendingEvents[call.view.CallID]
	delete(s.pendingEvents, call.view.CallID)
	s.mu.Unlock()
	if bindErr != nil {
		s.releaseDeviceReservation(request.DeviceID)
		hangupErr := s.gateway.HangupCall(s.ctx, call.view.DeviceID, call.view.CallID)
		return CallView{}, errors.Join(bindErr, hangupErr)
	}
	s.resumePendingMediaDrop(call.mediaID, pendingMediaDrop)
	s.persist(call.record)
	s.publish("call_started", call)
	for _, event := range pendingEvents {
		s.dispatchCallEvent(event)
	}
	return s.callView(call.view.CallID, request.Lease), err
}

func (s *Service) newOutboundCall(
	request StartCallRequest,
	snapshot voicehost.CallSnapshot,
	startedAt time.Time,
) *activeCall {
	view := CallView{
		CallID: snapshot.CallID, DeviceID: request.DeviceID, Direction: "outbound",
		Peer: request.Callee, Status: StatusCalling, MediaID: request.MediaID, StartedAt: startedAt,
	}
	record := CallRecord{
		CallID: snapshot.CallID, DeviceID: request.DeviceID, ICCID: s.iccid(request.DeviceID),
		Direction: "outbound", Peer: request.Callee, Status: StatusCalling, StartedAt: startedAt,
	}
	if endpoint, err := parseRTPEndpoint(snapshot.ClientSDP); err == nil && endpoint.Codec != "" {
		view.Codec, record.Codec = endpoint.Codec, endpoint.Codec
	}
	return &activeCall{
		view: view, record: record, owner: request.Owner, lease: request.Lease,
		mediaID: request.MediaID, recordingBase: s.captureBase(request.DeviceID, startedAt),
		terminalDone: make(chan struct{}), finalizedDone: make(chan struct{}),
	}
}

func (s *Service) Answer(ctx context.Context, request ControlRequest) (CallView, error) {
	call, media, reservation, err := s.claimIncoming(request)
	if err != nil {
		return CallView{}, err
	}
	defer s.releaseIncomingClaim(call)
	defer s.releaseMediaReservation(reservation)
	s.mu.RLock()
	incomingSDP, deviceID, callID := call.incomingSDP, call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	if err := media.Attach(incomingSDP); err != nil {
		rejectErr := s.gateway.RejectIncomingCall(voicehost.RejectRequest{
			DeviceID: deviceID, CallID: callID, StatusCode: 488,
		})
		return CallView{}, errors.Join(err, rejectErr)
	}
	answer, err := s.gateway.AnswerIncomingCall(ctx, voicehost.AnswerRequest{
		DeviceID: deviceID, CallID: callID, SDP: media.PlainSDP(),
	})
	if err != nil {
		return CallView{}, err
	}
	if answer.OfferSDP != "" && answer.OfferSDP != incomingSDP {
		if err := media.Attach(answer.OfferSDP); err != nil {
			return CallView{}, err
		}
	}
	if err := s.assignControl(call.view.CallID, reservation); err != nil {
		hangupErr := s.gateway.HangupCall(ctx, deviceID, callID)
		return CallView{}, errors.Join(err, hangupErr)
	}
	s.startMixedRecording(call, media)
	return s.callView(callID, request.Lease), nil
}

func (s *Service) Reject(request ControlRequest) error {
	call, _, reservation, err := s.claimIncoming(request)
	if err != nil {
		return err
	}
	defer s.releaseIncomingClaim(call)
	defer s.releaseMediaReservation(reservation)
	s.mu.Lock()
	if current := s.calls[request.CallID]; current != call || call.terminal {
		s.mu.Unlock()
		return errActiveCallNotFound
	}
	pendingMediaDrop, err := s.bindMediaLocked(request.CallID, reservation)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	call.userRejected = true
	call.owner, call.lease, call.mediaID = request.Owner, request.Lease, reservation.mediaID
	call.view.MediaID = reservation.mediaID
	deviceID := call.view.DeviceID
	s.mu.Unlock()
	s.resumePendingMediaDrop(reservation.mediaID, pendingMediaDrop)
	if err := s.gateway.RejectIncomingCall(voicehost.RejectRequest{
		DeviceID: deviceID, CallID: request.CallID, StatusCode: 486,
	}); err != nil {
		return err
	}
	s.finishCall(voicehost.CallEvent{
		Type: "CallCanceled", DeviceID: deviceID, CallID: request.CallID,
		Reason: "local_reject", Time: time.Now(),
	})
	return nil
}

func (s *Service) Hangup(ctx context.Context, owner, callID, lease string) error {
	call, err := s.controlledCall(owner, callID, lease)
	if err != nil {
		if errors.Is(err, errActiveCallNotFound) {
			return nil
		}
		return err
	}
	s.mu.RLock()
	deviceID, resolvedCallID := call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	if err := s.gateway.HangupCall(ctx, deviceID, resolvedCallID); err != nil {
		return err
	}
	// Native lookup can succeed the HTTP hangup without emitting CallEnded.
	s.finishCall(voicehost.CallEvent{
		Type: "CallEnded", DeviceID: deviceID, CallID: resolvedCallID,
		Reason: "local_hangup", Time: time.Now(),
	})
	return nil
}

func (s *Service) DTMF(owner, callID, lease, digit string) error {
	if !dtmfPattern.MatchString(digit) {
		return errors.New("phone: DTMF must be one of 0-9, * or #")
	}
	call, err := s.controlledCall(owner, callID, lease)
	if err != nil {
		return err
	}
	s.mu.RLock()
	status, deviceID, resolvedCallID := call.view.Status, call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	if status != StatusConnected {
		return errors.New("phone: DTMF requires a connected call")
	}
	return s.gateway.SendCallDTMF(deviceID, resolvedCallID, digit)
}

func (s *Service) Hold(ctx context.Context, owner, callID, lease string) error {
	return s.setCallHold(ctx, owner, callID, lease, true)
}

func (s *Service) Resume(ctx context.Context, owner, callID, lease string) error {
	return s.setCallHold(ctx, owner, callID, lease, false)
}

func (s *Service) Switch(owner, callID, lease string) error {
	call, err := s.controlledCall(owner, callID, lease)
	if err != nil {
		return err
	}
	s.mu.RLock()
	deviceID, resolvedCallID := call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	return s.gateway.SwitchCall(deviceID, resolvedCallID)
}

func (s *Service) setCallHold(ctx context.Context, owner, callID, lease string, hold bool) error {
	call, err := s.controlledCall(owner, callID, lease)
	if err != nil {
		return err
	}
	s.mu.RLock()
	status, deviceID, resolvedCallID := call.view.Status, call.view.DeviceID, call.view.CallID
	s.mu.RUnlock()
	if status != StatusConnected {
		return errors.New("phone: hold requires a connected call")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if hold {
		err = s.gateway.HoldCall(ctx, deviceID, resolvedCallID)
	} else {
		err = s.gateway.ResumeCall(ctx, deviceID, resolvedCallID)
	}
	if err != nil {
		return mapHoldError(err)
	}
	s.mu.Lock()
	if current := s.calls[resolvedCallID]; current != nil && !current.terminal {
		current.view.Held = hold
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) claimIncoming(request ControlRequest) (*activeCall, *MediaSession, *mediaReservation, error) {
	media, reservation, err := s.reserveControlledMedia(request.Owner, request.MediaID, request.Lease)
	if err != nil {
		return nil, nil, nil, err
	}
	s.mu.Lock()
	call := s.calls[request.CallID]
	if call == nil || call.terminal || call.view.Direction != "inbound" || call.controlPending {
		s.mu.Unlock()
		s.releaseMediaReservation(reservation)
		return nil, nil, nil, errors.New("phone: pending incoming call not found")
	}
	if call.lease != "" && (!secureEqual(call.lease, request.Lease) || call.owner != request.Owner) {
		s.mu.Unlock()
		s.releaseMediaReservation(reservation)
		return nil, nil, nil, errors.New("phone: call is controlled by another browser")
	}
	call.controlPending = true
	if call.autoAnswerTimer != nil {
		call.autoAnswerTimer.Stop()
		call.autoAnswerTimer = nil
	}
	s.mu.Unlock()
	return call, media, reservation, nil
}

func (s *Service) releaseIncomingClaim(call *activeCall) {
	if call == nil {
		return
	}
	s.mu.Lock()
	if current := s.calls[call.view.CallID]; current == call {
		call.controlPending = false
	}
	s.mu.Unlock()
}

func (s *Service) controlledCall(owner, callID, lease string) (*activeCall, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	call := s.calls[strings.TrimSpace(callID)]
	if call == nil || call.terminal {
		return nil, errActiveCallNotFound
	}
	if call.owner != owner || !secureEqual(call.lease, lease) {
		return nil, errors.New("phone: control lease does not own this call")
	}
	return call, nil
}

func (s *Service) reserveDevice(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.deviceCalls[deviceID]; busy {
		return fmt.Errorf("phone: device %s already has an active call", deviceID)
	}
	s.deviceCalls[deviceID] = ""
	return nil
}

func (s *Service) releaseDeviceReservation(deviceID string) {
	s.mu.Lock()
	if s.deviceCalls[deviceID] == "" {
		delete(s.deviceCalls, deviceID)
	}
	s.mu.Unlock()
}

func (s *Service) assignControl(callID string, reservation *mediaReservation) error {
	s.mu.Lock()
	call := s.calls[callID]
	if call == nil || call.terminal {
		s.mu.Unlock()
		return errActiveCallNotFound
	}
	pendingMediaDrop, err := s.bindMediaLocked(callID, reservation)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	call.owner, call.lease, call.mediaID = reservation.media.Owner, reservation.media.Lease, reservation.mediaID
	call.view.MediaID = reservation.mediaID
	s.mu.Unlock()
	s.resumePendingMediaDrop(reservation.mediaID, pendingMediaDrop)
	return nil
}

func mapHoldError(err error) error {
	if errors.Is(err, voicehost.ErrHoldNotAligned) {
		return ErrHoldUnavailable
	}
	return err
}

func (s *Service) iccid(deviceID string) string {
	if s.resolveICCID == nil {
		return ""
	}
	return strings.TrimSpace(s.resolveICCID(deviceID))
}
