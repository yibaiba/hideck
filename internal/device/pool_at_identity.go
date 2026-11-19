package device

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/modem"
)

var errATIdentityUnavailable = errors.New("at identity unavailable")
var errATNoSIM = errors.New("AT modem has no SIM")

type atIdentityRules struct {
	verifyLiveIMEI    bool
	strictSIMIdentity bool
	simStatusSource   string
	model             string
}

func atIdentityRulesFor(profile modem.ATProfile) atIdentityRules {
	switch profile {
	case modem.ATProfileML307C:
		return atIdentityRules{verifyLiveIMEI: true, strictSIMIdentity: true, simStatusSource: "ml307c_sim_status", model: "ML307C"}
	default:
		return atIdentityRules{model: "EC20"}
	}
}

func (w *Worker) atProfile() modem.ATProfile {
	if w == nil || w.Modem == nil {
		return modem.ATProfileDefault
	}
	return w.Modem.ATProfile()
}

func (w *Worker) isML307C() bool {
	return w.atProfile() == modem.ATProfileML307C
}

var probeATIdentityFn = modem.ProbeATIdentity

func probeWorkerATIdentity(ctx context.Context, w *Worker) (modem.ATIdentity, error) {
	if w == nil {
		return modem.ATIdentity{}, errATIdentityUnavailable
	}
	atPort := w.ResolvedATPort()
	if atPort == "" {
		return modem.ATIdentity{}, errATIdentityUnavailable
	}
	timeout := 2 * time.Second
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			if remain := time.Until(deadline); remain > 0 && remain < timeout {
				timeout = remain
			}
		}
	}
	ident, err := probeATIdentityFn(atPort, timeout)
	if err != nil {
		return ident, err
	}
	ident.IMEI = strings.TrimSpace(ident.IMEI)
	ident.IMSI = strings.TrimSpace(ident.IMSI)
	ident.ICCID = strings.TrimSpace(ident.ICCID)
	if ident.IMSI == "" && ident.ICCID == "" {
		return ident, errATIdentityUnavailable
	}
	return ident, nil
}

func (w *Worker) markQMICoreStarting() {
	if w != nil {
		w.qmiCoreStarting.Store(true)
	}
}

func (w *Worker) clearQMICoreStarting() {
	if w != nil {
		w.qmiCoreStarting.Store(false)
	}
}

func (w *Worker) isQMICoreStarting() bool {
	return w != nil && w.qmiCoreStarting.Load()
}
