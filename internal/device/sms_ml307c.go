package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/internal/smsnotify"
	"github.com/yibaiba/hideck/pkg/logger"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

var ml307cSMSStores = [...]string{"ME", "SM"}

type ml307cSMSOwner struct {
	workerLifecycleOwner
	SMSIdentity
	simGeneration uint64
}

func (w *Worker) ml307cSMSOwner() (ml307cSMSOwner, error) {
	w.cacheMu.RLock()
	id := w.state.Identity
	w.cacheMu.RUnlock()
	identity := normalizeSMSIdentity(SMSIdentity{ICCID: id.ICCID, IMSI: id.IMSI})
	if !id.Ready || id.Phase != simIdentityPhaseReady || identity.ICCID == "" || identity.IMSI == "" {
		return ml307cSMSOwner{}, ErrSMSIdentityUnknown
	}
	return ml307cSMSOwner{
		workerLifecycleOwner: workerLifecycleOwner{worker: w, generation: w.generation},
		SMSIdentity:          identity, simGeneration: id.Generation,
	}, nil
}

func (owner ml307cSMSOwner) check() error {
	w := owner.worker
	if !w.Pool.workerLifecycleOwnerCurrent(owner.workerLifecycleOwner) ||
		w.Pool.IsESIMSwitching(w.ID) || !w.Modem.StoredSMSReceiving() {
		return ErrSMSIdentityTransitioning
	}
	current, err := w.ml307cSMSOwner()
	if err != nil || current != owner {
		return ErrSMSIdentityTransitioning
	}
	return nil
}

func (owner ml307cSMSOwner) verifyLive() error {
	if err := owner.check(); err != nil {
		return err
	}
	w := owner.worker
	inserted, err := w.Modem.QuerySIMInserted()
	if err != nil {
		return err
	}
	if !inserted {
		return ErrSMSIdentityUnknown
	}
	iccid, err := w.Modem.QueryICCID()
	if err != nil {
		return err
	}
	imsi, err := w.Modem.QueryIMSI()
	if err != nil {
		return err
	}
	if normalizeSMSIdentity(SMSIdentity{ICCID: iccid, IMSI: imsi}) != owner.SMSIdentity {
		return ErrSMSIdentityConflict
	}
	return owner.check()
}

func (w *Worker) handleML307CStoredSMS(storage, index string) {
	slot, err := strconv.Atoi(index)
	if err != nil || slot < 0 || (storage != "ME" && storage != "SM") {
		return
	}
	// Capture before queuing the work. A delayed URC must keep its original
	// worker and SIM generation across eSIM and physical card changes.
	owner, err := w.ml307cSMSOwner()
	if err != nil {
		return
	}
	go w.runML307CSMS(owner, storage, slot)
}

func (w *Worker) scanML307CSMS() {
	if !w.isML307C() || !w.Modem.StoredSMSReceiving() {
		return
	}
	owner, err := w.ml307cSMSOwner()
	if err != nil {
		return
	}
	w.runML307CSMS(owner, "", 0)
}

func (w *Worker) runML307CSMS(owner ml307cSMSOwner, storage string, index int) {
	var notifications []InboundSMS
	err := w.readML307CSMSInbox(owner, storage, index, &notifications)
	if err != nil {
		logger.Warn("ML307C 短信收件未完成", "device", w.ID, "storage", storage, "err", err)
	}
	for _, inbound := range notifications {
		w.Pool.notifyInboundSMS(inbound)
		if notifier := w.Pool.getNotifier(); notifier != nil {
			notifySMS(notifier, inbound, "")
		}
	}
}

func (w *Worker) readML307CSMSInbox(owner ml307cSMSOwner, storage string, index int, notifications *[]InboundSMS) error {
	// The eSIM manager uses the same transition lock. Take it before the
	// serial inbox lock, matching the lock order used for sending.
	release, err := w.Pool.AcquireDeviceOperation(w.ID)
	if err != nil {
		return err
	}
	defer release()
	transition := w.Pool.policyTransitionFor(w.ID)
	transition.Lock()
	defer transition.Unlock()
	gateCtx, cancelGate := context.WithTimeout(w.Pool.Context(), 5*time.Second)
	defer cancelGate()
	releaseAT, err := w.Pool.acquireDeviceAT(gateCtx, w.ID)
	if err != nil {
		return err
	}
	defer releaseAT()
	if err := owner.verifyLive(); err != nil {
		return err
	}
	scope := db.SMSInboundFragmentScope{DeviceID: w.ID, ICCID: owner.ICCID, IMSI: owner.IMSI}
	return w.Modem.WithSMSInbox(func(inbox *modem.SMSInbox) error {
		state, err := loadATSMSParts(scope)
		if err != nil {
			return err
		}
		if storage != "" {
			if err := inbox.Select(storage); err != nil {
				return err
			}
			record, err := inbox.Read(index)
			if err != nil {
				return err
			}
			if err := owner.verifyLive(); err != nil {
				return err
			}
			return w.ingestML307CSlot(inbox, owner, state, record, true, notifications)
		}

		var scanErr error
		for _, store := range ml307cSMSStores {
			if err := inbox.Select(store); err != nil {
				return errors.Join(scanErr, err)
			}
			records, err := inbox.List()
			if err != nil {
				scanErr = errors.Join(scanErr, err)
				continue
			}
			if err := owner.verifyLive(); err != nil {
				return errors.Join(scanErr, err)
			}
			for _, record := range records {
				if err := w.ingestML307CSlot(inbox, owner, state, record, store == "SM", notifications); err != nil {
					scanErr = errors.Join(scanErr, err)
				}
			}
		}
		if scanErr != nil {
			return scanErr
		}
		if err := owner.check(); err != nil {
			return err
		}
		return state.clearCompletedGroups()
	})
}

func (w *Worker) ingestML307CSlot(inbox *modem.SMSInbox, owner ml307cSMSOwner, state *atSMSParts, record modem.SMSPDURecord, sourceKnown bool, notifications *[]InboundSMS) error {
	if !record.Received() {
		return nil
	}
	if err := owner.check(); err != nil {
		return err
	}
	part, err := decodeATInboxPart(owner.SMSIdentity, record.PDU)
	if err != nil {
		return fmt.Errorf("SMS slot %d: %w", record.Index, err)
	}
	stored, inbound, err := state.persist(part, sourceKnown)
	if err != nil {
		return fmt.Errorf("persist SMS slot %d: %w", record.Index, err)
	}
	if inbound != nil {
		*notifications = append(*notifications, *inbound)
	}
	if !stored {
		return nil
	}
	if err := inbox.DeleteMatching(record, owner.verifyLive); err != nil {
		return fmt.Errorf("delete persisted SMS slot %d: %w", record.Index, err)
	}
	return nil
}

type atInboxPart struct {
	smscodec.ATDeliverSMS
	key string
}

func decodeATInboxPart(identity SMSIdentity, pduHex string) (atInboxPart, error) {
	raw, err := hex.DecodeString(pduHex)
	if err != nil {
		return atInboxPart{}, errors.New("invalid SMS PDU hex")
	}
	decoded, err := smscodec.DecodeATDeliverSMS(raw)
	if err != nil || decoded.Sender == "" || decoded.Timestamp.IsZero() {
		return atInboxPart{}, errors.New("cannot decode stored SMS-DELIVER")
	}
	key := fmt.Sprintf("%ssingle:%x", db.ATSMSInboundPrefix, sha256.Sum256(raw))
	concat := decoded.Concat
	if concat.IsConcat {
		if concat.Seq < 1 || concat.Seq > concat.Total {
			return atInboxPart{}, errors.New("invalid SMS fragment sequence")
		}
		group := fmt.Sprintf("%q:%q:%d:%d:%d:%s", identity.IMSI, decoded.Sender, concat.RefBits, concat.Ref, concat.Total, decoded.SCTS)
		key = fmt.Sprintf("%sparts:%x", db.ATSMSInboundPrefix, sha256.Sum256([]byte(group)))
	}
	return atInboxPart{ATDeliverSMS: decoded, key: key}, nil
}

type atSMSParts struct {
	scope  db.SMSInboundFragmentScope
	groups map[string][]db.SMSInboundFragment
	seen   map[string]bool
}

func loadATSMSParts(scope db.SMSInboundFragmentScope) (*atSMSParts, error) {
	rows, err := db.LoadSMSInboundFragments(scope)
	if err != nil {
		return nil, err
	}
	state := &atSMSParts{scope: scope, groups: make(map[string][]db.SMSInboundFragment), seen: make(map[string]bool)}
	for _, row := range rows {
		if strings.HasPrefix(row.SessionKey, db.ATSMSInboundPrefix) {
			state.groups[row.SessionKey] = append(state.groups[row.SessionKey], row)
		}
	}
	return state, nil
}

func (state *atSMSParts) persist(part atInboxPart, sourceKnown bool) (bool, *InboundSMS, error) {
	if part.Concat.IsConcat {
		state.seen[part.key] = true
		if part.Binary {
			return false, nil, nil
		}
		return state.persistFragment(part, sourceKnown)
	}
	if smsnotify.ShouldSuppressReceivedSMS(part.Content) {
		return false, nil, nil
	}
	if !sourceKnown {
		saved, found, err := db.LookupCompletedReceivedSMS(state.scope.ICCID, state.scope.IMSI, part.key)
		return found && strings.TrimSpace(part.Content) == saved.Content, nil, err
	}
	return state.commit(part, part.Content)
}

func (state *atSMSParts) persistFragment(part atInboxPart, sourceKnown bool) (bool, *InboundSMS, error) {
	fragments := state.groups[part.key]
	matched := false
	for _, saved := range fragments {
		if saved.Sequence == part.Concat.Seq {
			if saved.Content != part.Content {
				return false, nil, db.ErrSMSInboundFragmentCollision
			}
			matched = true
			break
		}
	}
	if !matched {
		// Another part with the exact session key establishes this ME
		// fragment's provenance after the worker has restarted.
		if !sourceKnown && len(fragments) == 0 {
			return false, nil, nil
		}
		scope := state.scope
		scope.SessionKey = part.key
		concat := part.Concat
		result, err := db.SaveSMSInboundFragment(scope, db.SMSInboundFragment{
			Sequence: concat.Seq, Reference: concat.Ref, ReferenceBits: concat.RefBits,
			Total: concat.Total, Content: part.Content, ArrivedAt: part.Timestamp,
		})
		if err != nil {
			return false, nil, err
		}
		fragments = result.Fragments
		state.groups[part.key] = fragments
	}
	if len(fragments) < part.Concat.Total {
		return true, nil, nil
	}
	var content strings.Builder
	for i, fragment := range fragments {
		if fragment.Sequence != i+1 || fragment.Total != part.Concat.Total {
			return false, nil, db.ErrSMSInboundFragmentCollision
		}
		content.WriteString(fragment.Content)
	}
	return state.commit(part, content.String())
}

func (state *atSMSParts) commit(part atInboxPart, content string) (bool, *InboundSMS, error) {
	timestamp := correctSMSCTimestamp(state.scope.DeviceID, state.scope.IMSI, part.Timestamp, time.Now())
	result, err := db.SaveReceivedMultipartSMS(db.ReceivedMultipartSMS{
		ICCID: state.scope.ICCID, IMSI: state.scope.IMSI,
		Sender: part.Sender, Content: content, Timestamp: timestamp, FragmentSessionKey: part.key,
	})
	if err != nil {
		return false, nil, err
	}
	if !result.Created {
		return true, nil, nil
	}
	return true, &InboundSMS{
		DeviceID: state.scope.DeviceID, ICCID: state.scope.ICCID, IMSI: state.scope.IMSI,
		Sender: part.Sender, Content: content, Time: timestamp,
	}, nil
}

func (state *atSMSParts) clearCompletedGroups() error {
	for key, fragments := range state.groups {
		if state.seen[key] || len(fragments) != fragments[0].Total {
			continue
		}
		if slices.ContainsFunc(fragments, func(part db.SMSInboundFragment) bool {
			return part.DeviceID != state.scope.DeviceID
		}) {
			continue
		}
		_, found, err := db.LookupCompletedReceivedSMS(state.scope.ICCID, state.scope.IMSI, key)
		if err != nil {
			return err
		}
		if found {
			scope := state.scope
			scope.SessionKey = key
			if err := db.DeleteSMSInboundFragments(scope); err != nil {
				return err
			}
		}
	}
	return nil
}
