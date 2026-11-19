package device

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/yibaiba/hideck/internal/outbound"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

type OutboundLimiter interface {
	Consume(context.Context, outbound.Request) error
}

// SetOutboundLimiter is configured by the application before workers start.
func (p *Pool) SetOutboundLimiter(limiter OutboundLimiter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outboundLimiter = limiter
}

var outboundNumber = regexp.MustCompile(`^\+?[0-9*#]{1,32}$`)

func (p *Pool) AuthorizeOutboundCall(ctx context.Context, id, number string) error {
	if !outboundNumber.MatchString(strings.TrimSpace(number)) {
		return errors.New("无效的外呼号码")
	}
	return p.consumeOutbound(ctx, p.GetWorker(id), outbound.Request{Kind: outbound.Call, Units: 1}, nil)
}

// DialAuthorizedCellularCall keeps the SIM owner stable from quota charging
// through the modem dial command.
func (p *Pool) DialAuthorizedCellularCall(ctx context.Context, worker *Worker, number string) error {
	if p == nil || worker == nil || worker.Modem == nil {
		return errors.New("cellular calling requires an AT modem channel")
	}
	if err := worker.CheckPhoneModeSupported(PhoneModeVoLTE); err != nil {
		return err
	}
	release, err := p.acquireOutboundOperation(worker)
	if err != nil {
		return err
	}
	defer release()
	checkOwner := p.outboundOwnerCheck(worker)
	if err := p.AuthorizeOutboundCall(ctx, worker.ID, number); err != nil {
		return err
	}
	if err := checkOwner(); err != nil {
		return err
	}
	return worker.Modem.DialCall(number)
}

func (p *Pool) acquireOutboundOperation(worker *Worker) (func(), error) {
	if p == nil || worker == nil {
		return nil, errors.New("outbound operation requires an active device")
	}
	releaseDevice, err := p.AcquireDeviceOperation(worker.ID)
	if err != nil {
		return nil, err
	}
	transition := p.policyTransitionFor(worker.ID)
	transition.Lock()
	return func() {
		transition.Unlock()
		releaseDevice()
	}, nil
}

type outboundSMSRequest struct {
	Worker     *Worker
	To, Text   string
	Options    smscodec.SubmitOptions
	CheckOwner func() error
}

func (p *Pool) authorizeSMS(ctx context.Context, request outboundSMSRequest) error {
	if !outboundNumber.MatchString(strings.TrimSpace(request.To)) || strings.TrimSpace(request.Text) == "" {
		return errors.New("短信号码或内容无效")
	}
	parts, _, err := smscodec.BuildSubmitTPDUsWithOptions(request.To, request.Text, request.Options)
	if err != nil {
		return err
	}
	return p.consumeOutbound(ctx, request.Worker, outbound.Request{Kind: outbound.SMS, Units: len(parts)}, request.CheckOwner)
}

// A queued operation must not spend one SIM's budget on another.
func (p *Pool) outboundOwnerCheck(w *Worker) func() error {
	sim := currentSIMOwnership(w)
	return func() error {
		if p != nil && (w == nil || p.GetWorker(w.ID) != w || currentSIMOwnership(w) != sim || p.IsESIMSwitching(w.ID)) {
			return errors.New("等待外发期间设备或 SIM 已变更，请重新操作")
		}
		return nil
	}
}

func (p *Pool) consumeOutbound(ctx context.Context, w *Worker, request outbound.Request, checkOwner func() error) error {
	if p == nil {
		return nil
	}
	if w != nil {
		if err := p.CheckMBNIdle(w.ID); err != nil {
			return err
		}
	}
	p.mu.RLock()
	limiter := p.outboundLimiter
	p.mu.RUnlock()
	// Embedding hosts/tests may omit the optional policy; main always installs it.
	if limiter == nil {
		return nil
	}
	sim := currentSIMOwnership(w)
	if checkOwner != nil {
		if err := checkOwner(); err != nil {
			return err
		}
	} else if w == nil || p.GetWorker(w.ID) != w || p.IsESIMSwitching(w.ID) {
		return errors.New("设备或 SIM 正在变更，不能发起外发请求")
	}
	attempt := outbound.Request{ICCID: sim.iccid, Kind: request.Kind, Units: request.Units}
	if err := limiter.Consume(contextOrBackground(ctx), attempt); err != nil {
		return err
	}
	if checkOwner != nil {
		return checkOwner()
	}
	if p.GetWorker(w.ID) != w || currentSIMOwnership(w) != sim || p.IsESIMSwitching(w.ID) {
		return errors.New("记录额度期间设备或 SIM 已变更，请重新操作")
	}
	return nil
}
