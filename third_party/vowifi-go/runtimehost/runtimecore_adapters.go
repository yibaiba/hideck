package runtimehost

import (
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/events"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/smsdelivery"
	"github.com/iniwex5/vowifi-go/runtimehost/eventhost"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
)

func runtimeCoreDispatcher(dispatcher eventhost.Dispatcher) events.EventDispatcher {
	if dispatcher == nil {
		return nil
	}
	return eventDispatcherAdapter{dispatch: dispatcher}
}

func runtimeCoreSubscriptionRejectionStore(
	store messaging.DeliveryStore,
) imscore.SubscriptionRejectionPersistence {
	persistence, _ := store.(messaging.SubscriptionRejectionStore)
	return persistence
}

func runtimeCoreMWIPolicyStore(store messaging.DeliveryStore) imscore.MWISubscriptionPolicyStore {
	policy, _ := store.(messaging.MWISubscriptionPolicyStore)
	return policy
}

type runtimeCoreDeliveryStoreAdapter struct{ store messaging.DeliveryStore }

type runtimeCoreSIPDeliveryStoreAdapter struct {
	runtimeCoreDeliveryStoreAdapter
	store messaging.SIPResultStore
}

type runtimeCoreFragmentCapability struct {
	store messaging.InboundFragmentStore
}

type runtimeCoreFragmentLifecycleCapability struct {
	runtimeCoreFragmentCapability
	lifecycle messaging.InboundFragmentLifecycleStore
}

type runtimeCoreFragmentStoreAdapter struct {
	runtimeCoreDeliveryStoreAdapter
	runtimeCoreFragmentCapability
}

type runtimeCoreLifecycleStoreAdapter struct {
	runtimeCoreDeliveryStoreAdapter
	runtimeCoreFragmentLifecycleCapability
}

type runtimeCoreCompleteStoreAdapter struct {
	runtimeCoreSIPDeliveryStoreAdapter
	runtimeCoreFragmentCapability
}

type runtimeCoreCompleteLifecycleStoreAdapter struct {
	runtimeCoreSIPDeliveryStoreAdapter
	runtimeCoreFragmentLifecycleCapability
}

func runtimeCoreDeliveryStore(store messaging.DeliveryStore) smsdelivery.Store {
	if store == nil {
		return nil
	}
	base := runtimeCoreDeliveryStoreAdapter{store: store}
	sipResults, hasSIPResults := store.(messaging.SIPResultStore)
	fragments, hasFragments := store.(messaging.InboundFragmentStore)
	lifecycle, hasLifecycle := store.(messaging.InboundFragmentLifecycleStore)
	switch {
	case hasSIPResults && hasFragments && hasLifecycle:
		return runtimeCoreCompleteLifecycleStoreAdapter{
			runtimeCoreSIPDeliveryStoreAdapter: runtimeCoreSIPDeliveryStoreAdapter{
				runtimeCoreDeliveryStoreAdapter: base, store: sipResults,
			},
			runtimeCoreFragmentLifecycleCapability: runtimeCoreFragmentLifecycleCapability{
				runtimeCoreFragmentCapability: runtimeCoreFragmentCapability{store: fragments},
				lifecycle:                     lifecycle,
			},
		}
	case hasSIPResults && hasFragments:
		return runtimeCoreCompleteStoreAdapter{
			runtimeCoreSIPDeliveryStoreAdapter: runtimeCoreSIPDeliveryStoreAdapter{
				runtimeCoreDeliveryStoreAdapter: base, store: sipResults,
			},
			runtimeCoreFragmentCapability: runtimeCoreFragmentCapability{store: fragments},
		}
	case hasSIPResults:
		return runtimeCoreSIPDeliveryStoreAdapter{
			runtimeCoreDeliveryStoreAdapter: base, store: sipResults,
		}
	case hasFragments && hasLifecycle:
		return runtimeCoreLifecycleStoreAdapter{
			runtimeCoreDeliveryStoreAdapter: base,
			runtimeCoreFragmentLifecycleCapability: runtimeCoreFragmentLifecycleCapability{
				runtimeCoreFragmentCapability: runtimeCoreFragmentCapability{store: fragments},
				lifecycle:                     lifecycle,
			},
		}
	case hasFragments:
		return runtimeCoreFragmentStoreAdapter{
			runtimeCoreDeliveryStoreAdapter: base,
			runtimeCoreFragmentCapability:   runtimeCoreFragmentCapability{store: fragments},
		}
	default:
		return base
	}
}

func (adapter runtimeCoreFragmentLifecycleCapability) MarkInboundFragmentsDegraded(
	scope smsdelivery.InboundFragmentScope,
	at time.Time,
) error {
	return adapter.lifecycle.MarkInboundFragmentsDegraded(scope, at)
}

func (adapter runtimeCoreFragmentCapability) LoadInboundFragments(
	owner smsdelivery.InboundFragmentOwner,
) ([]smsdelivery.StoredInboundFragment, error) {
	return adapter.store.LoadInboundFragments(owner)
}

func (adapter runtimeCoreFragmentCapability) SaveInboundFragment(
	scope smsdelivery.InboundFragmentScope,
	fragment smsdelivery.InboundFragment,
) (smsdelivery.InboundFragmentSaveResult, error) {
	return adapter.store.SaveInboundFragment(scope, fragment)
}

func (adapter runtimeCoreFragmentCapability) DeleteInboundFragments(
	scope smsdelivery.InboundFragmentScope,
) error {
	return adapter.store.DeleteInboundFragments(scope)
}

func (adapter runtimeCoreFragmentCapability) MarkInboundFragmentAcked(
	scope smsdelivery.InboundFragmentScope,
	sequence int,
	at time.Time,
) error {
	return adapter.store.MarkInboundFragmentAcked(scope, sequence, at)
}

func (adapter runtimeCoreSIPDeliveryStoreAdapter) MarkSMSDeliveryPartSIPResult(
	messageID string,
	partNo, sipCode int,
	state, errorText string,
	at time.Time,
) error {
	return adapter.store.MarkSMSDeliveryPartSIPResult(
		messageID, partNo, sipCode, state, errorText, at,
	)
}

func (adapter runtimeCoreDeliveryStoreAdapter) CreateSMSDelivery(
	messageID, imsi, deviceID, peer, content string,
	partsTotal int,
	at time.Time,
) error {
	return deliveryStoreErrorToInternal(
		adapter.store.CreateSMSDelivery(messageID, imsi, deviceID, peer, content, partsTotal, at),
	)
}

func (adapter runtimeCoreDeliveryStoreAdapter) CreateSMSDeliveryForIdentity(
	record smsdelivery.DeliveryRecord,
) error {
	if store, ok := adapter.store.(messaging.IdentityDeliveryStore); ok {
		return deliveryStoreErrorToInternal(store.CreateSMSDeliveryForIdentity(record))
	}
	return adapter.CreateSMSDelivery(
		record.MessageID, record.IMSI, record.DeviceID,
		record.Peer, record.Content, record.PartsTotal, record.At,
	)
}

func (adapter runtimeCoreDeliveryStoreAdapter) UpsertSMSDeliveryPart(
	messageID string,
	partNo int,
	callID string,
	rpMR int,
	state string,
	sentAt time.Time,
) error {
	return deliveryStoreErrorToInternal(
		adapter.store.UpsertSMSDeliveryPart(messageID, partNo, callID, rpMR, state, sentAt),
	)
}

func (adapter runtimeCoreDeliveryStoreAdapter) MarkSMSDeliveryPartReport(
	inReplyTo, callID, deviceID string,
	rpMR int,
	state string,
	sipCode, rpCause int,
	errText string,
	at time.Time,
) (smsdelivery.DeliveryPartMatch, error) {
	match, err := adapter.store.MarkSMSDeliveryPartReport(
		inReplyTo, callID, deviceID, rpMR, state, sipCode, rpCause, errText, at,
	)
	return smsdelivery.DeliveryPartMatch{
		MessageID: match.MessageID, PartNo: match.PartNo, State: match.State,
		Matched: match.Matched || match.MessageID != "",
	}, deliveryStoreErrorToInternal(err)
}

func (adapter runtimeCoreDeliveryStoreAdapter) RecomputeSMSDelivery(
	messageID string,
	at time.Time,
) error {
	return deliveryStoreErrorToInternal(adapter.store.RecomputeSMSDelivery(messageID, at))
}

func (adapter runtimeCoreDeliveryStoreAdapter) UpdateSMSDeliveryState(
	messageID, state, lastError string,
	acks int,
	at time.Time,
) error {
	return deliveryStoreErrorToInternal(
		adapter.store.UpdateSMSDeliveryState(messageID, state, lastError, acks, at),
	)
}

func (adapter runtimeCoreDeliveryStoreAdapter) GetSMSDeliveryStatus(
	messageID string,
) (*smsdelivery.DeliveryStatus, error) {
	status, err := adapter.store.GetSMSDeliveryStatus(messageID)
	if err != nil || status == nil {
		return nil, deliveryStoreErrorToInternal(err)
	}
	return runtimeCoreDeliveryStatus(status), nil
}

func runtimeCoreDeliveryStatus(status *messaging.DeliveryStatus) *smsdelivery.DeliveryStatus {
	result := &smsdelivery.DeliveryStatus{
		MessageID: status.MessageID, IMSI: status.IMSI, ICCID: status.ICCID, DeviceID: status.DeviceID,
		Peer: status.Peer, Content: status.Content, PartsTotal: status.PartsTotal,
		Acks: status.Acks, State: status.State, LastError: status.LastError,
		CreatedAt: status.CreatedAt, UpdatedAt: status.UpdatedAt,
		Parts: make([]smsdelivery.DeliveryPartStatus, 0, len(status.Parts)),
	}
	for _, part := range status.Parts {
		result.Parts = append(result.Parts, runtimeCoreDeliveryPart(part))
	}
	return result
}

func runtimeCoreDeliveryPart(part messaging.DeliveryPartStatus) smsdelivery.DeliveryPartStatus {
	return smsdelivery.DeliveryPartStatus{
		PartNo: part.PartNo, CallID: part.CallID, InReplyTo: part.InReplyTo,
		RPMR: part.RPMR, State: part.State, SIPCode: part.SIPCode,
		RPCause: part.RPCause, RPCauseText: part.RPCauseText, ErrorText: part.ErrorText,
		SentAt: part.SentAt, ReportAt: part.ReportAt,
		CreatedAt: part.CreatedAt, UpdatedAt: part.UpdatedAt,
	}
}
