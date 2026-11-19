package device

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
)

var errPolicyOwnerChanged = errors.New("卡策略任务已失效：设备或 SIM 已更换，或正在切卡")

// Stable across removal/re-addition; never hold the registry mutex during I/O.
type policyTransition struct {
	sync.Mutex
	switchEpoch uint64
	mbnSession  atomic.Pointer[mbnDeviceSession]
	operations  sync.RWMutex // Ordinary operations share; MBN owns it through re-enumeration.
}

func (p *Pool) policyTransitionFor(deviceID string) *policyTransition {
	p.policyTransitionsMu.Lock()
	defer p.policyTransitionsMu.Unlock()
	if p.policyTransitions == nil {
		p.policyTransitions = make(map[string]*policyTransition)
	}
	deviceID = strings.TrimSpace(deviceID)
	transition := p.policyTransitions[deviceID]
	if transition == nil {
		transition = &policyTransition{}
		p.policyTransitions[deviceID] = transition
	}
	return transition
}

type policyOwner struct {
	worker             *Worker
	iccid              string
	identityGeneration uint64
	switchEpoch        uint64
}

func capturePolicyOwner(worker *Worker, transition *policyTransition) policyOwner {
	worker.cacheMu.RLock()
	defer worker.cacheMu.RUnlock()
	return policyOwner{worker: worker, iccid: strings.TrimSpace(worker.state.Identity.ICCID),
		identityGeneration: worker.state.Identity.Generation, switchEpoch: transition.switchEpoch}
}

// Caller holds the device transition lock through effects, excluding removal,
// registration and a new eSIM switch. Recheck after slow operations as physical
// SIM identity observations can still arrive while those operations run.
func (p *Pool) validatePolicyOwner(owner policyOwner, transition *policyTransition) error {
	w := owner.worker
	if p.GetWorker(w.ID) != w || transition.switchEpoch != owner.switchEpoch {
		return errPolicyOwnerChanged
	}
	w.cacheMu.RLock()
	identity := w.state.Identity
	w.cacheMu.RUnlock()
	if strings.TrimSpace(identity.ICCID) != owner.iccid || identity.Generation != owner.identityGeneration {
		return errPolicyOwnerChanged
	}
	if w.atDeviceRules().strictSIMIdentity && (!identity.Ready || identity.Phase != simIdentityPhaseReady ||
		strings.TrimSpace(identity.ICCID) == "" || strings.TrimSpace(identity.IMSI) == "") {
		return errPolicyOwnerChanged
	}
	p.switchMu.Lock()
	switching, snapshot := p.switchingDevices[w.ID], p.switchContexts[w.ID]
	p.switchMu.Unlock()
	target := normalizeSIMIdentityForCompare(snapshot.TargetICCID)
	if switching && (identity.Phase != simIdentityPhaseReady || snapshot.IdentityGeneration == 0 ||
		identity.Generation != snapshot.IdentityGeneration ||
		(target != "" && normalizeSIMIdentityForCompare(owner.iccid) != target)) {
		return errPolicyOwnerChanged
	}
	select {
	case <-w.stop:
		return errPolicyOwnerChanged
	default:
		return nil
	}
}
