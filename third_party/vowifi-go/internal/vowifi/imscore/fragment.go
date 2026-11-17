package imscore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const (
	fragmentAuditLimit = 24
	fragmentKeySep     = "\x00"
)

type fragmentSessionIdentity struct {
	Sender        string
	ServiceCenter string
	Local         string
	Reference     int
	RefBits       int
	Total         int
}

type fragmentHandlingContext struct {
	Sender     string
	Key        string
	InterimKey string
	Fragment   *smsFragment
}

func (s *Service) handleSMSFragment(sender string, fragment *smsFragment) (string, bool, error) {
	result, err := s.handleSMSFragmentAssembly(sender, fragment)
	return result.Content, result.Complete, err
}

type fragmentAssemblyResult struct {
	Content   string
	SessionID string
	Complete  bool
}

func (s *Service) handleSMSFragmentAssembly(
	sender string,
	fragment *smsFragment,
) (fragmentAssemblyResult, error) {
	if s == nil || fragment == nil {
		return fragmentAssemblyResult{}, errors.New("imscore: nil SMS fragment")
	}
	if fragment.Total < 2 || fragment.Seq < 1 || fragment.Seq > fragment.Total {
		return fragmentAssemblyResult{}, errors.New("imscore: invalid SMS fragment bounds")
	}
	fragment.Time = time.Now()
	identity := fragmentSessionIdentity{
		Sender: sender, ServiceCenter: fragment.ServiceCenter, Local: fragment.ToURI,
		Reference: fragment.Ref, RefBits: fragment.RefBits, Total: fragment.Total,
	}
	key := buildFragmentSessionKey(identity)
	interimKey := buildInterimFragmentSessionKey(identity)
	s.startFragmentCleanup()
	duplicate, err := s.recordFragmentArrival(key, fragment)
	if err != nil {
		return fragmentAssemblyResult{}, err
	}
	if duplicate {
		return fragmentAssemblyResult{}, nil
	}
	handling := fragmentHandlingContext{
		Sender: sender, Key: key, InterimKey: interimKey, Fragment: fragment,
	}
	if store := s.inboundFragmentStore(); store != nil {
		return s.handlePersistedSMSFragment(persistedFragmentContext{
			fragmentHandlingContext: handling, Store: store,
		})
	}
	return s.handleVolatileSMSFragmentAssembly(handling)
}

func (s *Service) recordFragmentArrival(key string, fragment *smsFragment) (bool, error) {
	s.fragmentMu.Lock()
	defer s.fragmentMu.Unlock()
	s.fragmentArrivedTotal++
	if completed, found := s.fragmentRecentComplete[key]; found {
		if time.Since(completed.At) <= 2*inboundSMSFragmentTTL &&
			completedFragmentMatches(completed, fragment) {
			s.fragmentDup++
			return true, nil
		}
		delete(s.fragmentRecentComplete, key)
	}
	if expiredAt, expired := s.fragmentRecentExpired[key]; expired {
		if time.Since(expiredAt) <= 2*inboundSMSFragmentTTL {
			s.fragmentOrphanLate++
			return false, errors.New("imscore: late SMS fragment for expired session")
		}
		delete(s.fragmentRecentExpired, key)
	}
	return false, nil
}

func completedFragmentMatches(session completedSMSFragmentSession, candidate *smsFragment) bool {
	if candidate == nil {
		return false
	}
	part, found := session.Parts[candidate.Seq]
	return found && part.Total == candidate.Total && part.RPMR == candidate.RpMr &&
		strings.TrimSpace(part.Content) == strings.TrimSpace(candidate.Content)
}

func (s *Service) recordCompletedFragmentsLocked(key string, fragments []*smsFragment) {
	parts := make(map[int]completedSMSFragment, len(fragments))
	for _, fragment := range fragments {
		if fragment != nil {
			parts[fragment.Seq] = completedSMSFragment{
				Content: fragment.Content, RPMR: fragment.RpMr, Total: fragment.Total,
			}
		}
	}
	if len(parts) > 0 {
		if s.fragmentRecentComplete == nil {
			s.fragmentRecentComplete = make(map[string]completedSMSFragmentSession)
		}
		s.fragmentRecentComplete[key] = completedSMSFragmentSession{At: time.Now(), Parts: parts}
	}
}

func (s *Service) handleVolatileSMSFragmentAssembly(
	ctx fragmentHandlingContext,
) (fragmentAssemblyResult, error) {
	s.fragmentMu.Lock()
	defer s.fragmentMu.Unlock()
	fragments := s.fragmentCache[ctx.Key]
	if len(fragments) == 0 && ctx.Fragment.Seq != 1 {
		s.logOutOfOrderFirst(ctx.Sender, ctx.Fragment)
	}
	if reason, collision := detectFragmentKeyCollision(fragments, ctx.Fragment); collision {
		s.recordFragmentCollisionLocked(ctx, reason, fragments)
		return fragmentAssemblyResult{}, fmt.Errorf("imscore: SMS fragment key collision: %s", reason)
	}
	for _, existing := range fragments {
		if existing != nil && existing.Seq == ctx.Fragment.Seq {
			s.fragmentDup++
			return fragmentAssemblyResult{}, nil
		}
	}
	s.fragmentCache[ctx.Key] = append(fragments, ctx.Fragment)
	fragments = s.fragmentCache[ctx.Key]
	if len(fragments) != ctx.Fragment.Total || len(missingSMSSeqs(fragments, ctx.Fragment.Total)) != 0 {
		return fragmentAssemblyResult{}, nil
	}
	content := assembleFragmentText(fragments)
	sessionID := buildFragmentSessionInstanceID(ctx.Key, fragments)
	s.recordCompletedFragmentsLocked(ctx.Key, fragments)
	delete(s.fragmentCache, ctx.Key)
	s.fragmentAssembledOK++
	return fragmentAssemblyResult{Content: content, SessionID: sessionID, Complete: true}, nil
}

func buildFragmentSessionInstanceID(key string, fragments []*smsFragment) string {
	first, _ := fragmentBounds(fragments)
	if strings.TrimSpace(key) == "" || first.IsZero() {
		return ""
	}
	source := key + fragmentKeySep + strconv.FormatInt(first.UnixNano(), 10)
	digest := sha256.Sum256([]byte(source))
	return fmt.Sprintf("%x", digest[:])
}

func (s *Service) logOutOfOrderFirst(sender string, fragment *smsFragment) {
	deviceID := ""
	if s.cfg != nil {
		deviceID = s.cfg.DeviceID
	}
	logging.WarnRate("sms-fragment-out-of-order-first:"+deviceID, time.Duration(0),
		"IMS 长短信出现乱序首包（缓存为空时先收到非第1片）",
		"device", deviceID, "sender", normalizeFragmentIdentity(sender),
		"ref", fragment.Ref, "seq", fragment.Seq, "total", fragment.Total)
}

func (s *Service) recordFragmentCollisionLocked(
	ctx fragmentHandlingContext,
	reason string,
	fragments []*smsFragment,
) {
	delete(s.fragmentCache, ctx.Key)
	s.appendFragmentAuditFailureLocked(fragmentAuditFailure{
		At: time.Now(), Key: ctx.Key, InterimKey: ctx.InterimKey,
		Sender: normalizeFragmentIdentity(ctx.Sender), Received: len(fragments),
		Total: ctx.Fragment.Total, SeqList: fragmentSeqList(fragments), Reason: reason,
	})
}

func detectFragmentKeyCollision(fragments []*smsFragment, candidate *smsFragment) (string, bool) {
	if len(fragments) == 0 || candidate == nil {
		return "", false
	}
	if fragments[0] != nil && fragments[0].Total != candidate.Total {
		return "total_mismatch", true
	}
	for _, fragment := range fragments {
		if fragment == nil || fragment.Seq != candidate.Seq {
			continue
		}
		if strings.TrimSpace(fragment.Content) != strings.TrimSpace(candidate.Content) {
			return "sequence_content_mismatch", true
		}
	}
	return "", false
}

func (s *Service) markFragmentAcked(key string, sequence int) {
	key = strings.TrimSpace(key)
	if s == nil || key == "" || sequence < 1 {
		return
	}
	at := time.Now()
	s.fragmentPersistMu.Lock()
	defer s.fragmentPersistMu.Unlock()
	if store := s.inboundFragmentStore(); store != nil {
		if err := store.MarkInboundFragmentAcked(s.inboundFragmentScope(key), sequence, at); err != nil {
			deviceID := ""
			if s.cfg != nil {
				deviceID = s.cfg.DeviceID
			}
			logging.WarnRate("sms-fragment-ack-persist:"+deviceID, time.Minute,
				"IMS SMS fragment ACK persistence failed", "device", deviceID, "error", err)
		}
	}
	s.fragmentMu.Lock()
	defer s.fragmentMu.Unlock()
	for _, fragment := range s.fragmentCache[key] {
		if fragment == nil || fragment.Seq != sequence {
			continue
		}
		fragment.AckSent = true
		fragment.AckSentAt = at
		return
	}
}

func missingSMSSeqs(fragments []*smsFragment, total int) string {
	if total < 1 {
		return ""
	}
	present := make(map[int]bool, len(fragments))
	for _, fragment := range fragments {
		if fragment != nil {
			present[fragment.Seq] = true
		}
	}
	missing := make([]string, 0)
	for sequence := 1; sequence <= total; sequence++ {
		if !present[sequence] {
			missing = append(missing, strconv.Itoa(sequence))
		}
	}
	return strings.Join(missing, ",")
}

func fragmentSeqList(fragments []*smsFragment) string {
	sequences := make([]int, 0, len(fragments))
	for _, fragment := range fragments {
		if fragment != nil {
			sequences = append(sequences, fragment.Seq)
		}
	}
	sort.Ints(sequences)
	values := make([]string, len(sequences))
	for index, sequence := range sequences {
		values[index] = strconv.Itoa(sequence)
	}
	return strings.Join(values, ",")
}

func ackedFragmentSeqs(fragments []*smsFragment) string {
	acked := make([]*smsFragment, 0, len(fragments))
	for _, fragment := range fragments {
		if fragment != nil && fragment.AckSent {
			acked = append(acked, fragment)
		}
	}
	return fragmentSeqList(acked)
}

func fragmentCallIDList(fragments []*smsFragment) string {
	values := make([]string, 0, len(fragments))
	for _, fragment := range fragments {
		if fragment != nil && strings.TrimSpace(fragment.CallID) != "" {
			values = append(values, strings.TrimSpace(fragment.CallID))
		}
	}
	return strings.Join(values, ",")
}

func fragmentBounds(fragments []*smsFragment) (time.Time, time.Time) {
	var earliest, latest time.Time
	for _, fragment := range fragments {
		if fragment == nil || fragment.Time.IsZero() {
			continue
		}
		if earliest.IsZero() || fragment.Time.Before(earliest) {
			earliest = fragment.Time
		}
		if latest.IsZero() || fragment.Time.After(latest) {
			latest = fragment.Time
		}
	}
	return earliest, latest
}

func senderFromFragmentKey(key string) string {
	if strings.HasPrefix(key, "sender=") {
		sender, _, _ := strings.Cut(strings.TrimPrefix(key, "sender="), "|")
		return sender
	}
	sender, _, _ := strings.Cut(key, fragmentKeySep)
	return sender
}

func buildFragmentSessionKey(identity fragmentSessionIdentity) string {
	bits := identity.RefBits
	if bits != 16 {
		bits = 8
	}
	// Adjacent segments can arrive through different SMS centers. The center is
	// transport metadata, not part of the concatenated message's identity.
	return fmt.Sprintf("sender=%s|ref=%d|bits=%d|local=%s",
		normalizeFragmentIdentity(identity.Sender), identity.Reference, bits,
		normalizeFragmentIdentity(identity.Local))
}

func buildInterimFragmentSessionKey(identity fragmentSessionIdentity) string {
	reference := identity.Reference + identity.RefBits<<16
	return strings.Join([]string{
		normalizeFragmentIdentity(identity.Sender), normalizeFragmentIdentity(identity.Local),
		strconv.Itoa(reference), strconv.Itoa(identity.Total),
	}, fragmentKeySep)
}

func normalizeFragmentIdentity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Trim(value, "<>\"")
	if strings.HasPrefix(value, "sip:") || strings.HasPrefix(value, "tel:") {
		value = value[4:]
	}
	if separator := strings.IndexAny(value, ";>@"); separator >= 0 {
		value = value[:separator]
	}
	return strings.TrimSpace(value)
}

func assembleFragmentText(fragments []*smsFragment) string {
	ordered := append([]*smsFragment(nil), fragments...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left] == nil {
			return false
		}
		if ordered[right] == nil {
			return true
		}
		return ordered[left].Seq < ordered[right].Seq
	})
	var content strings.Builder
	for _, fragment := range ordered {
		if fragment != nil {
			content.WriteString(fragment.Content)
		}
	}
	return content.String()
}

func fragmentTotal(fragments []*smsFragment) int {
	for _, fragment := range fragments {
		if fragment != nil {
			return fragment.Total
		}
	}
	return 0
}

func firstSMSFragment(fragments []*smsFragment) *smsFragment {
	for _, fragment := range fragments {
		if fragment != nil {
			return fragment
		}
	}
	return nil
}
