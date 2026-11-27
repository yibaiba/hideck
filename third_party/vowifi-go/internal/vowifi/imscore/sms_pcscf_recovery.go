package imscore

import (
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const (
	vodafoneUKMTReportFailure        = "mt_report_488"
	vodafoneUKMTReportRecoveryPolicy = "vodafone_uk_mt_report_488"
)

func (s *Service) settleMTReportRecoveryAfterRegister() bool {
	if s == nil || s.registrarPenalties == nil {
		return false
	}
	s.mu.RLock()
	attempt := s.registrarRecoveryAttempt
	s.mu.RUnlock()
	return s.registrarPenalties.settleMTReportRecoveryAfterRegister(attempt)
}

func (s *Service) triggerMTReportPCSCFRecovery(reportErr error) {
	status := rpReportRejectStatus(reportErr)
	registrar := rpReportRejectRegistrar(reportErr)
	if s == nil || status != 488 || registrar == "" ||
		!usesVodafoneUKPortSResetRecovery(s.cfg) || s.stopped() {
		return
	}
	penalty := s.markVodafoneRegistrarFailure(registrar, vodafoneUKMTReportFailure, nil)
	if !s.pcscfRecoveryPending.CompareAndSwap(false, true) {
		return
	}
	if !s.startPCSCFRecovery(func() { s.recoverPCSCFAfterMTReportReject(registrar, status, penalty.deprioritizedUntil) }) {
		s.finishPCSCFRecovery()
	}
}

func (s *Service) recoverPCSCFAfterMTReportReject(
	registrar string,
	status int,
	unavailableUntil time.Time,
) {
	defer s.finishPCSCFRecovery()
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	if s.stopped() {
		return
	}
	current := s.currentPortSRecoveryRegistrar()
	if s.stopped() {
		return
	}
	if !strings.EqualFold(current, registrar) {
		logging.Info("IMS MT report rejection belongs to an earlier P-CSCF path",
			"device", s.DeviceID(), "policy", vodafoneUKMTReportRecoveryPolicy,
			"rejected_pcscf", registrar, "current_pcscf", current,
			"deprioritized_until", unavailableUntil)
		return
	}
	next := s.selectMTReportAlternate(registrar)
	cause := portSFailoverCause{
		reason: vodafoneUKMTReportFailure, observedAt: time.Now(), deprioritizedUntil: unavailableUntil,
	}
	if next != "" {
		s.recoverPortSOnAlternate(registrar, next, cause)
		return
	}
	reason := fmt.Sprintf("P-CSCF path %s rejected the MT SMS RP report with SIP %d", registrar, status)
	s.markPCSCFRegistrationUnboundWithReason(reason, int32(status), SIPStatusText(status))
	err := fmt.Errorf("imscore: %s; fresh runtime required", reason)
	logging.WarnRate("ims-mt-report-pcscf-recovery-"+s.DeviceID()+"-"+registrar, 30*time.Second,
		"IMS MT report rejection requires a fresh P-CSCF path",
		"device", s.DeviceID(), "policy", vodafoneUKMTReportRecoveryPolicy,
		"pcscf", registrar, "status", status,
		"deprioritized_until", unavailableUntil)
	s.reportRegistrationRuntimeError(err)
}

// Keep hard report rejections in the shared downlink round so a later 488 on
// another node cannot select an earlier rejected P-CSCF before rediscovery.
func (s *Service) selectMTReportAlternate(registrar string) string {
	s.registrarPenalties.noteMTReportFailureAttempt(registrar, time.Now())
	s.mu.RLock()
	candidates := append([]string(nil), s.registrarCandidates...)
	s.mu.RUnlock()
	plan := s.planDownlinkRound(candidates, registrar)
	if plan.next == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.EqualFold(strings.TrimSpace(s.registrar), strings.TrimSpace(registrar)) ||
		!s.selectDownlinkAlternateLocked(plan.next) {
		return ""
	}
	return plan.next
}
