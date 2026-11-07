package manager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

type serviceTimeoutKey struct {
	service string
	op      string
}

type serviceTimeoutWindow struct {
	first time.Time
	count int
}

func shouldRecoverServiceError(service string, err error, serviceUnavailableText string) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, qmi.ErrServiceNotSupported) {
		return false
	}

	var notReady *ServiceNotReadyError
	if errors.As(err, &notReady) && strings.EqualFold(strings.TrimSpace(notReady.Service), strings.TrimSpace(service)) {
		return true
	}

	if qe := qmi.GetQMIError(err); qe != nil {
		switch qe.ErrorCode {
		case qmi.QMIErrInvalidID, qmi.QMIErrDeviceNotReady, qmi.QMIErrClientIDsExhausted:
			return true
		}
		if qe.Service == qmi.ServiceControl && qe.MessageID == qmi.CTLGetClientID {
			return true
		}
	}

	lowerErr := strings.ToLower(err.Error())
	lowerSvc := strings.ToLower(strings.TrimSpace(service))
	needle := strings.TrimSpace(serviceUnavailableText)
	if needle == "" {
		needle = fmt.Sprintf("%s service not available", lowerSvc)
	}

	return strings.Contains(lowerErr, strings.ToLower(needle)) ||
		strings.Contains(lowerErr, "qmi 服务未就绪: "+lowerSvc) ||
		strings.Contains(lowerErr, "allocate client id request failed")
}

func isServiceTimeoutError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeoutErr *qmi.TimeoutError
	return errors.As(err, &timeoutErr)
}

func (m *Manager) shouldRecoverServiceOperationError(service string, op string, err error, serviceUnavailableText string) bool {
	if shouldRecoverServiceError(service, err, serviceUnavailableText) {
		return true
	}
	if !isServiceTimeoutError(err) {
		return false
	}
	return m.recordServiceTimeoutFailure(service, op, err)
}

func (m *Manager) recordServiceTimeoutFailure(service string, op string, err error) bool {
	if m == nil {
		return false
	}
	m.serviceTimeouts.Add(1)
	if m.cfg.RecoveryPolicy.DisableServiceTimeoutRecovery {
		return false
	}

	threshold := m.cfg.RecoveryPolicy.ServiceTimeoutThreshold
	if threshold <= 0 {
		threshold = defaultServiceTimeoutThreshold
	}
	window := m.cfg.RecoveryPolicy.ServiceTimeoutWindow
	if window <= 0 {
		window = defaultServiceTimeoutWindow
	}
	if threshold <= 1 {
		threshold = 1
	}

	now := time.Now()
	key := serviceTimeoutKey{
		service: strings.ToUpper(strings.TrimSpace(service)),
		op:      strings.TrimSpace(op),
	}
	if key.op == "" {
		key.op = "*"
	}

	m.serviceTimeoutMu.Lock()
	if m.serviceTimeoutFailures == nil {
		m.serviceTimeoutFailures = make(map[serviceTimeoutKey]serviceTimeoutWindow)
	}
	state := m.serviceTimeoutFailures[key]
	if state.first.IsZero() || now.Sub(state.first) > window {
		state = serviceTimeoutWindow{first: now}
	}
	state.count++
	m.serviceTimeoutFailures[key] = state
	reached := state.count >= threshold
	firstReached := state.count == threshold
	m.serviceTimeoutMu.Unlock()

	entry := m.log.
		WithField("service_name", key.service).
		WithField("op", key.op).
		WithField("timeout_count", state.count).
		WithField("timeout_threshold", threshold).
		WithField("timeout_window_ms", window.Milliseconds())

	m.detectTimeoutStorm(key.service)

	if reached {
		if firstReached {
			m.serviceTimeoutRecoveries.Add(1)
			entry.WithError(err).Warn("Service operation timeout threshold reached; enabling recovery")
		} else {
			entry.WithError(err).Debug("Service operation timeout remains above recovery threshold")
		}
		return true
	}
	entry.WithError(err).Debug("Service operation timeout observed below recovery threshold")
	return false
}

func (m *Manager) detectTimeoutStorm(service string) {
	const stormWindow = 5 * time.Second
	const stormMinSvcs = 2
	const stormCooldown = 30 * time.Second

	m.globalTimeoutMu.Lock()
	defer m.globalTimeoutMu.Unlock()

	now := time.Now()
	if m.globalTimeoutServices == nil {
		m.globalTimeoutServices = make(map[string]time.Time)
	}

	for svc, t := range m.globalTimeoutServices {
		if now.Sub(t) > stormWindow {
			delete(m.globalTimeoutServices, svc)
		}
	}
	m.globalTimeoutServices[service] = now

	if len(m.globalTimeoutServices) >= stormMinSvcs {
		if m.globalTimeoutStormAt.IsZero() || now.Sub(m.globalTimeoutStormAt) > stormCooldown {
			// Name the services before clearing the window; logging after the
			// reset always printed 0 (hideck#94).
			affected := make([]string, 0, len(m.globalTimeoutServices))
			for svc := range m.globalTimeoutServices {
				affected = append(affected, svc)
			}
			sort.Strings(affected)
			m.globalTimeoutStormAt = now
			m.globalTimeoutServices = make(map[string]time.Time)
			m.log.Warn("Timeout storm detected; triggering immediate core recovery",
				"services_affected", len(affected), "services", strings.Join(affected, ","))
			m.enqueueModemResetEvent("timeout_storm")
		}
	}
}

func (m *Manager) noteServiceOperationSuccess(service string, op string) {
	if m == nil {
		return
	}
	key := serviceTimeoutKey{
		service: strings.ToUpper(strings.TrimSpace(service)),
		op:      strings.TrimSpace(op),
	}
	if key.op == "" {
		key.op = "*"
	}
	m.serviceTimeoutMu.Lock()
	delete(m.serviceTimeoutFailures, key)
	m.serviceTimeoutMu.Unlock()
}

func (m *Manager) logServiceRecovery(service string, op string, phase string, err error, message string) {
	log := Logger(NewNopLogger())
	if m != nil && m.log != nil {
		log = m.log
	}
	entry := log.WithField("service_name", service).WithField("op", op).WithField("phase", phase)
	if qe := qmi.GetQMIError(err); qe != nil {
		entry = entry.
			WithField("service", fmt.Sprintf("0x%02x", qe.Service)).
			WithField("msg", fmt.Sprintf("0x%04x", qe.MessageID)).
			WithField("error_code", fmt.Sprintf("0x%04x", qe.ErrorCode))
	}
	entry.WithError(err).Warn(message)
}

func (m *Manager) triggerCoreRecoveryFromService(service string, op string, phase string, cause error) bool {
	if m == nil {
		return false
	}

	m.mu.RLock()
	coreReady := m.coreReady
	stopping := m.state == StateStopping
	m.mu.RUnlock()
	if !coreReady || stopping {
		return false
	}

	cooldown := m.uimRecoverCooldown
	if cooldown <= 0 {
		cooldown = defaultUIMRecoverCooldown
	}

	now := time.Now()
	m.uimRecoveryMu.Lock()
	if !m.uimLastRecoverSignal.IsZero() && now.Sub(m.uimLastRecoverSignal) < cooldown {
		m.uimRecoveryMu.Unlock()
		m.log.
			WithField("service_name", service).
			WithField("op", op).
			WithField("phase", phase).
			Debug("Skip core recovery trigger due to cooldown")
		return false
	}
	m.uimLastRecoverSignal = now
	m.uimRecoveryMu.Unlock()

	m.logServiceRecovery(service, op, "recover-core", cause, "Scheduling core recovery due to service failure")
	m.enqueueModemResetEvent(strings.ToLower(service) + "_recovery")
	return true
}

// RequestCoreRecovery asks the manager to run the same core recovery path used
// for modem reset/service-failure handling. It is intended for higher-level
// flows that have already classified a service stall, such as post-eSIM-switch
// convergence.
func (m *Manager) RequestCoreRecovery(reason string) bool {
	if m == nil {
		return false
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "external_request"
	}

	m.mu.RLock()
	coreReady := m.coreReady
	stopping := m.state == StateStopping
	m.mu.RUnlock()
	if !coreReady || stopping {
		return false
	}

	cause := fmt.Errorf("%s", reason)
	m.logServiceRecovery("POST_SWITCH", reason, "recover-core", cause, "Scheduling core recovery due to explicit request")
	m.enqueueModemResetEvent("post_switch_recovery")
	return true
}

func (m *Manager) maybeReplayWMSStateAfterRebind(reason string) {
	m.wmsReplayMu.Lock()
	if m.wmsReplayInProgress {
		m.wmsReplayMu.Unlock()
		return
	}
	m.wmsReplayInProgress = true
	m.wmsReplayMu.Unlock()
	defer func() {
		m.wmsReplayMu.Lock()
		m.wmsReplayInProgress = false
		m.wmsReplayMu.Unlock()
	}()

	if m.onWMSRebindReplayHook != nil {
		m.onWMSRebindReplayHook(reason)
		return
	}

	m.log.WithField("reason", reason).Info("Replaying WMS readiness state after WMS rebind")
	m.recoverWMSState()
}

func (m *Manager) withDMSRecovery(op string, fn func(dms *qmi.DMSService) error) error {
	_, err := withDMSRecoveryValue(m, op, func(dms *qmi.DMSService) (struct{}, error) {
		return struct{}{}, fn(dms)
	})
	return err
}

func withDMSRecoveryValue[T any](m *Manager, op string, fn func(dms *qmi.DMSService) (T, error)) (T, error) {
	var zero T

	dms, err := m.ensureDMSService()
	if err != nil {
		if m.shouldRecoverDMSError(op, err) {
			m.logServiceRecovery("DMS", op, "initial", err, "DMS ensure failed (core recovery skipped)")
		}
		return zero, err
	}

	result, err := fn(dms)
	if err == nil {
		m.noteServiceOperationSuccess("DMS", op)
		return result, nil
	}
	if !m.shouldRecoverDMSError(op, err) {
		return result, err
	}

	m.logServiceRecovery("DMS", op, "initial", err, "DMS operation failed; rebinding DMS service")

	m.dmsRecoveryMu.Lock()
	dms, rebindErr := m.rebindDMSService("recover:" + op)
	m.dmsRecoveryMu.Unlock()
	if rebindErr != nil {
		m.logServiceRecovery("DMS", op, "rebind", rebindErr, "DMS service rebind failed (core recovery skipped)")
		return zero, fmt.Errorf("%s: DMS rebind failed: %w (initial=%v)", op, rebindErr, err)
	}

	retryResult, retryErr := fn(dms)
	if retryErr == nil {
		m.noteServiceOperationSuccess("DMS", op)
		m.log.WithField("service_name", "DMS").WithField("op", op).WithField("phase", "retry").Info("DMS operation recovered after rebind")
		return retryResult, nil
	}
	if m.shouldRecoverDMSError(op, retryErr) {
		m.logServiceRecovery("DMS", op, "retry", retryErr, "DMS operation still failing after rebind; escalating to core recovery")
		m.triggerCoreRecoveryFromService("DMS", op, "retry", retryErr)
	}
	return retryResult, retryErr
}

func (m *Manager) ensureDMSService() (*qmi.DMSService, error) {
	if m == nil {
		return nil, ErrServiceNotReady("DMS")
	}
	if m.ensureDMSServiceHook != nil {
		return m.ensureDMSServiceHook()
	}

	m.mu.RLock()
	dms := m.dms
	client := m.client
	m.mu.RUnlock()
	if dms != nil {
		return dms, nil
	}
	if client == nil {
		return nil, ErrServiceNotReady("DMS")
	}

	m.dmsRecoveryMu.Lock()
	defer m.dmsRecoveryMu.Unlock()

	m.mu.RLock()
	dms = m.dms
	client = m.client
	m.mu.RUnlock()
	if dms != nil {
		return dms, nil
	}
	if client == nil {
		return nil, ErrServiceNotReady("DMS")
	}

	allocated, err := qmi.NewDMSService(client)
	if err != nil {
		return nil, fmt.Errorf("allocate DMS client failed: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != client {
		_ = allocated.Close()
		return nil, ErrServiceNotReady("DMS")
	}
	m.dms = allocated
	m.log.Info("DMS service lazily allocated")
	return allocated, nil
}

func (m *Manager) rebindDMSService(reason string) (*qmi.DMSService, error) {
	if m == nil {
		return nil, ErrServiceNotReady("DMS")
	}
	if m.rebindDMSServiceHook != nil {
		return m.rebindDMSServiceHook(reason)
	}

	m.mu.Lock()
	prev := m.dms
	client := m.client
	m.dms = nil
	m.mu.Unlock()

	if prev != nil {
		if err := prev.Close(); err != nil {
			m.log.WithError(err).WithField("reason", reason).Warn("Closing previous DMS client failed during rebind")
		}
	}
	if client == nil {
		return nil, ErrServiceNotReady("DMS")
	}

	allocated, err := qmi.NewDMSService(client)
	if err != nil {
		return nil, fmt.Errorf("allocate DMS client failed: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != client {
		_ = allocated.Close()
		return nil, ErrServiceNotReady("DMS")
	}
	m.dms = allocated
	m.log.WithField("reason", reason).Info("DMS service rebound")
	return allocated, nil
}

func (m *Manager) shouldRecoverDMSError(op string, err error) bool {
	return m.shouldRecoverServiceOperationError("DMS", op, err, "dms service not available")
}

func (m *Manager) withNASRecovery(op string, fn func(nas *qmi.NASService) error) error {
	_, err := withNASRecoveryValue(m, op, func(nas *qmi.NASService) (struct{}, error) {
		return struct{}{}, fn(nas)
	})
	return err
}

func withNASRecoveryValue[T any](m *Manager, op string, fn func(nas *qmi.NASService) (T, error)) (T, error) {
	var zero T

	nas, err := m.ensureNASService()
	if err != nil {
		if m.shouldRecoverNASError(op, err) {
			m.triggerCoreRecoveryFromService("NAS", op, "initial", err)
		}
		return zero, err
	}

	result, err := fn(nas)
	if err == nil {
		m.noteServiceOperationSuccess("NAS", op)
		return result, nil
	}
	if !m.shouldRecoverNASError(op, err) {
		return result, err
	}

	m.logServiceRecovery("NAS", op, "initial", err, "NAS operation failed; rebinding NAS service")

	m.nasRecoveryMu.Lock()
	nas, rebindErr := m.rebindNASService("recover:" + op)
	m.nasRecoveryMu.Unlock()
	if rebindErr != nil {
		m.logServiceRecovery("NAS", op, "rebind", rebindErr, "NAS service rebind failed")
		m.triggerCoreRecoveryFromService("NAS", op, "rebind", rebindErr)
		return zero, fmt.Errorf("%s: NAS rebind failed: %w (initial=%v)", op, rebindErr, err)
	}

	retryResult, retryErr := fn(nas)
	if retryErr == nil {
		m.noteServiceOperationSuccess("NAS", op)
		m.log.WithField("service_name", "NAS").WithField("op", op).WithField("phase", "retry").Info("NAS operation recovered after rebind")
		return retryResult, nil
	}
	if m.shouldRecoverNASError(op, retryErr) {
		m.logServiceRecovery("NAS", op, "retry", retryErr, "NAS operation still failing after rebind")
		m.triggerCoreRecoveryFromService("NAS", op, "retry", retryErr)
	}
	return retryResult, retryErr
}

func (m *Manager) ensureNASService() (*qmi.NASService, error) {
	if m == nil {
		return nil, ErrServiceNotReady("NAS")
	}
	if m.ensureNASServiceHook != nil {
		return m.ensureNASServiceHook()
	}

	m.mu.RLock()
	nas := m.nas
	client := m.client
	m.mu.RUnlock()
	if nas != nil {
		return nas, nil
	}
	if client == nil {
		return nil, ErrServiceNotReady("NAS")
	}

	m.nasRecoveryMu.Lock()
	defer m.nasRecoveryMu.Unlock()

	m.mu.RLock()
	nas = m.nas
	client = m.client
	m.mu.RUnlock()
	if nas != nil {
		return nas, nil
	}
	if client == nil {
		return nil, ErrServiceNotReady("NAS")
	}

	allocated, err := qmi.NewNASService(client)
	if err != nil {
		return nil, fmt.Errorf("allocate NAS client failed: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != client {
		_ = allocated.Close()
		return nil, ErrServiceNotReady("NAS")
	}
	m.nas = allocated
	m.log.Info("NAS service lazily allocated")
	return allocated, nil
}

func (m *Manager) rebindNASService(reason string) (*qmi.NASService, error) {
	if m == nil {
		return nil, ErrServiceNotReady("NAS")
	}
	if m.rebindNASServiceHook != nil {
		return m.rebindNASServiceHook(reason)
	}

	m.mu.Lock()
	prev := m.nas
	client := m.client
	m.nas = nil
	m.mu.Unlock()

	if prev != nil {
		if err := prev.Close(); err != nil {
			m.log.WithError(err).WithField("reason", reason).Warn("Closing previous NAS client failed during rebind")
		}
	}
	if client == nil {
		return nil, ErrServiceNotReady("NAS")
	}

	allocated, err := qmi.NewNASService(client)
	if err != nil {
		return nil, fmt.Errorf("allocate NAS client failed: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != client {
		_ = allocated.Close()
		return nil, ErrServiceNotReady("NAS")
	}
	m.nas = allocated
	m.log.WithField("reason", reason).Info("NAS service rebound")
	return allocated, nil
}

func (m *Manager) shouldRecoverNASError(op string, err error) bool {
	return m.shouldRecoverServiceOperationError("NAS", op, err, "nas service not available")
}

func (m *Manager) withWMSRecovery(op string, fn func(wms *qmi.WMSService) error) error {
	_, err := withWMSRecoveryValue(m, op, func(wms *qmi.WMSService) (struct{}, error) {
		return struct{}{}, fn(wms)
	})
	return err
}

func withWMSRecoveryValue[T any](m *Manager, op string, fn func(wms *qmi.WMSService) (T, error)) (T, error) {
	var zero T

	wms, err := m.ensureWMSService()
	if err != nil {
		if m.shouldRecoverWMSError(op, err) {
			m.logServiceRecovery("WMS", op, "initial", err, "WMS ensure failed (core recovery skipped)")
		}
		return zero, err
	}

	result, err := fn(wms)
	if err == nil {
		m.noteServiceOperationSuccess("WMS", op)
		return result, nil
	}
	if !m.shouldRecoverWMSError(op, err) {
		return result, err
	}

	m.logServiceRecovery("WMS", op, "initial", err, "WMS operation failed; rebinding WMS service")

	m.wmsRecoveryMu.Lock()
	wms, rebindErr := m.rebindWMSService("recover:" + op)
	m.wmsRecoveryMu.Unlock()
	if rebindErr != nil {
		m.logServiceRecovery("WMS", op, "rebind", rebindErr, "WMS service rebind failed (core recovery skipped)")
		return zero, fmt.Errorf("%s: WMS rebind failed: %w (initial=%v)", op, rebindErr, err)
	}

	retryResult, retryErr := fn(wms)
	if retryErr == nil {
		m.noteServiceOperationSuccess("WMS", op)
		m.log.WithField("service_name", "WMS").WithField("op", op).WithField("phase", "retry").Info("WMS operation recovered after rebind")
		return retryResult, nil
	}
	if m.shouldRecoverWMSError(op, retryErr) {
		m.logServiceRecovery("WMS", op, "retry", retryErr, "WMS operation still failing after rebind (core recovery skipped)")
	}
	return retryResult, retryErr
}

func (m *Manager) ensureWMSService() (*qmi.WMSService, error) {
	if m == nil {
		return nil, ErrServiceNotReady("WMS")
	}
	if m.ensureWMSServiceHook != nil {
		return m.ensureWMSServiceHook()
	}

	m.mu.RLock()
	wms := m.wms
	client := m.client
	m.mu.RUnlock()
	if wms != nil {
		return wms, nil
	}
	if client == nil {
		return nil, ErrServiceNotReady("WMS")
	}

	m.wmsRecoveryMu.Lock()
	defer m.wmsRecoveryMu.Unlock()

	m.mu.RLock()
	wms = m.wms
	client = m.client
	m.mu.RUnlock()
	if wms != nil {
		return wms, nil
	}
	if client == nil {
		return nil, ErrServiceNotReady("WMS")
	}

	allocated, err := qmi.NewWMSService(client)
	if err != nil {
		return nil, fmt.Errorf("allocate WMS client failed: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != client {
		_ = allocated.Close()
		return nil, ErrServiceNotReady("WMS")
	}
	m.wms = allocated
	m.log.Info("WMS service lazily allocated")
	return allocated, nil
}

func (m *Manager) rebindWMSService(reason string) (*qmi.WMSService, error) {
	if m == nil {
		return nil, ErrServiceNotReady("WMS")
	}
	if m.rebindWMSServiceHook != nil {
		rebound, err := m.rebindWMSServiceHook(reason)
		if err == nil && rebound != nil {
			m.maybeReplayWMSStateAfterRebind(reason)
		}
		return rebound, err
	}

	m.mu.Lock()
	prev := m.wms
	client := m.client
	m.wms = nil
	m.mu.Unlock()

	if prev != nil {
		if err := prev.Close(); err != nil {
			m.log.WithError(err).WithField("reason", reason).Warn("Closing previous WMS client failed during rebind")
		}
	}
	if client == nil {
		return nil, ErrServiceNotReady("WMS")
	}

	allocated, err := qmi.NewWMSService(client)
	if err != nil {
		return nil, fmt.Errorf("allocate WMS client failed: %w", err)
	}

	m.mu.Lock()
	if m.client != client {
		m.mu.Unlock()
		_ = allocated.Close()
		return nil, ErrServiceNotReady("WMS")
	}
	m.wms = allocated
	m.mu.Unlock()

	m.log.WithField("reason", reason).Info("WMS service rebound")
	m.maybeReplayWMSStateAfterRebind(reason)
	return allocated, nil
}

func (m *Manager) shouldRecoverWMSError(op string, err error) bool {
	return m.shouldRecoverServiceOperationError("WMS", op, err, "wms service not available")
}

func (m *Manager) withVOICERecovery(op string, fn func(voice *qmi.VOICEService) error) error {
	_, err := withVOICERecoveryValue(m, op, func(voice *qmi.VOICEService) (struct{}, error) {
		return struct{}{}, fn(voice)
	})
	return err
}

func withVOICERecoveryValue[T any](m *Manager, op string, fn func(voice *qmi.VOICEService) (T, error)) (T, error) {
	var zero T

	voice, err := m.ensureVOICEService()
	if err != nil {
		if m.shouldRecoverVOICEError(op, err) {
			m.logServiceRecovery("VOICE", op, "initial", err, "VOICE ensure failed (core recovery skipped)")
		}
		return zero, err
	}

	result, err := fn(voice)
	if err == nil {
		m.noteServiceOperationSuccess("VOICE", op)
		return result, nil
	}
	if !m.shouldRecoverVOICEError(op, err) {
		return result, err
	}

	m.logServiceRecovery("VOICE", op, "initial", err, "VOICE operation failed; rebinding VOICE service")

	m.voiceRecoveryMu.Lock()
	voice, rebindErr := m.rebindVOICEService("recover:" + op)
	m.voiceRecoveryMu.Unlock()
	if rebindErr != nil {
		m.logServiceRecovery("VOICE", op, "rebind", rebindErr, "VOICE service rebind failed (core recovery skipped)")
		return zero, fmt.Errorf("%s: VOICE rebind failed: %w (initial=%v)", op, rebindErr, err)
	}

	retryResult, retryErr := fn(voice)
	if retryErr == nil {
		m.noteServiceOperationSuccess("VOICE", op)
		m.log.WithField("service_name", "VOICE").WithField("op", op).WithField("phase", "retry").Info("VOICE operation recovered after rebind")
		return retryResult, nil
	}
	if m.shouldRecoverVOICEError(op, retryErr) {
		m.logServiceRecovery("VOICE", op, "retry", retryErr, "VOICE operation still failing after rebind (core recovery skipped)")
	}
	return retryResult, retryErr
}

func (m *Manager) ensureVOICEService() (*qmi.VOICEService, error) {
	if m == nil {
		return nil, ErrServiceNotReady("VOICE")
	}
	if m.ensureVOICEServiceHook != nil {
		return m.ensureVOICEServiceHook()
	}

	m.mu.RLock()
	voice := m.voice
	client := m.client
	m.mu.RUnlock()
	if voice != nil {
		return voice, nil
	}
	if client == nil {
		return nil, ErrServiceNotReady("VOICE")
	}

	m.voiceRecoveryMu.Lock()
	defer m.voiceRecoveryMu.Unlock()

	m.mu.RLock()
	voice = m.voice
	client = m.client
	m.mu.RUnlock()
	if voice != nil {
		return voice, nil
	}
	if client == nil {
		return nil, ErrServiceNotReady("VOICE")
	}

	allocated, err := qmi.NewVOICEService(client)
	if err != nil {
		return nil, fmt.Errorf("allocate VOICE client failed: %w", err)
	}
	if err := m.restoreVOICEIndications(allocated); err != nil {
		_ = allocated.Close()
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != client {
		_ = allocated.Close()
		return nil, ErrServiceNotReady("VOICE")
	}
	m.voice = allocated
	m.log.Info("VOICE service lazily allocated")
	return allocated, nil
}

// registerVOICEIndicationsOnService deliberately bypasses VOICE recovery: it
// is called while voiceRecoveryMu is held and recursively recovering here
// would deadlock.
func (m *Manager) registerVOICEIndicationsOnService(ctx context.Context, voice *qmi.VOICEService, cfg qmi.VoiceIndicationRegistration) error {
	if m.registerVOICEIndications != nil {
		return m.registerVOICEIndications(ctx, cfg)
	}
	if voice == nil {
		return ErrServiceNotReady("VOICE")
	}
	return voice.IndicationRegister(ctx, cfg)
}

func (m *Manager) restoreVOICEIndications(voice *qmi.VOICEService) error {
	cfg, enabled := m.voiceIndicationRegistration()
	if !enabled {
		return nil
	}
	ctx, cancel := contextWithMaxTimeout(context.Background(), m.cfg.Timeouts.IndicationRegister)
	defer cancel()
	if err := m.registerVOICEIndicationsOnService(ctx, voice, cfg); err != nil {
		return fmt.Errorf("restore VOICE indications: %w", err)
	}
	return nil
}

func (m *Manager) rebindVOICEService(reason string) (*qmi.VOICEService, error) {
	if m == nil {
		return nil, ErrServiceNotReady("VOICE")
	}
	if m.rebindVOICEServiceHook != nil {
		return m.rebindVOICEServiceHook(reason)
	}

	m.mu.Lock()
	prev := m.voice
	client := m.client
	m.voice = nil
	m.mu.Unlock()

	if prev != nil {
		if err := prev.Close(); err != nil {
			m.log.WithError(err).WithField("reason", reason).Warn("Closing previous VOICE client failed during rebind")
		}
	}
	if client == nil {
		return nil, ErrServiceNotReady("VOICE")
	}

	allocated, err := qmi.NewVOICEService(client)
	if err != nil {
		return nil, fmt.Errorf("allocate VOICE client failed: %w", err)
	}
	if err := m.restoreVOICEIndications(allocated); err != nil {
		_ = allocated.Close()
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != client {
		_ = allocated.Close()
		return nil, ErrServiceNotReady("VOICE")
	}
	m.voice = allocated
	m.log.WithField("reason", reason).Info("VOICE service rebound")
	return allocated, nil
}

func (m *Manager) shouldRecoverVOICEError(op string, err error) bool {
	if op == "VOICEEndCall" && qmi.VoiceCallAlreadyGone(err) {
		return false
	}
	return m.shouldRecoverServiceOperationError("VOICE", op, err, "voice service not available")
}
