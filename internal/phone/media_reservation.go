package phone

import (
	"errors"
	"strings"
)

type mediaReservationRequest struct {
	owner        string
	mediaID      string
	lease        string
	requireLease bool
}

type mediaReservation struct {
	mediaID string
	media   *MediaSession
}

func (s *Service) reserveControlledMedia(owner, mediaID, lease string) (*MediaSession, *mediaReservation, error) {
	return s.reserveMedia(mediaReservationRequest{
		owner: owner, mediaID: mediaID, lease: lease, requireLease: true,
	})
}

func (s *Service) reserveOwnedMedia(owner, mediaID string) (*MediaSession, *mediaReservation, error) {
	return s.reserveMedia(mediaReservationRequest{owner: owner, mediaID: mediaID})
}

func (s *Service) reserveMedia(request mediaReservationRequest) (*MediaSession, *mediaReservation, error) {
	mediaID := strings.TrimSpace(request.mediaID)
	s.mu.Lock()
	defer s.mu.Unlock()
	media := s.media.Get(mediaID)
	if media == nil || media.Owner != request.owner {
		if !request.requireLease {
			return nil, nil, errors.New("phone: media session is unavailable")
		}
		return nil, nil, errors.New("phone: invalid media control lease")
	}
	if request.requireLease && !media.Matches(request.owner, request.lease) {
		return nil, nil, errors.New("phone: invalid media control lease")
	}
	if s.mediaCalls[mediaID] != "" || s.mediaReservations[mediaID] != nil {
		return nil, nil, errors.New("phone: media session already has an active call operation")
	}
	reservation := &mediaReservation{mediaID: mediaID, media: media}
	s.mediaReservations[mediaID] = reservation
	s.pausePendingMediaDropLocked(mediaID)
	return media, reservation, nil
}

func (s *Service) releaseMediaReservation(reservation *mediaReservation) {
	if reservation == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mediaReservations[reservation.mediaID] != reservation {
		return
	}
	delete(s.mediaReservations, reservation.mediaID)
	s.resumeUnboundMediaDropLocked(reservation.mediaID)
}

func (s *Service) bindMediaLocked(callID string, reservation *mediaReservation) (bool, error) {
	if reservation == nil || s.mediaReservations[reservation.mediaID] != reservation {
		return false, errors.New("phone: media reservation is no longer active")
	}
	if s.media.Get(reservation.mediaID) != reservation.media {
		delete(s.mediaReservations, reservation.mediaID)
		return false, errors.New("phone: reserved media session is unavailable")
	}
	delete(s.mediaReservations, reservation.mediaID)
	s.mediaCalls[reservation.mediaID] = callID
	return s.clearPendingMediaDropLocked(reservation.mediaID), nil
}

// mediaUsedByOtherCallLocked reports whether a live call still binds the given
// browser media. Callers must hold s.mu.
func (s *Service) mediaUsedByOtherCallLocked(mediaID, exceptCallID string) bool {
	if mediaID == "" {
		return false
	}
	for callID, call := range s.calls {
		if callID == exceptCallID || call == nil || call.terminal {
			continue
		}
		if call.mediaID == mediaID {
			return true
		}
	}
	return false
}
