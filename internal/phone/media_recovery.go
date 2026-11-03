package phone

import "errors"

func (s *Service) RefreshMedia(request RefreshRequest) (CallView, string, error) {
	s.mu.RLock()
	current := s.calls[request.CallID]
	s.mu.RUnlock()
	if current == nil {
		return CallView{}, "", errActiveCallNotFound
	}
	// Serialize ownership checks, backend updates, binding and old-session release.
	current.mediaRefresh.Lock()
	defer current.mediaRefresh.Unlock()
	media, reservation, err := s.reserveOwnedMedia(request.Owner, request.MediaID)
	if err != nil {
		return CallView{}, "", err
	}
	defer s.releaseMediaReservation(reservation)
	request.MediaID = reservation.mediaID
	s.mu.RLock()
	call := s.calls[request.CallID]
	if call != current || call.terminal {
		s.mu.RUnlock()
		return CallView{}, "", errActiveCallNotFound
	}
	if !request.Takeover && !secureEqual(call.lease, request.Lease) {
		s.mu.RUnlock()
		return CallView{}, "", errors.New("phone: control lease does not own this call")
	}
	s.mu.RUnlock()
	if err := s.attachCurrentMedia(request.CallID, media); err != nil {
		return CallView{}, "", err
	}
	return s.finishMediaRefresh(current, request, reservation)
}

func (s *Service) finishMediaRefresh(current *activeCall, request RefreshRequest, reservation *mediaReservation) (CallView, string, error) {
	media := reservation.media
	s.mu.Lock()
	call := s.calls[request.CallID]
	if call != current || call.terminal {
		s.mu.Unlock()
		return CallView{}, "", errors.New("phone: active call ended while refreshing media")
	}
	if !request.Takeover && !secureEqual(call.lease, request.Lease) {
		s.mu.Unlock()
		return CallView{}, "", errors.New("phone: call control changed while refreshing media")
	}
	oldMediaID := call.mediaID
	pendingMediaDrop, err := s.bindMediaLocked(request.CallID, reservation)
	if err != nil {
		s.mu.Unlock()
		return CallView{}, "", err
	}
	call.owner, call.lease, call.mediaID = request.Owner, media.Lease, request.MediaID
	call.view.MediaID = request.MediaID
	delete(s.mediaCalls, oldMediaID)
	s.clearPendingMediaDropLocked(oldMediaID)
	if call.disconnectTimer != nil {
		call.disconnectTimer.Stop()
		call.disconnectTimer = nil
	}
	connected := call.view.Status == StatusConnected
	s.mu.Unlock()
	if connected {
		s.startMixedRecording(call, media)
	}
	s.resumePendingMediaDrop(request.MediaID, pendingMediaDrop)
	if oldMediaID != "" && oldMediaID != request.MediaID {
		s.media.Remove(oldMediaID)
	}
	s.publish("media_recovered", call)
	return s.callView(request.CallID, media.Lease), media.Lease, nil
}

func (s *Service) attachCurrentMedia(callID string, media *MediaSession) error {
	s.mu.RLock()
	call := s.calls[callID]
	if call == nil {
		s.mu.RUnlock()
		return errActiveCallNotFound
	}
	remoteSDP, deviceID, recorder := call.incomingSDP, call.view.DeviceID, call.mixedRecorder
	s.mu.RUnlock()
	if snapshot := s.gateway.ActiveCall(deviceID); snapshot != nil && snapshot.CallID == callID && snapshot.ClientSDP != "" {
		remoteSDP = snapshot.ClientSDP
	}
	if remoteSDP == "" {
		return errors.New("phone: call media endpoint is not negotiated yet")
	}
	if err := media.Attach(remoteSDP); err != nil {
		return err
	}
	media.SetRecorder(recorder)
	if updater, ok := s.gateway.(interface {
		UpdateCallMedia(string, string, string) error
	}); ok {
		if err := updater.UpdateCallMedia(deviceID, callID, media.PlainSDP()); err != nil {
			media.SetRecorder(nil)
			return err
		}
	}
	return nil
}
