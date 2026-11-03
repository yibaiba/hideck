package manager

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

func newRecoveryTestManager() *Manager {
	return &Manager{
		log:                   NewNopLogger(),
		events:                NewEventEmitter(),
		eventCh:               make(chan internalEvent, 8),
		scheduledTimers:       make(map[*time.Timer]struct{}),
		modemResetDedupWindow: defaultModemResetDedupWindow,
		uimRecoverCooldown:    defaultUIMRecoverCooldown,
	}
}

func waitInternalRecoveryEvent(t *testing.T, ch <-chan internalEvent, timeout time.Duration) internalEvent {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for internal event after %s", timeout)
		return 0
	}
}

func TestServiceNotSupportedDoesNotTriggerRecovery(t *testing.T) {
	err := fmt.Errorf("allocate WMS: %w", qmi.ErrServiceNotSupported)
	if shouldRecoverServiceError("WMS", err, qmi.ErrServiceNotSupported.Error()) {
		t.Fatal("ErrServiceNotSupported should not trigger service recovery")
	}
}

func recoverableQMIError(service uint8, msg uint16) error {
	return &qmi.QMIError{
		Service:   service,
		MessageID: msg,
		Result:    0x0001,
		ErrorCode: qmi.QMIErrInvalidID,
	}
}

func ctlGetClientIDFailure() error {
	return &qmi.QMIError{
		Service:   qmi.ServiceControl,
		MessageID: qmi.CTLGetClientID,
		Result:    0x0001,
		ErrorCode: qmi.QMIErrClientIDsExhausted,
	}
}

func TestServiceTimeoutBelowThresholdDoesNotRecover(t *testing.T) {
	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{
		RecoveryPolicy: RecoveryPolicy{
			ServiceTimeoutThreshold: 3,
			ServiceTimeoutWindow:    time.Minute,
		},
	})
	m.ensureDMSServiceHook = func() (*qmi.DMSService, error) { return &qmi.DMSService{}, nil }
	rebindCalls := 0
	m.rebindDMSServiceHook = func(reason string) (*qmi.DMSService, error) {
		rebindCalls++
		return &qmi.DMSService{}, nil
	}

	for i := 0; i < 2; i++ {
		err := m.withDMSRecovery("DMS.TimeoutOp", func(dms *qmi.DMSService) error {
			return context.DeadlineExceeded
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d error=%v, want context deadline exceeded", i+1, err)
		}
	}
	if rebindCalls != 0 {
		t.Fatalf("expected no rebind below timeout threshold, got %d", rebindCalls)
	}
	select {
	case evt := <-m.eventCh:
		t.Fatalf("expected no core recovery event below timeout threshold, got %v", evt)
	default:
	}
	stats := m.Stats()
	if stats.ServiceTimeouts != 2 {
		t.Fatalf("expected service_timeouts=2, got %d", stats.ServiceTimeouts)
	}
	if stats.ServiceTimeoutRecoveries != 0 {
		t.Fatalf("expected service_timeout_recoveries=0, got %d", stats.ServiceTimeoutRecoveries)
	}
}

func TestDMSServiceTimeoutThresholdRebindsWithoutCoreRecovery(t *testing.T) {
	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{
		RecoveryPolicy: RecoveryPolicy{
			ServiceTimeoutThreshold: 3,
			ServiceTimeoutWindow:    time.Minute,
			ServiceRecoverCooldown:  10 * time.Millisecond,
		},
	})
	m.coreReady = true
	m.state = StateDisconnected
	m.ensureDMSServiceHook = func() (*qmi.DMSService, error) { return &qmi.DMSService{}, nil }
	rebindCalls := 0
	m.rebindDMSServiceHook = func(reason string) (*qmi.DMSService, error) {
		rebindCalls++
		if reason != "recover:DMS.TimeoutOp" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		return &qmi.DMSService{}, nil
	}

	for i := 0; i < 3; i++ {
		err := m.withDMSRecovery("DMS.TimeoutOp", func(dms *qmi.DMSService) error {
			return context.DeadlineExceeded
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d error=%v, want context deadline exceeded", i+1, err)
		}
	}
	if rebindCalls != 1 {
		t.Fatalf("expected one rebind at timeout threshold, got %d", rebindCalls)
	}
	if evt := waitInternalRecoveryEvent(t, m.eventCh, time.Second); evt != eventModemReset {
		t.Fatalf("expected eventModemReset, got %v", evt)
	}
	stats := m.Stats()
	if stats.ServiceTimeouts != 4 {
		t.Fatalf("expected service_timeouts=4 including retry, got %d", stats.ServiceTimeouts)
	}
	if stats.ServiceTimeoutRecoveries != 1 {
		t.Fatalf("expected service_timeout_recoveries=1, got %d", stats.ServiceTimeoutRecoveries)
	}
}

func TestNASServiceTimeoutThresholdTriggersCoreRecovery(t *testing.T) {
	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{
		RecoveryPolicy: RecoveryPolicy{
			ServiceTimeoutThreshold: 2,
			ServiceTimeoutWindow:    time.Minute,
			ServiceRecoverCooldown:  10 * time.Millisecond,
		},
	})
	m.coreReady = true
	m.state = StateDisconnected
	m.ensureNASServiceHook = func() (*qmi.NASService, error) { return &qmi.NASService{}, nil }
	m.rebindNASServiceHook = func(reason string) (*qmi.NASService, error) {
		if reason != "recover:NAS.TimeoutOp" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		return &qmi.NASService{}, nil
	}

	for i := 0; i < 2; i++ {
		_, err := withNASRecoveryValue(m, "NAS.TimeoutOp", func(nas *qmi.NASService) (struct{}, error) {
			return struct{}{}, context.DeadlineExceeded
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d error=%v, want context deadline exceeded", i+1, err)
		}
	}
	if evt := waitInternalRecoveryEvent(t, m.eventCh, time.Second); evt != eventModemReset {
		t.Fatalf("expected eventModemReset, got %v", evt)
	}
}

func TestContextCanceledDoesNotTriggerServiceRecovery(t *testing.T) {
	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{
		RecoveryPolicy: RecoveryPolicy{
			ServiceTimeoutThreshold: 1,
			ServiceTimeoutWindow:    time.Minute,
		},
	})
	m.coreReady = true
	m.state = StateDisconnected
	m.ensureDMSServiceHook = func() (*qmi.DMSService, error) { return &qmi.DMSService{}, nil }
	rebindCalls := 0
	m.rebindDMSServiceHook = func(reason string) (*qmi.DMSService, error) {
		rebindCalls++
		return &qmi.DMSService{}, nil
	}

	err := m.withDMSRecovery("DMS.CanceledOp", func(dms *qmi.DMSService) error {
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context canceled", err)
	}
	if rebindCalls != 0 {
		t.Fatalf("expected no rebind on context canceled, got %d", rebindCalls)
	}
	if stats := m.Stats(); stats.ServiceTimeouts != 0 {
		t.Fatalf("expected no timeout stats on context canceled, got %d", stats.ServiceTimeouts)
	}
	select {
	case evt := <-m.eventCh:
		t.Fatalf("expected no core recovery event on context canceled, got %v", evt)
	default:
	}
}

func TestDMSRecoveryRebindThenRetrySuccess(t *testing.T) {
	m := newRecoveryTestManager()
	m.ensureDMSServiceHook = func() (*qmi.DMSService, error) { return &qmi.DMSService{}, nil }

	rebindCalls := 0
	m.rebindDMSServiceHook = func(reason string) (*qmi.DMSService, error) {
		rebindCalls++
		if reason != "recover:GetDeviceSerialNumbers" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		return &qmi.DMSService{}, nil
	}

	attempts := 0
	err := m.withDMSRecovery("GetDeviceSerialNumbers", func(dms *qmi.DMSService) error {
		attempts++
		if attempts == 1 {
			return recoverableQMIError(qmi.ServiceDMS, 0x0025)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected retry success, got error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if rebindCalls != 1 {
		t.Fatalf("expected 1 rebind call, got %d", rebindCalls)
	}
}

func TestDMSRecoveryRebindsWrappedSetOperatingModeError(t *testing.T) {
	m := newRecoveryTestManager()
	m.ensureDMSServiceHook = func() (*qmi.DMSService, error) { return &qmi.DMSService{}, nil }

	rebindCalls := 0
	m.rebindDMSServiceHook = func(reason string) (*qmi.DMSService, error) {
		rebindCalls++
		if reason != "recover:SetOperatingMode" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		return &qmi.DMSService{}, nil
	}

	attempts := 0
	err := m.withDMSRecovery("SetOperatingMode", func(dms *qmi.DMSService) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("set operating mode failed: %w", &qmi.QMIError{
				Service:   qmi.ServiceDMS,
				MessageID: qmi.DMSSetOperatingMode,
				Result:    0x0001,
				ErrorCode: qmi.QMIErrDeviceNotReady,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected retry success, got error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if rebindCalls != 1 {
		t.Fatalf("expected 1 rebind call, got %d", rebindCalls)
	}
}

func TestNASRecoveryRebindThenRetrySuccess(t *testing.T) {
	m := newRecoveryTestManager()
	m.ensureNASServiceHook = func() (*qmi.NASService, error) { return &qmi.NASService{}, nil }

	rebindCalls := 0
	m.rebindNASServiceHook = func(reason string) (*qmi.NASService, error) {
		rebindCalls++
		if reason != "recover:GetServingSystem" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		return &qmi.NASService{}, nil
	}

	attempts := 0
	_, err := withNASRecoveryValue(m, "GetServingSystem", func(nas *qmi.NASService) (struct{}, error) {
		attempts++
		if attempts == 1 {
			return struct{}{}, recoverableQMIError(qmi.ServiceNAS, qmi.NASGetServingSystem)
		}
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatalf("expected retry success, got error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if rebindCalls != 1 {
		t.Fatalf("expected 1 rebind call, got %d", rebindCalls)
	}
}

func TestVOICERecoveryRebindThenRetrySuccess(t *testing.T) {
	m := newRecoveryTestManager()
	m.ensureVOICEServiceHook = func() (*qmi.VOICEService, error) { return &qmi.VOICEService{}, nil }

	rebindCalls := 0
	m.rebindVOICEServiceHook = func(reason string) (*qmi.VOICEService, error) {
		rebindCalls++
		if reason != "recover:VOICEGetConfig" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		return &qmi.VOICEService{}, nil
	}

	attempts := 0
	_, err := withVOICERecoveryValue(m, "VOICEGetConfig", func(voice *qmi.VOICEService) (struct{}, error) {
		attempts++
		if attempts == 1 {
			return struct{}{}, recoverableQMIError(qmi.ServiceVOICE, qmi.VOICEGetConfig)
		}
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatalf("expected retry success, got error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if rebindCalls != 1 {
		t.Fatalf("expected 1 rebind call, got %d", rebindCalls)
	}
}

func TestRestoreVOICEIndicationsUsesRegistrationHook(t *testing.T) {
	m := newRecoveryTestManager()
	calls := 0
	m.registerVOICEIndications = func(_ context.Context, cfg qmi.VoiceIndicationRegistration) error {
		calls++
		if !cfg.CallNotificationEvents {
			t.Fatal("call notification events were not requested")
		}
		return nil
	}
	if err := m.restoreVOICEIndications(&qmi.VOICEService{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("registration calls=%d", calls)
	}
}

func TestWMSRecoveryRebindThenRetrySuccessAndReplay(t *testing.T) {
	m := newRecoveryTestManager()
	m.ensureWMSServiceHook = func() (*qmi.WMSService, error) { return &qmi.WMSService{}, nil }

	rebindCalls := 0
	m.rebindWMSServiceHook = func(reason string) (*qmi.WMSService, error) {
		rebindCalls++
		if reason != "recover:WMSGetRoutes" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		return &qmi.WMSService{}, nil
	}

	// Force replay path to run but fail softly, verifying it does not block main retry success.
	eventReportCalls := 0
	indicationCalls := 0
	smscCalls := 0
	m.registerWMSEventReport = func(_ context.Context) error {
		eventReportCalls++
		return fmt.Errorf("forced register event report failure")
	}
	m.registerWMSIndications = func(_ context.Context, _ bool) error {
		indicationCalls++
		return fmt.Errorf("forced indication register failure")
	}
	m.queryWMSRoutes = func(_ context.Context) (*qmi.WMSRouteConfig, error) {
		return nil, fmt.Errorf("forced routes query failure")
	}
	m.queryWMSTransportState = func(_ context.Context) (qmi.WMSTransportNetworkRegistration, error) {
		return 0, fmt.Errorf("forced transport query failure")
	}
	m.querySMSC = func(_ context.Context) (string, error) {
		smscCalls++
		return "", fmt.Errorf("forced smsc query failure")
	}

	attempts := 0
	err := m.withWMSRecovery("WMSGetRoutes", func(wms *qmi.WMSService) error {
		attempts++
		if attempts == 1 {
			return recoverableQMIError(qmi.ServiceWMS, qmi.WMSGetRoutes)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected retry success, got error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if rebindCalls != 1 {
		t.Fatalf("expected 1 rebind call, got %d", rebindCalls)
	}
	// replay hook path with forced failures should still execute
	if eventReportCalls == 0 && indicationCalls == 0 {
		t.Fatal("expected WMS replay path to run after rebind")
	}
	if smscCalls == 0 {
		t.Fatal("expected WMS replay to refresh SMSC diagnostics after rebind")
	}
}

func TestServiceRecoveryRebindFailureTriggersCoreRecovery(t *testing.T) {
	type tc struct {
		name               string
		expectCoreRecovery bool
		invoke             func(m *Manager) error
	}

	cases := []tc{
		{
			name:               "DMS",
			expectCoreRecovery: false,
			invoke: func(m *Manager) error {
				m.ensureDMSServiceHook = func() (*qmi.DMSService, error) { return &qmi.DMSService{}, nil }
				m.rebindDMSServiceHook = func(reason string) (*qmi.DMSService, error) { return nil, ctlGetClientIDFailure() }
				return m.withDMSRecovery("DMS.Op", func(dms *qmi.DMSService) error {
					return recoverableQMIError(qmi.ServiceDMS, 0x0025)
				})
			},
		},
		{
			name:               "NAS",
			expectCoreRecovery: true,
			invoke: func(m *Manager) error {
				m.ensureNASServiceHook = func() (*qmi.NASService, error) { return &qmi.NASService{}, nil }
				m.rebindNASServiceHook = func(reason string) (*qmi.NASService, error) { return nil, ctlGetClientIDFailure() }
				return m.withNASRecovery("NAS.Op", func(nas *qmi.NASService) error {
					return recoverableQMIError(qmi.ServiceNAS, qmi.NASGetServingSystem)
				})
			},
		},
		{
			name:               "WMS",
			expectCoreRecovery: false,
			invoke: func(m *Manager) error {
				m.ensureWMSServiceHook = func() (*qmi.WMSService, error) { return &qmi.WMSService{}, nil }
				m.rebindWMSServiceHook = func(reason string) (*qmi.WMSService, error) { return nil, ctlGetClientIDFailure() }
				return m.withWMSRecovery("WMS.Op", func(wms *qmi.WMSService) error {
					return recoverableQMIError(qmi.ServiceWMS, qmi.WMSGetRoutes)
				})
			},
		},
		{
			name:               "VOICE",
			expectCoreRecovery: false,
			invoke: func(m *Manager) error {
				m.ensureVOICEServiceHook = func() (*qmi.VOICEService, error) { return &qmi.VOICEService{}, nil }
				m.rebindVOICEServiceHook = func(reason string) (*qmi.VOICEService, error) { return nil, ctlGetClientIDFailure() }
				return m.withVOICERecovery("VOICE.Op", func(voice *qmi.VOICEService) error {
					return recoverableQMIError(qmi.ServiceVOICE, qmi.VOICEGetConfig)
				})
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newRecoveryTestManager()
			m.coreReady = true
			m.state = StateDisconnected
			err := c.invoke(m)
			if err == nil {
				t.Fatal("expected error when rebind fails")
			}
			if c.expectCoreRecovery {
				evt := waitInternalRecoveryEvent(t, m.eventCh, time.Second)
				if evt != eventModemReset {
					t.Fatalf("expected eventModemReset, got %v", evt)
				}
				return
			}
			select {
			case evt := <-m.eventCh:
				t.Fatalf("expected no core recovery event, got %v", evt)
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

func TestServiceRecoveryRetryFailureTriggersCoreRecovery(t *testing.T) {
	type tc struct {
		name               string
		expectCoreRecovery bool
		invoke             func(m *Manager) (int, error)
	}

	cases := []tc{
		{
			name:               "DMS",
			expectCoreRecovery: true,
			invoke: func(m *Manager) (int, error) {
				attempts := 0
				m.ensureDMSServiceHook = func() (*qmi.DMSService, error) { return &qmi.DMSService{}, nil }
				m.rebindDMSServiceHook = func(reason string) (*qmi.DMSService, error) { return &qmi.DMSService{}, nil }
				err := m.withDMSRecovery("DMS.Op", func(dms *qmi.DMSService) error {
					attempts++
					return &qmi.QMIError{Service: qmi.ServiceDMS, MessageID: 0x0025, Result: 0x0001, ErrorCode: qmi.QMIErrDeviceNotReady}
				})
				return attempts, err
			},
		},
		{
			name:               "NAS",
			expectCoreRecovery: true,
			invoke: func(m *Manager) (int, error) {
				attempts := 0
				m.ensureNASServiceHook = func() (*qmi.NASService, error) { return &qmi.NASService{}, nil }
				m.rebindNASServiceHook = func(reason string) (*qmi.NASService, error) { return &qmi.NASService{}, nil }
				err := m.withNASRecovery("NAS.Op", func(nas *qmi.NASService) error {
					attempts++
					return &qmi.QMIError{Service: qmi.ServiceNAS, MessageID: qmi.NASGetServingSystem, Result: 0x0001, ErrorCode: qmi.QMIErrDeviceNotReady}
				})
				return attempts, err
			},
		},
		{
			name:               "WMS",
			expectCoreRecovery: false,
			invoke: func(m *Manager) (int, error) {
				attempts := 0
				m.ensureWMSServiceHook = func() (*qmi.WMSService, error) { return &qmi.WMSService{}, nil }
				m.rebindWMSServiceHook = func(reason string) (*qmi.WMSService, error) { return &qmi.WMSService{}, nil }
				m.onWMSRebindReplayHook = func(reason string) {}
				err := m.withWMSRecovery("WMS.Op", func(wms *qmi.WMSService) error {
					attempts++
					return &qmi.QMIError{Service: qmi.ServiceWMS, MessageID: qmi.WMSGetRoutes, Result: 0x0001, ErrorCode: qmi.QMIErrDeviceNotReady}
				})
				return attempts, err
			},
		},
		{
			name:               "VOICE",
			expectCoreRecovery: false,
			invoke: func(m *Manager) (int, error) {
				attempts := 0
				m.ensureVOICEServiceHook = func() (*qmi.VOICEService, error) { return &qmi.VOICEService{}, nil }
				m.rebindVOICEServiceHook = func(reason string) (*qmi.VOICEService, error) { return &qmi.VOICEService{}, nil }
				err := m.withVOICERecovery("VOICE.Op", func(voice *qmi.VOICEService) error {
					attempts++
					return &qmi.QMIError{Service: qmi.ServiceVOICE, MessageID: qmi.VOICEGetConfig, Result: 0x0001, ErrorCode: qmi.QMIErrDeviceNotReady}
				})
				return attempts, err
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newRecoveryTestManager()
			m.coreReady = true
			m.state = StateDisconnected
			attempts, err := c.invoke(m)
			if err == nil {
				t.Fatal("expected retry failure")
			}
			if attempts != 2 {
				t.Fatalf("expected 2 attempts, got %d", attempts)
			}
			if c.expectCoreRecovery {
				evt := waitInternalRecoveryEvent(t, m.eventCh, time.Second)
				if evt != eventModemReset {
					t.Fatalf("expected eventModemReset, got %v", evt)
				}
				return
			}
			select {
			case evt := <-m.eventCh:
				t.Fatalf("expected no core recovery event, got %v", evt)
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

func TestServiceRecoveryCooldownSuppressesRepeatedCoreRecovery(t *testing.T) {
	m := newRecoveryTestManager()
	m.coreReady = true
	m.state = StateDisconnected
	m.uimRecoverCooldown = time.Hour

	m.ensureNASServiceHook = func() (*qmi.NASService, error) { return &qmi.NASService{}, nil }
	m.rebindNASServiceHook = func(reason string) (*qmi.NASService, error) { return nil, ctlGetClientIDFailure() }
	_ = m.withNASRecovery("NAS.Op", func(nas *qmi.NASService) error {
		return recoverableQMIError(qmi.ServiceNAS, qmi.NASGetServingSystem)
	})
	if evt := waitInternalRecoveryEvent(t, m.eventCh, time.Second); evt != eventModemReset {
		t.Fatalf("expected first event to be modem reset, got %v", evt)
	}

	m.ensureUIMServiceHook = func() (*qmi.UIMService, error) { return &qmi.UIMService{}, nil }
	m.rebindUIMServiceHook = func(reason string) (*qmi.UIMService, error) { return nil, ctlGetClientIDFailure() }
	_ = m.withUIMRecovery("UIM.Op", func(uim *qmi.UIMService) error {
		return recoverableQMIError(qmi.ServiceUIM, qmi.UIMGetSlotStatus)
	})

	select {
	case evt := <-m.eventCh:
		t.Fatalf("expected cooldown to suppress second recovery event, got %v", evt)
	case <-time.After(350 * time.Millisecond):
	}
}

func TestRequestCoreRecoveryEnqueuesModemResetEvent(t *testing.T) {
	m := newRecoveryTestManager()
	m.coreReady = true
	m.state = StateDisconnected

	if !m.RequestCoreRecovery("post_switch_service_stalled") {
		t.Fatal("RequestCoreRecovery() = false, want true")
	}
	if evt := waitInternalRecoveryEvent(t, m.eventCh, time.Second); evt != eventModemReset {
		t.Fatalf("expected eventModemReset, got %v", evt)
	}
}

func TestRequestCoreRecoveryBypassesServiceRecoveryCooldown(t *testing.T) {
	m := newRecoveryTestManager()
	m.coreReady = true
	m.state = StateDisconnected
	m.uimRecoverCooldown = time.Hour
	m.uimLastRecoverSignal = time.Now()

	if !m.RequestCoreRecovery("post_switch_service_stalled") {
		t.Fatal("RequestCoreRecovery() = false, want true despite service recovery cooldown")
	}
	if evt := waitInternalRecoveryEvent(t, m.eventCh, time.Second); evt != eventModemReset {
		t.Fatalf("expected eventModemReset, got %v", evt)
	}
}

func TestServiceRecoveryEnsureHooks(t *testing.T) {
	m := newRecoveryTestManager()

	dmsEnsureCalls := 0
	nasEnsureCalls := 0
	wmsEnsureCalls := 0
	voiceEnsureCalls := 0
	uimEnsureCalls := 0
	m.ensureDMSServiceHook = func() (*qmi.DMSService, error) {
		dmsEnsureCalls++
		return &qmi.DMSService{}, nil
	}
	m.ensureNASServiceHook = func() (*qmi.NASService, error) {
		nasEnsureCalls++
		return &qmi.NASService{}, nil
	}
	m.ensureWMSServiceHook = func() (*qmi.WMSService, error) {
		wmsEnsureCalls++
		return &qmi.WMSService{}, nil
	}
	m.ensureVOICEServiceHook = func() (*qmi.VOICEService, error) {
		voiceEnsureCalls++
		return &qmi.VOICEService{}, nil
	}
	m.ensureUIMServiceHook = func() (*qmi.UIMService, error) {
		uimEnsureCalls++
		return &qmi.UIMService{}, nil
	}

	if err := m.withDMSRecovery("DMS.Ensure", func(dms *qmi.DMSService) error { return nil }); err != nil {
		t.Fatalf("DMS ensure path failed: %v", err)
	}
	if err := m.withNASRecovery("NAS.Ensure", func(nas *qmi.NASService) error { return nil }); err != nil {
		t.Fatalf("NAS ensure path failed: %v", err)
	}
	if err := m.withWMSRecovery("WMS.Ensure", func(wms *qmi.WMSService) error { return nil }); err != nil {
		t.Fatalf("WMS ensure path failed: %v", err)
	}
	if err := m.withVOICERecovery("VOICE.Ensure", func(voice *qmi.VOICEService) error { return nil }); err != nil {
		t.Fatalf("VOICE ensure path failed: %v", err)
	}
	if err := m.withUIMRecovery("UIM.Ensure", func(uim *qmi.UIMService) error { return nil }); err != nil {
		t.Fatalf("UIM ensure path failed: %v", err)
	}

	if dmsEnsureCalls != 1 || nasEnsureCalls != 1 || wmsEnsureCalls != 1 || voiceEnsureCalls != 1 || uimEnsureCalls != 1 {
		t.Fatalf(
			"unexpected ensure hook calls: dms=%d nas=%d wms=%d voice=%d uim=%d",
			dmsEnsureCalls, nasEnsureCalls, wmsEnsureCalls, voiceEnsureCalls, uimEnsureCalls,
		)
	}
}

func TestUIMRecoveryRebindThenRetrySuccessAndReplayRegistration(t *testing.T) {
	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{})
	m.ensureUIMServiceHook = func() (*qmi.UIMService, error) { return &qmi.UIMService{}, nil }

	rebindCalls := 0
	m.rebindUIMServiceHook = func(reason string) (*qmi.UIMService, error) {
		rebindCalls++
		if reason != "recover:UIMGetSlotStatus" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		rebound := &qmi.UIMService{}
		m.mu.Lock()
		m.uim = rebound
		m.mu.Unlock()
		ctx, cancel := m.opContext(m.cfg.Timeouts.IndicationRegister)
		_, replayErr := m.registerUIMIndicationsWithContext(ctx, rebound)
		cancel()
		if replayErr != nil {
			m.log.WithError(replayErr).Warn("Failed to replay UIM indication registration after rebind")
		}
		return rebound, nil
	}

	registerCalls := 0
	m.registerUIMIndications = func(_ context.Context) (uint32, error) {
		registerCalls++
		return m.uimIndicationRegistrationMask(), nil
	}

	attempts := 0
	err := m.withUIMRecovery("UIMGetSlotStatus", func(uim *qmi.UIMService) error {
		attempts++
		if attempts == 1 {
			return recoverableQMIError(qmi.ServiceUIM, qmi.UIMGetSlotStatus)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected retry success, got error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if rebindCalls != 1 {
		t.Fatalf("expected 1 rebind call, got %d", rebindCalls)
	}
	if registerCalls != 1 {
		t.Fatalf("expected UIM indication replay once, got %d", registerCalls)
	}
}

func TestUIMRecoveryRebindReplayRegistrationFailureNonFatal(t *testing.T) {
	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{})
	m.ensureUIMServiceHook = func() (*qmi.UIMService, error) { return &qmi.UIMService{}, nil }
	registerCalls := 0
	m.registerUIMIndications = func(_ context.Context) (uint32, error) {
		registerCalls++
		return 0, fmt.Errorf("forced replay register failure")
	}
	m.rebindUIMServiceHook = func(reason string) (*qmi.UIMService, error) {
		if reason != "recover:UIMGetSlotStatus" {
			t.Fatalf("unexpected rebind reason: %q", reason)
		}
		rebound := &qmi.UIMService{}
		m.mu.Lock()
		m.uim = rebound
		m.mu.Unlock()
		ctx, cancel := m.opContext(m.cfg.Timeouts.IndicationRegister)
		_, replayErr := m.registerUIMIndicationsWithContext(ctx, rebound)
		cancel()
		if replayErr != nil {
			m.log.WithError(replayErr).Warn("Failed to replay UIM indication registration after rebind")
		}
		return rebound, nil
	}

	attempts := 0
	err := m.withUIMRecovery("UIMGetSlotStatus", func(uim *qmi.UIMService) error {
		attempts++
		if attempts == 1 {
			return recoverableQMIError(qmi.ServiceUIM, qmi.UIMGetSlotStatus)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected retry success even when replay registration fails, got: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if registerCalls != 1 {
		t.Fatalf("expected replay registration call once, got %d", registerCalls)
	}
}

func TestEnqueueModemResetEventDeduplicatesBurst(t *testing.T) {
	m := newRecoveryTestManager()
	m.modemResetDedupWindow = time.Minute

	m.enqueueModemResetEvent("t1")
	m.enqueueModemResetEvent("t2")

	if evt := waitInternalRecoveryEvent(t, m.eventCh, time.Second); evt != eventModemReset {
		t.Fatalf("expected modem reset event, got %v", evt)
	}

	select {
	case evt := <-m.eventCh:
		t.Fatalf("expected second event to be deduplicated, got %v", evt)
	case <-time.After(300 * time.Millisecond):
	}

	stats := m.Stats()
	if stats.ResetEvents != 2 {
		t.Fatalf("expected reset_events=2, got %d", stats.ResetEvents)
	}
	if stats.ResetCoalesced < 1 {
		t.Fatalf("expected reset_coalesced >= 1, got %d", stats.ResetCoalesced)
	}
}

func TestHandleModemResetEventCoalescesWhileRecovering(t *testing.T) {
	m := newRecoveryTestManager()
	m.modemResetRecovering = true

	m.handleEvent(eventModemReset)

	select {
	case evt := <-m.eventCh:
		t.Fatalf("unexpected modem reset enqueue while recovering: %v", evt)
	case <-time.After(200 * time.Millisecond):
	}

	if !m.modemResetPending {
		t.Fatal("expected modemResetPending=true while coalescing")
	}
	if stats := m.Stats(); stats.ResetCoalesced < 1 {
		t.Fatalf("expected reset_coalesced >= 1, got %d", stats.ResetCoalesced)
	}
}

func TestStopCancelsRecoveryRetryWhenCoreNotReadyDisconnected(t *testing.T) {
	m := newRecoveryTestManager()
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.cancel = cancel
	m.state = StateDisconnected
	m.coreReady = false

	var scheduled func()
	m.afterFunc = func(delay time.Duration, fn func()) *time.Timer {
		if delay <= 0 {
			t.Fatalf("expected positive recovery delay, got %v", delay)
		}
		scheduled = fn
		return time.NewTimer(time.Hour)
	}

	m.scheduleRecoverRetry("test")
	if scheduled == nil {
		t.Fatal("expected recovery retry callback to be scheduled")
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	scheduled()

	select {
	case evt := <-m.eventCh:
		t.Fatalf("stopped manager enqueued unexpected event %v", evt)
	default:
	}
	if ctx.Err() == nil {
		t.Fatal("expected Stop() to cancel manager context")
	}
}

type warnRecorder struct {
	Logger
	warns *[][]interface{}
}

func (r warnRecorder) Warn(args ...interface{})             { *r.warns = append(*r.warns, args) }
func (r warnRecorder) WithField(string, interface{}) Logger { return r }
func (r warnRecorder) WithError(error) Logger               { return r }

// hideck#94: the storm log printed services_affected 0 because it read the
// window after clearing it.
func TestTimeoutStormLogNamesTheServices(t *testing.T) {
	m := newRecoveryTestManager()
	var warns [][]interface{}
	m.log = warnRecorder{Logger: NewNopLogger(), warns: &warns}
	m.detectTimeoutStorm("NAS")
	m.detectTimeoutStorm("DMS")
	for _, args := range warns {
		if len(args) == 0 || args[0] != "Timeout storm detected; triggering immediate core recovery" {
			continue
		}
		got := fmt.Sprint(args[1:]...)
		if got != fmt.Sprint("services_affected", 2, "services", "DMS,NAS") {
			t.Fatalf("storm log fields = %v", args[1:])
		}
		return
	}
	t.Fatalf("no storm log in %v", warns)
}
