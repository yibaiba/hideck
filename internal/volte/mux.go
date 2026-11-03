package volte

import (
	"context"
	"strings"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
)

type VoiceBackend interface {
	SubscribeIncomingCalls(func(voicehost.IncomingCall)) func()
	SubscribeCallEvents(func(voicehost.CallEvent)) func()
	BeginCall(context.Context, voicehost.BeginCallRequest) (voicehost.CallSnapshot, error)
	ActiveCall(deviceID string) *voicehost.CallSnapshot
	AnswerIncomingCall(context.Context, voicehost.AnswerRequest) (voicehost.AnswerResult, error)
	RejectIncomingCall(voicehost.RejectRequest) error
	HangupCall(context.Context, string, string) error
	SendCallDTMF(string, string, string) error
	HoldCall(context.Context, string, string) error
	ResumeCall(context.Context, string, string) error
	SwitchCall(string, string) error
	StartCallCapture(string, string, string) error
	DeviceStatus(deviceID string) map[string]interface{}
}

type callMediaUpdater interface {
	UpdateCallMedia(string, string, string) error
}

var _ callMediaUpdater = (*Controller)(nil)

type Mux struct {
	BeforeDial func(context.Context, string, string) error
	IMS        VoiceBackend
	Native     VoiceBackend
	IsNative   func(deviceID string) bool
	Modem      VoiceBackend
	IsModem    func(deviceID string) bool
}

func (m *Mux) native(deviceID string) bool {
	if m == nil || m.IsNative == nil {
		return false
	}
	return m.IsNative(strings.TrimSpace(deviceID))
}

func (m *Mux) pick(deviceID string) VoiceBackend {
	if m.IsModem != nil && m.IsModem(deviceID) && m.Modem != nil {
		return m.Modem
	}
	if m.native(deviceID) && m.Native != nil {
		return m.Native
	}
	return m.IMS
}

// Existing calls retain their backend when a device's selected mode changes.
func (m *Mux) pickCall(deviceID, callID string) VoiceBackend {
	if strings.HasPrefix(callID, "modemvoice-") && m.Modem != nil {
		return m.Modem
	}
	if strings.HasPrefix(callID, "volte-") && m.Native != nil {
		return m.Native
	}
	if m.IsModem != nil && m.IsModem(deviceID) {
		return m.IMS
	}
	return m.pick(deviceID)
}

func (m *Mux) SubscribeIncomingCalls(handler func(voicehost.IncomingCall)) func() {
	var unsubs []func()
	if m != nil && m.Modem != nil {
		unsubs = append(unsubs, m.Modem.SubscribeIncomingCalls(handler))
	}
	if m != nil && m.IMS != nil {
		unsubs = append(unsubs, m.IMS.SubscribeIncomingCalls(handler))
	}
	if m != nil && m.Native != nil {
		unsubs = append(unsubs, m.Native.SubscribeIncomingCalls(handler))
	}
	return func() {
		for _, u := range unsubs {
			if u != nil {
				u()
			}
		}
	}
}

func (m *Mux) SubscribeCallEvents(handler func(voicehost.CallEvent)) func() {
	var unsubs []func()
	if m != nil && m.Modem != nil {
		unsubs = append(unsubs, m.Modem.SubscribeCallEvents(handler))
	}
	if m != nil && m.IMS != nil {
		unsubs = append(unsubs, m.IMS.SubscribeCallEvents(handler))
	}
	if m != nil && m.Native != nil {
		unsubs = append(unsubs, m.Native.SubscribeCallEvents(handler))
	}
	return func() {
		for _, u := range unsubs {
			if u != nil {
				u()
			}
		}
	}
}

func (m *Mux) BeginCall(ctx context.Context, request voicehost.BeginCallRequest) (voicehost.CallSnapshot, error) {
	if m.BeforeDial != nil {
		if err := m.BeforeDial(ctx, request.DeviceID, request.Callee); err != nil {
			return voicehost.CallSnapshot{}, err
		}
	}
	return m.pick(request.DeviceID).BeginCall(ctx, request)
}

func (m *Mux) ActiveCall(deviceID string) *voicehost.CallSnapshot {
	if m.Modem != nil {
		if call := m.Modem.ActiveCall(deviceID); call != nil {
			return call
		}
	}
	return m.pick(deviceID).ActiveCall(deviceID)
}

func (m *Mux) AnswerIncomingCall(ctx context.Context, request voicehost.AnswerRequest) (voicehost.AnswerResult, error) {
	return m.pickCall(request.DeviceID, request.CallID).AnswerIncomingCall(ctx, request)
}

func (m *Mux) RejectIncomingCall(request voicehost.RejectRequest) error {
	return m.pickCall(request.DeviceID, request.CallID).RejectIncomingCall(request)
}

func (m *Mux) HangupCall(ctx context.Context, deviceID, callID string) error {
	return m.pickCall(deviceID, callID).HangupCall(ctx, deviceID, callID)
}

func (m *Mux) SendCallDTMF(deviceID, callID, digit string) error {
	return m.pickCall(deviceID, callID).SendCallDTMF(deviceID, callID, digit)
}

func (m *Mux) HoldCall(ctx context.Context, deviceID, callID string) error {
	return m.pickCall(deviceID, callID).HoldCall(ctx, deviceID, callID)
}

func (m *Mux) ResumeCall(ctx context.Context, deviceID, callID string) error {
	return m.pickCall(deviceID, callID).ResumeCall(ctx, deviceID, callID)
}

func (m *Mux) SwitchCall(deviceID, callID string) error {
	return m.pickCall(deviceID, callID).SwitchCall(deviceID, callID)
}

func (m *Mux) StartCallCapture(deviceID, callID, basePath string) error {
	return m.pickCall(deviceID, callID).StartCallCapture(deviceID, callID, basePath)
}

func (m *Mux) DeviceStatus(deviceID string) map[string]interface{} {
	backend := m.pick(deviceID)
	if backend == nil {
		return map[string]interface{}{"device_id": deviceID, "ready": false}
	}
	return backend.DeviceStatus(deviceID)
}

func (m *Mux) UpdateCallMedia(deviceID, callID, sdp string) error {
	if backend, ok := m.pickCall(deviceID, callID).(callMediaUpdater); ok {
		return backend.UpdateCallMedia(deviceID, callID, sdp)
	}
	// Backends without explicit updates retain their existing media recovery path.
	return nil
}
