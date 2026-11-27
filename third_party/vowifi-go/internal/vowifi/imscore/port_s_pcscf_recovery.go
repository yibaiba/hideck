package imscore

import (
	"strings"
	"time"
)

const (
	vodafoneUKCarrierPresetID          = "vodafone_uk_23415"
	vodafoneUKPortSResetRecoveryPolicy = "vodafone_uk_port_s_reset"
	portSPeerResetFailure              = "port_s_peer_reset"
	vodafoneUKPortSResetReconnectGrace = 5 * time.Second
	vodafoneUKPortSReconnectGrace      = 30 * time.Second
	vodafoneUKPCSCFDeprioritizedPeriod = 30 * time.Minute
)

type portSResetRecoveryState struct {
	registrar           string
	observedAt          time.Time
	recoveryAttemptedAt time.Time
	recoverySucceeded   bool
	failoverPending     bool
}

func (s *Service) armVodafoneUKResetRecoveryLocked(registrar string, openedAt, now time.Time) bool {
	if !usesVodafoneUKPortSResetRecovery(s.cfg) || openedAt.IsZero() {
		return false
	}
	if now.Before(openedAt) {
		return false
	}
	registrar = strings.TrimSpace(registrar)
	if registrar == "" {
		return false
	}
	state := &s.portSSession.resetRecovery
	if state.registrar == registrar && !state.recoveryAttemptedAt.IsZero() &&
		!openedAt.Before(state.recoveryAttemptedAt) {
		if !state.recoverySucceeded {
			return false
		}
		state.observedAt = now
		state.failoverPending = true
		return true
	}
	if state.registrar == registrar && !state.observedAt.IsZero() {
		return false
	}
	s.portSSession.resetRecovery = portSResetRecoveryState{
		registrar: registrar, observedAt: now,
	}
	return false
}

func usesVodafoneUKPortSResetRecovery(cfg *IMSConfig) bool {
	return cfg != nil && strings.TrimSpace(cfg.CarrierPresetID) == vodafoneUKCarrierPresetID
}

func (s *Service) usesVodafoneUKPeerResetGrace() bool {
	if s == nil || !usesVodafoneUKPortSResetRecovery(s.cfg) {
		return false
	}
	s.portSSessionMu.Lock()
	defer s.portSSessionMu.Unlock()
	return s.portSSession.lastCloseKind == portSClosePeerReset
}

func (s *Service) markPortSResetRecoveryAttempt(registrar string) {
	if s == nil {
		return
	}
	s.portSSessionMu.Lock()
	defer s.portSSessionMu.Unlock()
	state := &s.portSSession.resetRecovery
	if state.registrar == strings.TrimSpace(registrar) && !state.observedAt.IsZero() {
		state.recoveryAttemptedAt = time.Now()
		state.recoverySucceeded = false
	}
}

func (s *Service) markPortSResetRecoverySucceeded(registrar string) {
	if s == nil {
		return
	}
	s.portSSessionMu.Lock()
	defer s.portSSessionMu.Unlock()
	state := &s.portSSession.resetRecovery
	if state.registrar == strings.TrimSpace(registrar) && !state.recoveryAttemptedAt.IsZero() {
		state.recoverySucceeded = true
	}
}

func (s *Service) clearPortSResetRecovery(registrar string) {
	if s == nil {
		return
	}
	s.portSSessionMu.Lock()
	defer s.portSSessionMu.Unlock()
	if registrar == "" || s.portSSession.resetRecovery.registrar == strings.TrimSpace(registrar) {
		s.portSSession.resetRecovery = portSResetRecoveryState{}
	}
}

func (s *Service) pendingPortSResetFailover() (string, time.Time, bool) {
	s.portSSessionMu.Lock()
	defer s.portSSessionMu.Unlock()
	state := s.portSSession.resetRecovery
	if !state.failoverPending || state.registrar == "" {
		return "", time.Time{}, false
	}
	s.portSSession.resetRecovery = portSResetRecoveryState{}
	return state.registrar, state.observedAt, true
}

func (s *Service) startPendingPortSResetFailover() {
	if s == nil || s.stopped() || !s.pcscfRecoveryPending.CompareAndSwap(false, true) {
		return
	}
	failed, observedAt, ok := s.pendingPortSResetFailover()
	if !ok {
		s.finishPCSCFRecovery()
		return
	}
	if !s.startPCSCFRecovery(func() { s.recoverPCSCFAfterPortSReset(failed, observedAt) }) {
		s.finishPCSCFRecovery()
	}
}

func (s *Service) recoverPCSCFAfterPortSReset(failedRegistrar string, observedAt time.Time) {
	defer s.finishPCSCFRecovery()
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	if s.stopped() {
		return
	}
	s.recoverPCSCFAfterPortSFailureLocked(failedRegistrar, portSFailoverCause{
		reason: portSPeerResetFailure, observedAt: observedAt,
	})
}
