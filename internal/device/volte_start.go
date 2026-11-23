package device

import (
	"context"
	"errors"
)

// Each start owns its cancellation and the worker/SIM captured at submission.
// Pointer identity prevents an old completion from removing a newer start.
type nativeVoLTEStart struct {
	ctx    context.Context
	cancel context.CancelFunc
	worker *Worker
	sim    simOwnership
	ready  <-chan struct{}
}

type nativeVoLTETransition struct {
	depth int
	done  chan struct{}
}

func (p *Pool) beginNativeVoLTESchedule(deviceID string) *nativeVoLTEStart {
	p.nativeVoLTEScheduleMu.Lock()
	defer p.nativeVoLTEScheduleMu.Unlock()
	w := p.GetWorker(deviceID)
	sim := currentSIMOwnership(w)
	if old := p.nativeVoLTEScheduled[deviceID]; old != nil {
		if old.worker == w && old.sim == sim && old.ctx.Err() == nil {
			return nil
		}
		p.cancelNativeVoLTEStartLocked(deviceID)
	}
	if p.nativeVoLTEScheduled == nil {
		p.nativeVoLTEScheduled = make(map[string]*nativeVoLTEStart)
	}
	ctx, cancel := context.WithCancel(p.Context())
	start := &nativeVoLTEStart{ctx: ctx, cancel: cancel, worker: w, sim: sim}
	if transition := p.nativeVoLTETransitions[deviceID]; transition != nil {
		start.ready = transition.done
	}
	p.nativeVoLTEScheduled[deviceID] = start
	return start
}

func (p *Pool) endNativeVoLTESchedule(deviceID string, start *nativeVoLTEStart) {
	p.nativeVoLTEScheduleMu.Lock()
	defer p.nativeVoLTEScheduleMu.Unlock()
	start.cancel()
	if p.nativeVoLTEScheduled[deviceID] == start {
		delete(p.nativeVoLTEScheduled, deviceID)
	}
}

func (p *Pool) cancelNativeVoLTEStart(deviceID string) {
	if p == nil {
		return
	}
	p.nativeVoLTEScheduleMu.Lock()
	defer p.nativeVoLTEScheduleMu.Unlock()
	// A teardown already in progress must not cancel a newer queued intent.
	// A subsequent transition cancels it explicitly at transition entry.
	if start := p.nativeVoLTEScheduled[deviceID]; start != nil {
		if transition := p.nativeVoLTETransitions[deviceID]; transition != nil && start.ready == transition.done {
			return
		}
	}
	p.cancelNativeVoLTEStartLocked(deviceID)
}

func (p *Pool) cancelNativeVoLTEWorkerStart(worker *Worker) {
	if worker == nil {
		return
	}
	p.nativeVoLTEScheduleMu.Lock()
	defer p.nativeVoLTEScheduleMu.Unlock()
	if start := p.nativeVoLTEScheduled[worker.ID]; start != nil && start.worker == worker {
		p.cancelNativeVoLTEStartLocked(worker.ID)
	}
}

func (p *Pool) cancelNativeVoLTEStartLocked(deviceID string) {
	if start := p.nativeVoLTEScheduled[deviceID]; start != nil {
		start.cancel()
		delete(p.nativeVoLTEScheduled, deviceID)
	}
}

// New submissions wait for all overlapping transitions, then recheck ownership
// and the effective policy. An older teardown cannot discard the latest intent.
func (p *Pool) beginNativeVoLTETransition(deviceID string) func() {
	p.nativeVoLTEScheduleMu.Lock()
	if p.nativeVoLTETransitions == nil {
		p.nativeVoLTETransitions = make(map[string]*nativeVoLTETransition)
	}
	transition := p.nativeVoLTETransitions[deviceID]
	if transition == nil {
		transition = &nativeVoLTETransition{done: make(chan struct{})}
		p.nativeVoLTETransitions[deviceID] = transition
	}
	transition.depth++
	p.cancelNativeVoLTEStartLocked(deviceID)
	p.nativeVoLTEScheduleMu.Unlock()
	return func() {
		p.nativeVoLTEScheduleMu.Lock()
		defer p.nativeVoLTEScheduleMu.Unlock()
		transition.depth--
		if transition.depth == 0 {
			delete(p.nativeVoLTETransitions, deviceID)
			close(transition.done)
		}
	}
}

func (p *Pool) validateNativeVoLTEStart(deviceID string, start *nativeVoLTEStart) error {
	p.nativeVoLTEScheduleMu.Lock()
	defer p.nativeVoLTEScheduleMu.Unlock()
	if err := start.ctx.Err(); err != nil {
		return err
	}
	if p.nativeVoLTEScheduled[deviceID] != start || p.nativeVoLTETransitions[deviceID] != nil {
		return context.Canceled
	}
	w := p.GetWorker(deviceID)
	if w == nil || w != start.worker || currentSIMOwnership(w) != start.sim || p.IsESIMSwitching(deviceID) {
		return errors.New("VoLTE 启动已失效：设备或 SIM 已更换或正在切卡")
	}
	if err := w.CheckPhoneModeSupported(PhoneModeVoLTE); err != nil {
		return err
	}
	if !IsNativeVoLTEMode(w.Config.PhoneMode) || !PhoneServiceEnabled(w.Config) || w.Config.AirplaneEnabled {
		return errors.New("VoLTE 启动已失效：电话模式已切换或处于飞行模式")
	}
	class, err := ClassifyWorkerLebaraUK(w)
	if err != nil {
		return err
	}
	if class.IsLebara || class.BlocksVoWiFi() {
		return ErrLebaraUKRFLocked
	}
	return nil
}

func (start *nativeVoLTEStart) waitTransition() error {
	if start.ready != nil {
		select {
		case <-start.ctx.Done():
			return start.ctx.Err()
		case <-start.ready:
		}
	}
	return start.ctx.Err()
}
