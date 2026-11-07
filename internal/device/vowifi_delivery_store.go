package device

import (
	"errors"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/yibaiba/hideck/internal/db"
	"gorm.io/gorm"
)

type vowifiDeliveryStore struct{}

func (vowifiDeliveryStore) LoadIMSSubscriptionRejection(
	identity, eventPackage string,
	now time.Time,
) (int, time.Time, error) {
	return db.LoadIMSSubscriptionRejection(identity, eventPackage, now)
}

func (vowifiDeliveryStore) SaveIMSSubscriptionRejection(
	identity, eventPackage string,
	status int,
	expiresAt time.Time,
) error {
	return db.SaveIMSSubscriptionRejection(identity, eventPackage, status, expiresAt)
}

func (vowifiDeliveryStore) DeleteIMSSubscriptionRejections(identity string) error {
	return db.DeleteIMSSubscriptionRejections(identity)
}

// LoadMWISubscriptionPolicy reads the card's MWI setting and any remembered
// network rejection. A card without a policy row follows the carrier preset.
func (vowifiDeliveryStore) LoadMWISubscriptionPolicy(iccid, identity string) (string, int, error) {
	mode := db.MWIModeAuto
	policy, err := db.GetCardPolicy(iccid)
	switch {
	case err == nil:
		mode = db.NormalizeMWIMode(policy.MWIMode)
	case !errors.Is(err, db.ErrCardPolicyNotFound):
		return "", 0, err
	}
	if db.CanonicalICCID(iccid) == "" {
		return mode, 0, nil
	}
	status, err := db.LoadIMSMWIRejection(iccid, identity)
	return mode, status, err
}

func (vowifiDeliveryStore) SaveMWISubscriptionRejection(iccid, identity string, status int) error {
	return db.SaveIMSMWIRejection(iccid, identity, status)
}

func (vowifiDeliveryStore) CreateSMSDelivery(
	messageID, imsi, deviceID, peer, content string,
	partsTotal int,
	at time.Time,
) error {
	return db.CreateSMSDelivery(db.SMSDeliveryCreate{
		MessageID: messageID, IMSI: imsi, DeviceID: deviceID,
		Peer: peer, Content: content, PartsTotal: partsTotal, At: at,
	})
}

func (vowifiDeliveryStore) CreateSMSDeliveryForIdentity(record messaging.DeliveryRecord) error {
	return db.CreateSMSDelivery(db.SMSDeliveryCreate{
		MessageID: record.MessageID, ICCID: record.ICCID, IMSI: record.IMSI,
		DeviceID: record.DeviceID, Peer: record.Peer, Content: record.Content,
		PartsTotal: record.PartsTotal, At: record.At,
	})
}

func (vowifiDeliveryStore) UpsertSMSDeliveryPart(messageID string, partNo int, callID string, rpMR int, state string, sentAt time.Time) error {
	return db.UpsertSMSDeliveryPart(messageID, partNo, callID, rpMR, state, sentAt)
}

func (vowifiDeliveryStore) MarkSMSDeliveryPartSIPResult(
	messageID string,
	partNo, sipCode int,
	state, errText string,
	at time.Time,
) error {
	return db.MarkSMSDeliveryPartSIPResult(messageID, partNo, sipCode, state, errText, at)
}

func (vowifiDeliveryStore) LoadInboundFragments(
	owner messaging.InboundFragmentOwner,
) ([]messaging.StoredInboundFragment, error) {
	rows, err := db.LoadSMSInboundFragments(db.SMSInboundFragmentScope{
		DeviceID: owner.DeviceID, ICCID: owner.ICCID, IMSI: owner.IMSI,
	})
	if err != nil {
		return nil, err
	}
	result := make([]messaging.StoredInboundFragment, 0, len(rows))
	for _, row := range rows {
		// The rows already match the owner's canonical ICCID and IMSI. Hand
		// the owner back as given so imscore's exact owner check accepts
		// fragments saved on another device or under a padded ICCID.
		fragment := inboundFragmentFromDB(row)
		fragment.Scope.Owner = owner
		result = append(result, fragment)
	}
	return result, nil
}

func (vowifiDeliveryStore) SaveInboundFragment(
	scope messaging.InboundFragmentScope,
	fragment messaging.InboundFragment,
) (messaging.InboundFragmentSaveResult, error) {
	dbResult, err := db.SaveSMSInboundFragment(fragmentScopeToDB(scope), inboundFragmentToDB(fragment))
	result := messaging.InboundFragmentSaveResult{
		Inserted: dbResult.Inserted, CollisionReason: dbResult.CollisionReason,
		Fragments: make([]messaging.InboundFragment, 0, len(dbResult.Fragments)),
	}
	for _, row := range dbResult.Fragments {
		result.Fragments = append(result.Fragments, inboundFragmentValueFromDB(row))
	}
	if errors.Is(err, db.ErrSMSInboundFragmentCollision) {
		err = errors.Join(messaging.ErrInboundFragmentCollision, err)
	}
	return result, err
}

func (vowifiDeliveryStore) DeleteInboundFragments(scope messaging.InboundFragmentScope) error {
	return db.DeleteSMSInboundFragments(fragmentScopeToDB(scope))
}

func (vowifiDeliveryStore) MarkInboundFragmentAcked(
	scope messaging.InboundFragmentScope,
	sequence int,
	at time.Time,
) error {
	return db.MarkSMSInboundFragmentAcked(fragmentScopeToDB(scope), sequence, at)
}

func (vowifiDeliveryStore) MarkInboundFragmentsDegraded(
	scope messaging.InboundFragmentScope,
	at time.Time,
) error {
	return db.MarkSMSInboundFragmentsDegraded(fragmentScopeToDB(scope), at)
}

func fragmentScopeToDB(scope messaging.InboundFragmentScope) db.SMSInboundFragmentScope {
	return db.SMSInboundFragmentScope{
		DeviceID: scope.Owner.DeviceID, ICCID: scope.Owner.ICCID,
		IMSI: scope.Owner.IMSI, SessionKey: scope.SessionKey,
	}
}

func inboundFragmentToDB(fragment messaging.InboundFragment) db.SMSInboundFragment {
	return db.SMSInboundFragment{
		Reference: fragment.Reference, ReferenceBits: fragment.ReferenceBits,
		Total: fragment.Total, Sequence: fragment.Sequence, Content: fragment.Content,
		ArrivedAt: fragment.ArrivedAt, RPMR: fragment.RPMR, CallID: fragment.CallID,
		ToURI: fragment.ToURI, ServiceCenter: fragment.ServiceCenter,
		AckSent: fragment.AckSent, AckSentAt: fragment.AckSentAt,
		DegradedAt: fragment.DegradedAt,
	}
}

func inboundFragmentFromDB(row db.SMSInboundFragment) messaging.StoredInboundFragment {
	return messaging.StoredInboundFragment{
		Scope: messaging.InboundFragmentScope{
			Owner:      messaging.InboundFragmentOwner{DeviceID: row.DeviceID, ICCID: row.ICCID, IMSI: row.IMSI},
			SessionKey: row.SessionKey,
		},
		Fragment: inboundFragmentValueFromDB(row),
	}
}

func inboundFragmentValueFromDB(row db.SMSInboundFragment) messaging.InboundFragment {
	return messaging.InboundFragment{
		Reference: row.Reference, ReferenceBits: row.ReferenceBits,
		Total: row.Total, Sequence: row.Sequence, Content: row.Content,
		ArrivedAt: row.ArrivedAt, RPMR: row.RPMR, CallID: row.CallID,
		ToURI: row.ToURI, ServiceCenter: row.ServiceCenter,
		AckSent: row.AckSent, AckSentAt: row.AckSentAt,
		DegradedAt: row.DegradedAt,
	}
}

func (vowifiDeliveryStore) MarkSMSDeliveryPartReport(inReplyTo, callID, deviceID string, rpMR int, state string, sipCode int, rpCause int, errText string, at time.Time) (messaging.DeliveryPartMatch, error) {
	part, err := db.MarkSMSDeliveryPartReport(inReplyTo, callID, deviceID, rpMR, state, sipCode, rpCause, errText, at)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return messaging.DeliveryPartMatch{}, messaging.ErrDeliveryNotFound
		}
		return messaging.DeliveryPartMatch{}, err
	}
	return messaging.DeliveryPartMatch{
		MessageID: part.MessageID,
		PartNo:    part.PartNo,
		State:     part.State,
		Matched:   true,
	}, nil
}

func (vowifiDeliveryStore) RecomputeSMSDelivery(messageID string, at time.Time) error {
	return db.RecomputeSMSDelivery(messageID, at)
}

func (vowifiDeliveryStore) UpdateSMSDeliveryState(messageID, state, lastError string, acks int, at time.Time) error {
	return db.UpdateSMSDeliveryState(messageID, state, lastError, acks, at)
}

func (vowifiDeliveryStore) GetSMSDeliveryStatus(messageID string) (*messaging.DeliveryStatus, error) {
	status, err := db.GetSMSDeliveryStatus(messageID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, messaging.ErrDeliveryNotFound
		}
		return nil, err
	}
	out := &messaging.DeliveryStatus{
		MessageID:  status.MessageID,
		IMSI:       status.IMSI,
		ICCID:      status.ICCID,
		DeviceID:   status.DeviceID,
		Peer:       status.Peer,
		Content:    status.Content,
		PartsTotal: status.PartsTotal,
		Acks:       status.Acks,
		State:      status.State,
		LastError:  status.LastError,
		CreatedAt:  status.CreatedAt,
		UpdatedAt:  status.UpdatedAt,
		Parts:      make([]messaging.DeliveryPartStatus, 0, len(status.Parts)),
	}
	for _, p := range status.Parts {
		out.Parts = append(out.Parts, messaging.DeliveryPartStatus{
			PartNo:      p.PartNo,
			CallID:      p.CallID,
			InReplyTo:   p.InReplyTo,
			RPMR:        p.RPMR,
			State:       p.State,
			SIPCode:     p.SIPCode,
			RPCause:     p.RPCause,
			RPCauseText: messaging.RPCauseText(p.RPCause),
			ErrorText:   p.ErrorText,
			SentAt:      p.SentAt,
			ReportAt:    p.ReportAt,
			CreatedAt:   p.CreatedAt,
			UpdatedAt:   p.UpdatedAt,
		})
	}
	return out, nil
}
