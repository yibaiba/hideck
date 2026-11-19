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
	"sync"
	"time"

	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/internal/smsnotify"
	"github.com/yibaiba/hideck/pkg/logger"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

var storedSMSStores = [...]string{"ME", "SM"}

type storedSMSOwner struct {
	workerLifecycleOwner
	SMSIdentity
	simGeneration uint64
}

func (w *Worker) storedSMSOwner() (storedSMSOwner, error) {
	w.cacheMu.RLock()
	id := w.state.Identity
	w.cacheMu.RUnlock()
	identity := normalizeSMSIdentity(SMSIdentity{ICCID: id.ICCID, IMSI: id.IMSI})
	if !id.Ready || id.Phase != simIdentityPhaseReady || identity.ICCID == "" || identity.IMSI == "" {
		return storedSMSOwner{}, ErrSMSIdentityUnknown
	}
	return storedSMSOwner{
		workerLifecycleOwner: workerLifecycleOwner{worker: w, generation: w.generation},
		SMSIdentity:          identity, simGeneration: id.Generation,
	}, nil
}

func (owner storedSMSOwner) check() error {
	w := owner.worker
	if !w.Pool.workerLifecycleOwnerCurrent(owner.workerLifecycleOwner) ||
		w.Pool.IsESIMSwitching(w.ID) || !w.Modem.StoredSMSReceiving() {
		return ErrSMSIdentityTransitioning
	}
	current, err := w.storedSMSOwner()
	if err != nil || current != owner {
		return ErrSMSIdentityTransitioning
	}
	return nil
}

type storedSMSIdentityReader interface {
	QuerySIMInserted() (bool, error)
	QueryICCID() (string, error)
	QueryIMSI() (string, error)
}

func (owner storedSMSOwner) verifyLive() error {
	return owner.verifyLiveFrom(owner.worker.Modem)
}

func (owner storedSMSOwner) verifyLiveFrom(reader storedSMSIdentityReader) error {
	if err := owner.check(); err != nil {
		return err
	}
	inserted, err := reader.QuerySIMInserted()
	if err != nil {
		return err
	}
	if !inserted {
		return ErrSMSIdentityUnknown
	}
	iccid, err := reader.QueryICCID()
	if err != nil {
		return err
	}
	imsi, err := reader.QueryIMSI()
	if err != nil {
		return err
	}
	if err := owner.check(); err != nil {
		return err
	}
	live := normalizeSMSIdentity(SMSIdentity{ICCID: iccid, IMSI: imsi})
	switch {
	case live.ICCID == "" || live.IMSI == "":
		err = ErrSMSIdentityUnknown
	case live != owner.SMSIdentity:
		err = ErrSMSIdentityConflict
	}
	if err != nil && owner.worker.atDeviceRules().strictSIMIdentity {
		return owner.worker.rejectLiveCSSMSIdentity(owner.simGeneration, "at_sms_inbox_identity", err)
	}
	return err
}

// A CMTI only proves provenance within the current worker and SIM generation.
type storedSMSSlot struct {
	store string
	index int
}

type storedSMSNotice struct {
	owner      storedSMSOwner
	slot       storedSMSSlot
	observedAt time.Time
	pdu        string // Set under the notice lock after the first successful read.
}

type storedSMSNotices struct {
	mu    sync.Mutex
	slots map[storedSMSSlot]*storedSMSNotice
}

func (notices *storedSMSNotices) add(notice *storedSMSNotice) {
	notices.mu.Lock()
	defer notices.mu.Unlock()
	if notices.slots == nil {
		notices.slots = make(map[storedSMSSlot]*storedSMSNotice)
	}
	notices.slots[notice.slot] = notice
}

func (notices *storedSMSNotices) snapshot(owner storedSMSOwner, store string) map[int]*storedSMSNotice {
	notices.mu.Lock()
	defer notices.mu.Unlock()
	var pending map[int]*storedSMSNotice
	for slot, notice := range notices.slots {
		if notice.owner != owner {
			if notice.owner.workerLifecycleOwner == owner.workerLifecycleOwner && notice.owner.simGeneration < owner.simGeneration {
				delete(notices.slots, slot)
			}
		} else if slot.store == store {
			if pending == nil {
				pending = make(map[int]*storedSMSNotice)
			}
			pending[slot.index] = notice
		}
	}
	return pending
}

func (notices *storedSMSNotices) current(notice *storedSMSNotice) bool {
	notices.mu.Lock()
	defer notices.mu.Unlock()
	return notices.slots[notice.slot] == notice
}

func (notices *storedSMSNotices) bind(notice *storedSMSNotice, pdu string) bool {
	notices.mu.Lock()
	defer notices.mu.Unlock()
	if notice == nil || notices.slots[notice.slot] != notice {
		return false
	}
	if notice.pdu != "" && !strings.EqualFold(notice.pdu, pdu) {
		// A previously read slot has been replaced. Only its new CMTI can
		// establish provenance for the replacement.
		delete(notices.slots, notice.slot)
		return false
	}
	notice.pdu = pdu
	return true
}

func (notices *storedSMSNotices) forget(notice *storedSMSNotice) {
	if notice == nil {
		return
	}
	notices.mu.Lock()
	defer notices.mu.Unlock()
	if notices.slots[notice.slot] == notice {
		delete(notices.slots, notice.slot)
	}
}

func (w *Worker) handleStoredSMS(storage, index string) {
	slot, err := strconv.Atoi(index)
	if err != nil || slot < 0 || slot > 65535 || (storage != "ME" && storage != "SM") {
		return
	}
	// Capture before queuing the work. A delayed URC must keep its original
	// worker and SIM generation across eSIM and physical card changes.
	owner, err := w.storedSMSOwner()
	if err != nil {
		return
	}
	notice := &storedSMSNotice{
		owner: owner, slot: storedSMSSlot{store: storage, index: slot}, observedAt: time.Now(),
	}
	w.storedSMSNotices.add(notice)
	go w.receiveStoredSMS(owner, notice)
}

// scanATSMS polls the stored inbox when the active AT SMS path enables it.
func (w *Worker) scanATSMS() {
	if w.Modem == nil || !w.Modem.StoredSMSReceiving() {
		return
	}
	owner, err := w.storedSMSOwner()
	if err != nil {
		return
	}
	w.receiveStoredSMS(owner, nil)
}

func (w *Worker) receiveStoredSMS(owner storedSMSOwner, notice *storedSMSNotice) {
	var notifications []InboundSMS
	if err := w.readStoredSMSInbox(owner, notice, &notifications); err != nil {
		logger.Warn("AT 短信收件未完成", "device", w.ID, "err", err)
	}
	for _, inbound := range notifications {
		w.Pool.notifyInboundSMS(inbound)
		if notifier := w.Pool.getNotifier(); notifier != nil {
			notifySMS(notifier, inbound, "")
		}
	}
}

func (w *Worker) readStoredSMSInbox(owner storedSMSOwner, notice *storedSMSNotice, notifications *[]InboundSMS) error {
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
		if notice != nil {
			if !w.storedSMSNotices.current(notice) {
				return nil
			}
			if err := inbox.Select(notice.slot.store); err != nil {
				return err
			}
			record, err := inbox.Read(notice.slot.index)
			if err != nil {
				return err
			}
			if err := owner.verifyLive(); err != nil {
				return err
			}
			return w.ingestStoredSMSSlot(inbox, owner, state, record, notice.slot.store == "SM", notice, notifications)
		}

		var scanErr error
		scanIncomplete := false
		for _, store := range storedSMSStores {
			if err := inbox.Select(store); err != nil {
				scanErr = errors.Join(scanErr, err)
				scanIncomplete = true
				continue
			}
			// Only notices already observed before this snapshot can identify
			// a listed PDU. A newer CMTI may refer to a reused slot.
			pending := w.storedSMSNotices.snapshot(owner, store)
			records, err := inbox.List()
			if err != nil {
				scanErr = errors.Join(scanErr, err)
				scanIncomplete = true
				continue
			}
			if err := owner.verifyLive(); err != nil {
				return errors.Join(scanErr, err)
			}
			for _, record := range records {
				notice := pending[record.Index]
				delete(pending, record.Index)
				if err := w.ingestStoredSMSSlot(inbox, owner, state, record, store == "SM", notice, notifications); err != nil {
					scanErr = errors.Join(scanErr, err)
				}
			}
			for _, absent := range pending {
				w.storedSMSNotices.forget(absent)
			}
		}
		if scanIncomplete {
			return scanErr
		}
		if err := owner.check(); err != nil {
			return errors.Join(scanErr, err)
		}
		recovered, err := state.archiveSettledATFragments(time.Now())
		*notifications = append(*notifications, recovered...)
		if err != nil || scanErr != nil {
			return errors.Join(scanErr, err)
		}
		return state.clearCompletedGroups()
	})
}

func (w *Worker) ingestStoredSMSSlot(inbox *modem.SMSInbox, owner storedSMSOwner, state *atSMSParts, record modem.SMSPDURecord, sourceKnown bool, notice *storedSMSNotice, notifications *[]InboundSMS) error {
	if !record.Received() {
		w.storedSMSNotices.forget(notice)
		return nil
	}
	if err := owner.check(); err != nil {
		return err
	}
	if notice != nil && !w.storedSMSNotices.bind(notice, record.PDU) {
		return nil
	}
	part, err := decodeATInboxPart(owner.SMSIdentity, record.PDU)
	if err != nil {
		return fmt.Errorf("SMS slot %d: %w", record.Index, err)
	}
	if notice != nil {
		part.observedAt = notice.observedAt
		sourceKnown = true
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
	w.storedSMSNotices.forget(notice)
	return nil
}

type atInboxPart struct {
	smscodec.ATDeliverSMS
	key         string
	family      string
	fingerprint [32]byte
	observedAt  time.Time
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
	fingerprint := sha256.Sum256(raw)
	key := fmt.Sprintf("%ssingle:%x", db.ATSMSInboundPrefix, fingerprint)
	var family string
	concat := decoded.Concat
	if concat.IsConcat {
		if concat.Seq < 1 || concat.Seq > concat.Total {
			return atInboxPart{}, errors.New("invalid SMS fragment sequence")
		}
		group := fmt.Sprintf("%q:%q:%d:%d:%d:%s", identity.IMSI, decoded.Sender, concat.RefBits, concat.Ref, concat.Total, decoded.SCTS)
		key = fmt.Sprintf("%sparts:%x", db.ATSMSInboundPrefix, sha256.Sum256([]byte(group)))
		origin := fmt.Sprintf("%q:%q:%q:%d:%d:%d:%d:%d:%q", identity.IMSI, decoded.Sender, decoded.SMSC,
			decoded.PID, decoded.DCS, concat.RefBits, concat.Ref, concat.Total, decoded.ApplicationPorts)
		family = fmt.Sprintf("%x", sha256.Sum256([]byte(origin)))
		if decoded.ApplicationPorts != "" {
			// Application-addressed text needs an exact SCTS and port match.
			family = "app-" + family
		}
	}
	return atInboxPart{ATDeliverSMS: decoded, key: key, family: family, fingerprint: fingerprint}, nil
}

type atSMSParts struct {
	scope           db.SMSInboundFragmentScope
	groups          map[string][]db.SMSInboundFragment
	seen            map[string]bool
	observed        map[atSMSFragmentObservation]string
	blockedFamilies map[string]bool
}

type atSMSFragmentObservation struct {
	sessionKey  string
	sequence    int
	fingerprint string
}

func loadATSMSParts(scope db.SMSInboundFragmentScope) (*atSMSParts, error) {
	rows, err := db.LoadSMSInboundFragments(scope)
	if err != nil {
		return nil, err
	}
	state := &atSMSParts{
		scope: scope, groups: make(map[string][]db.SMSInboundFragment),
		seen: make(map[string]bool), observed: make(map[atSMSFragmentObservation]string), blockedFamilies: make(map[string]bool),
	}
	for _, row := range rows {
		if strings.HasPrefix(row.SessionKey, db.ATSMSInboundPrefix) {
			state.groups[row.SessionKey] = append(state.groups[row.SessionKey], row)
		}
	}
	return state, nil
}

func (state *atSMSParts) persist(part atInboxPart, sourceKnown bool) (bool, *InboundSMS, error) {
	if part.Concat.IsConcat {
		if part.Binary {
			state.seen[part.key] = true
			return false, nil, nil
		}
		if sourceKnown && part.observedAt.IsZero() {
			part.observedAt = time.Now()
		}
		stored, inbound, err := state.persistFragment(part, sourceKnown)
		if err != nil {
			// The scan saw an exact piece for this family. If it could
			// not be recorded, its older partial groups are insufficient
			// evidence for any cross-SCTS assembly in this scan.
			state.blockedFamilies[part.family] = true
		}
		return stored, inbound, err
	}
	if smsnotify.ShouldSuppressReceivedSMS(part.Content) {
		return false, nil, nil
	}
	if !sourceKnown {
		// The single-part session key already hashes the full PDU.
		found, err := db.HasATSMSReceipt(state.scope.ICCID, state.scope.IMSI, part.key)
		return found, nil, err
	}
	return state.commit(part, part.Content)
}

const (
	atSMSPartGroupPrefix  = db.ATSMSInboundPrefix + "parts:v2:"
	atSMSReassemblyWindow = 2 * time.Minute
)

func (state *atSMSParts) fragmentGroup(part atInboxPart, fingerprint string, sourceKnown bool) (string, bool, error) {
	matches := func(saved db.SMSInboundFragment) bool {
		if saved.PDUFingerprint != "" {
			return saved.PDUFingerprint == fingerprint
		}
		return sourceKnown && saved.Content == part.Content
	}
	// Older fragments only have the legacy exact-SCTS key. It remains valid
	// for exact matches, but cannot prove the new metadata for a fuzzy join.
	if fragments := state.groups[part.key]; len(fragments) != 0 {
		for _, saved := range fragments {
			if saved.Sequence == part.Concat.Seq {
				if matches(saved) {
					return part.key, true, nil
				}
				if sourceKnown {
					return "", false, db.ErrSMSInboundFragmentCollision
				}
				break
			}
		}
		if sourceKnown {
			return part.key, false, nil
		}
		return "", false, nil
	}

	prefix := atSMSPartGroupPrefix + part.family + ":" + part.SCTS + ":"
	var duplicates, candidates []string
	for key, fragments := range state.groups {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		occupied := false
		for _, saved := range fragments {
			if saved.Sequence == part.Concat.Seq {
				occupied = true
				if matches(saved) {
					duplicates = append(duplicates, key)
				}
				break
			}
		}
		// An ME slot with no CMTI can retry only an already persisted PDU.
		if sourceKnown && !occupied && len(fragments) < part.Concat.Total {
			candidates = append(candidates, key)
		}
	}
	switch {
	case len(duplicates) > 1 || len(duplicates) == 0 && len(candidates) > 1:
		return "", false, db.ErrSMSInboundFragmentCollision
	case len(duplicates) == 1:
		return duplicates[0], true, nil
	case len(candidates) == 1:
		return candidates[0], false, nil
	case sourceKnown:
		return fmt.Sprintf("%s%d:%x", prefix, part.observedAt.UnixNano(), part.fingerprint), false, nil
	default:
		return "", false, nil
	}
}

func (state *atSMSParts) persistFragment(part atInboxPart, sourceKnown bool) (bool, *InboundSMS, error) {
	fingerprint := hex.EncodeToString(part.fingerprint[:])
	key, matched, err := state.fragmentGroup(part, fingerprint, sourceKnown)
	if err != nil || key == "" {
		return false, nil, err
	}
	state.seen[key] = true
	fragments := state.groups[key]
	if !matched {
		scope := state.scope
		scope.SessionKey = key
		concat := part.Concat
		result, err := db.SaveSMSInboundFragment(scope, db.SMSInboundFragment{
			Sequence: concat.Seq, Reference: concat.Ref, ReferenceBits: concat.RefBits,
			Total: concat.Total, Content: part.Content, ArrivedAt: part.Timestamp,
			PDUFingerprint: fingerprint,
		})
		if err != nil {
			return false, nil, err
		}
		fragments = result.Fragments
		state.groups[key] = fragments
	}
	state.observed[atSMSFragmentObservation{key, part.Concat.Seq, fingerprint}] = part.Sender
	if len(fragments) < part.Concat.Total {
		// Keep the source while other exact groups can still arrive.
		return false, nil, nil
	}
	part.key, part.Content, err = completeATMultipart(key, fragments)
	if err != nil {
		return false, nil, err
	}
	part.Timestamp = fragments[0].ArrivedAt
	return state.commit(part, part.Content)
}

func (state *atSMSParts) archiveSettledATFragments(now time.Time) ([]InboundSMS, error) {
	observedSender := func(fragment db.SMSInboundFragment) string {
		return state.observed[atSMSFragmentObservation{fragment.SessionKey, fragment.Sequence, fragment.PDUFingerprint}]
	}
	var notifications []InboundSMS
	for _, fragments := range state.settledATFragments(now) {
		// Each fragment must match a PDU observed in the current scan.
		if slices.ContainsFunc(fragments, func(fragment db.SMSInboundFragment) bool {
			return observedSender(fragment) == ""
		}) {
			continue
		}
		sender := observedSender(fragments[0])
		key, content, err := completeATMultipart(fragments[0].SessionKey, fragments)
		if err != nil {
			return notifications, err
		}
		part := atInboxPart{key: key, ATDeliverSMS: smscodec.ATDeliverSMS{Sender: sender, Timestamp: fragments[0].ArrivedAt}}
		_, inbound, err := state.commit(part, content)
		if err != nil {
			return notifications, err
		}
		if inbound != nil {
			notifications = append(notifications, *inbound)
		}
		// Keep cross-SCTS source slots and exact fragment rows for retry.
	}
	return notifications, nil
}

// settledATFragments only identifies candidates. The caller must first finish
// a complete scan and commit a candidate before touching its storage slots.
func (state *atSMSParts) settledATFragments(now time.Time) [][]db.SMSInboundFragment {
	type observedGroup struct {
		at    time.Time
		parts []db.SMSInboundFragment
	}
	families := make(map[string][]observedGroup)
	for key, parts := range state.groups {
		if len(parts) == 0 || len(parts) >= parts[0].Total {
			continue
		}
		body, ok := strings.CutPrefix(key, atSMSPartGroupPrefix)
		if !ok {
			continue // Legacy groups lack the SMSC and coding metadata.
		}
		fields := strings.Split(body, ":")
		if len(fields) != 4 || strings.HasPrefix(fields[0], "app-") || state.blockedFamilies[fields[0]] {
			continue
		}
		observed, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			continue
		}
		families[fields[0]] = append(families[fields[0]], observedGroup{time.Unix(0, observed), parts})
	}

	var settled [][]db.SMSInboundFragment
	for _, groups := range families {
		slices.SortFunc(groups, func(a, b observedGroup) int { return a.at.Compare(b.at) })
		for start := 0; start < len(groups); {
			end := start + 1
			for end < len(groups) && groups[end].at.Sub(groups[end-1].at) <= atSMSReassemblyWindow {
				end++
			}
			window := groups[start:end]
			start = end
			last := window[len(window)-1].at
			if len(window) < 2 || last.Sub(window[0].at) > atSMSReassemblyWindow || now.Sub(last) < atSMSReassemblyWindow {
				continue
			}
			var parts []db.SMSInboundFragment
			for _, group := range window {
				parts = append(parts, group.parts...)
			}
			if assembled := uniqueATFragmentSet(parts); assembled != nil {
				settled = append(settled, assembled)
			}
		}
	}
	return settled
}

func uniqueATFragmentSet(parts []db.SMSInboundFragment) []db.SMSInboundFragment {
	if len(parts) != parts[0].Total {
		return nil
	}
	ordered := make([]db.SMSInboundFragment, len(parts))
	earliest, latest := parts[0].ArrivedAt, parts[0].ArrivedAt
	for _, part := range parts {
		if part.Total != len(ordered) || part.Sequence < 1 || part.Sequence > len(ordered) ||
			ordered[part.Sequence-1].Sequence != 0 {
			return nil
		}
		ordered[part.Sequence-1] = part
		if part.ArrivedAt.Before(earliest) {
			earliest = part.ArrivedAt
		}
		if part.ArrivedAt.After(latest) {
			latest = part.ArrivedAt
		}
	}
	if latest.Sub(earliest) > atSMSReassemblyWindow {
		return nil
	}
	return ordered
}

func completeATMultipart(key string, fragments []db.SMSInboundFragment) (string, string, error) {
	var content strings.Builder
	var signature strings.Builder
	family, _, versioned := strings.Cut(strings.TrimPrefix(key, atSMSPartGroupPrefix), ":")
	versioned = versioned && strings.HasPrefix(key, atSMSPartGroupPrefix)
	for i, fragment := range fragments {
		if fragment.Sequence != i+1 || fragment.Total != len(fragments) {
			return "", "", db.ErrSMSInboundFragmentCollision
		}
		content.WriteString(fragment.Content)
		if versioned {
			fields := strings.Split(strings.TrimPrefix(fragment.SessionKey, atSMSPartGroupPrefix), ":")
			if len(fields) != 4 || fields[0] != family {
				return "", "", db.ErrSMSInboundFragmentCollision
			}
			fmt.Fprintf(&signature, "%s:%q;", fields[1], fragment.Content)
		}
	}
	if versioned {
		// Independent of which part arrived first or which worker read it.
		key = fmt.Sprintf("%sparts:complete:v2:%x", db.ATSMSInboundPrefix, sha256.Sum256([]byte(family+":"+signature.String())))
	}
	return key, content.String(), nil
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
		// History dedup follows ICCID. Slot deletion also requires the
		// receipt to attest the current IMSI.
		found, err := db.HasATSMSReceipt(state.scope.ICCID, state.scope.IMSI, part.key)
		return found, nil, err
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
		completedKey, _, err := completeATMultipart(key, fragments)
		if err != nil {
			return err
		}
		found, err := db.HasATSMSReceipt(state.scope.ICCID, state.scope.IMSI, completedKey)
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
