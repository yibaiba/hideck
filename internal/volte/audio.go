package volte

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

var ErrAudioUnsupported = errors.New("volte: audio runtime unsupported")

type AudioIdentity struct {
	DeviceID   string
	IMEI       string
	Firmware   string
	Kernel     string
	USBParent  string
	ALSADevice string
	HelperHash string
}

type CommandRunner interface {
	Start(name string, args ...string) (pid int, err error)
	Stop(pid int) error
}

type ALSALookup interface {
	DeviceForUSBParent(usbParent string) (string, error)
}

type audioCall struct {
	deviceID string
	alsa     string
	pid      int
}

type AudioRuntime struct {
	expect AudioIdentity
	runner CommandRunner
	cards  ALSALookup
	helper string
	mu     sync.Mutex
	bound  map[string]AudioIdentity
	calls  map[string]audioCall
}

func NewAudioRuntime(expect AudioIdentity, runner CommandRunner, cards ALSALookup, helper string) *AudioRuntime {
	return &AudioRuntime{
		expect: expect,
		runner: runner,
		cards:  cards,
		helper: helper,
		bound:  make(map[string]AudioIdentity),
		calls:  make(map[string]audioCall),
	}
}

func (r *AudioRuntime) Bind(id AudioIdentity) error {
	if r == nil {
		return ErrAudioUnsupported
	}
	if strings.TrimSpace(id.IMEI) == "" || id.IMEI != r.expect.IMEI {
		return fmt.Errorf("%w: IMEI", ErrAudioUnsupported)
	}
	if id.Firmware != r.expect.Firmware {
		return fmt.Errorf("%w: firmware", ErrAudioUnsupported)
	}
	if id.Kernel != r.expect.Kernel {
		return fmt.Errorf("%w: kernel", ErrAudioUnsupported)
	}
	if id.HelperHash != r.expect.HelperHash {
		return fmt.Errorf("%w: helper hash", ErrAudioUnsupported)
	}
	if strings.TrimSpace(id.USBParent) == "" {
		return fmt.Errorf("%w: usb parent", ErrAudioUnsupported)
	}
	alsa := strings.TrimSpace(id.ALSADevice)
	if r.cards != nil {
		found, err := r.cards.DeviceForUSBParent(id.USBParent)
		if err != nil {
			return err
		}
		alsa = found
	}
	if alsa == "" {
		return fmt.Errorf("%w: alsa", ErrAudioUnsupported)
	}
	id.ALSADevice = alsa
	r.mu.Lock()
	r.bound[id.DeviceID] = id
	r.mu.Unlock()
	return nil
}

func (r *AudioRuntime) Start(deviceID, callID string) error {
	if r == nil || r.runner == nil {
		return ErrAudioUnsupported
	}
	deviceID = strings.TrimSpace(deviceID)
	callID = strings.TrimSpace(callID)
	r.mu.Lock()
	id, ok := r.bound[deviceID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: device not bound", ErrAudioUnsupported)
	}
	pid, err := r.runner.Start(r.helper, "--device", id.ALSADevice, "--usb", id.USBParent, "--call", callID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.calls[callID] = audioCall{deviceID: deviceID, alsa: id.ALSADevice, pid: pid}
	r.mu.Unlock()
	return nil
}

func (r *AudioRuntime) Stop(callID string) error {
	if r == nil {
		return nil
	}
	callID = strings.TrimSpace(callID)
	r.mu.Lock()
	call, ok := r.calls[callID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	if r.runner != nil && call.pid != 0 {
		if err := r.runner.Stop(call.pid); err != nil {
			return err
		}
	}
	r.mu.Lock()
	if r.calls[callID] == call {
		delete(r.calls, callID)
	}
	r.mu.Unlock()
	return nil
}

func (r *AudioRuntime) Owner(callID string) (audioCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call, ok := r.calls[strings.TrimSpace(callID)]
	return call, ok
}
