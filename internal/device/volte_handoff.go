package device

import (
	"context"
	"errors"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/modemvoice"
	"github.com/yibaiba/hideck/internal/volte"
)

type nativeIdleHandoff struct {
	port     *modemVoicePort
	executor modemvoice.Executor
}

func (p *Pool) nativeIdleHandoff(start *nativeVoLTEStart) volte.IdleHandoff {
	w := start.worker
	if w.Backend == nil || w.Backend.Mode() != backend.BackendAT || w.QMICore != nil {
		return nil
	}
	port := &modemVoicePort{
		pool: p, worker: w, iccid: start.sim.iccid, identityGeneration: start.sim.generation,
	}
	return &nativeIdleHandoff{port: port, executor: port}
}

func (h *nativeIdleHandoff) Validate(ctx context.Context) error {
	if err := h.port.checkIdentity(ctx); err != nil {
		return err
	}
	w := h.port.worker
	if w.Backend == nil || w.Backend.Mode() != backend.BackendAT || w.QMICore != nil {
		return errors.New("VoLTE 交接的 AT 后端已变化")
	}
	return nil
}

func (h *nativeIdleHandoff) CheckIdle(ctx context.Context) error {
	if err := h.Validate(ctx); err != nil {
		return err
	}
	client, err := modemvoice.NewClient(h.executor)
	if err != nil {
		return err
	}
	calls, err := client.Calls(ctx)
	if err != nil {
		return err
	}
	if err := h.Validate(ctx); err != nil {
		return err
	}
	if len(calls) != 0 {
		return errors.New("模组仍有呼叫，请结束通话后重试切换")
	}
	return nil
}
