package device

import (
	"errors"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/db"
)

type smsIdentityStoreStub struct {
	identities map[string]SMSIdentity
	lookupErr  error
	saveErr    error
	saved      []savedInboundSMS
}

type savedInboundSMS struct {
	identity SMSIdentity
	message  inboundSMSRecord
}

func (s *smsIdentityStoreStub) LookupDeviceIdentity(selector db.SMSDeviceSelector) (SMSIdentity, bool, error) {
	if s.lookupErr != nil {
		return SMSIdentity{}, false, s.lookupErr
	}
	key := selector.DeviceID
	if selector.IMEI != "" {
		key = selector.IMEI
	}
	identity, ok := s.identities[key]
	return identity, ok, nil
}

func (s *smsIdentityStoreStub) SaveReceived(identity SMSIdentity, message inboundSMSRecord) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, savedInboundSMS{identity: identity, message: message})
	return nil
}

func smsTestPool(deviceID string, identity SMSIdentity) (*Pool, *smsIdentityStoreStub) {
	store := &smsIdentityStoreStub{identities: map[string]SMSIdentity{deviceID: identity}}
	return &Pool{workers: make(map[string]*Worker), smsIdentities: store}, store
}

func setWorkerSMSIdentity(worker *Worker, identity SMSIdentity) {
	worker.state.Identity.ICCID = identity.ICCID
	worker.state.Identity.IMSI = identity.IMSI
	worker.state.Identity.Ready = true
	worker.state.Identity.Phase = simIdentityPhaseReady
}

func TestResolveSMSIdentityUsesStoredBindingWithoutWorker(t *testing.T) {
	pool, _ := smsTestPool("offline-1", SMSIdentity{ICCID: "89440001", IMSI: "23410001"})

	identity, err := pool.ResolveSMSIdentity("offline-1")

	if err != nil {
		t.Fatalf("ResolveSMSIdentity() error=%v", err)
	}
	if identity.ICCID != "89440001" || identity.IMSI != "23410001" {
		t.Fatalf("identity=%+v", identity)
	}
}

func TestResolveSMSIdentityRejectsSIMSwapConflict(t *testing.T) {
	pool, store := smsTestPool("dev-1", SMSIdentity{ICCID: "old-card", IMSI: "old-imsi"})
	worker := &Worker{ID: "dev-1", Pool: pool}
	setWorkerSMSIdentity(worker, SMSIdentity{ICCID: "new-card", IMSI: "new-imsi"})
	pool.workers[worker.ID] = worker

	_, err := pool.ResolveSMSIdentity(worker.ID)

	if !errors.Is(err, ErrSMSIdentityConflict) {
		t.Fatalf("ResolveSMSIdentity() error=%v, want conflict", err)
	}
	if err := worker.processSMS("10086", "must stay", time.Now()); !errors.Is(err, ErrSMSIdentityConflict) {
		t.Fatalf("processSMS() error=%v, want conflict", err)
	}
	if len(store.saved) != 0 {
		t.Fatalf("saved=%v, want no cross-card insert", store.saved)
	}
}

func TestProcessSMSSameHistoricalIMSIKeepsDeviceICCID(t *testing.T) {
	store := &smsIdentityStoreStub{identities: map[string]SMSIdentity{
		"dev-a": {ICCID: "card-a", IMSI: "shared-imsi"},
		"dev-b": {ICCID: "card-b", IMSI: "shared-imsi"},
	}}
	pool := &Pool{workers: make(map[string]*Worker), smsIdentities: store}
	for _, deviceID := range []string{"dev-a", "dev-b"} {
		worker := &Worker{ID: deviceID, Pool: pool}
		setWorkerSMSIdentity(worker, store.identities[deviceID])
		pool.workers[deviceID] = worker
		if err := worker.processSMS("service", deviceID, time.Now()); err != nil {
			t.Fatalf("%s processSMS() error=%v", deviceID, err)
		}
	}
	if len(store.saved) != 2 || store.saved[0].identity.ICCID != "CARD-A" || store.saved[1].identity.ICCID != "CARD-B" {
		t.Fatalf("saved identities=%+v", store.saved)
	}
}

func TestResolveSMSIdentityPinsRuntimeIMEI(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "not persisted", true: "persisted"}[persisted], func(t *testing.T) {
			pool, store := smsTestPool("reused", SMSIdentity{ICCID: "OLD", IMSI: "old-imsi"})
			current := SMSIdentity{ICCID: "NEW", IMSI: "new-imsi"}
			if persisted {
				store.identities["current-imei"] = current
			}
			worker := &Worker{ID: "reused", Pool: pool}
			worker.Config.ModemIMEI = "old-configured-imei"
			setWorkerSMSIdentity(worker, current)
			worker.state.Identity.IMEI = "current-imei"
			pool.workers[worker.ID] = worker
			got, err := pool.ResolveSMSIdentity(worker.ID)
			if err != nil || got != current {
				t.Fatalf("identity=%+v err=%v", got, err)
			}
			if err := worker.processSMS("10086", "current modem", time.Now()); err != nil {
				t.Fatal(err)
			}
			if len(store.saved) != 1 || store.saved[0].identity != current {
				t.Fatalf("saved=%+v", store.saved)
			}
		})
	}
}

func TestResolveSMSIdentityRetainsPhysicalModemConflictChecks(t *testing.T) {
	pool, store := smsTestPool("reused", SMSIdentity{})
	store.identities["current-imei"] = SMSIdentity{ICCID: "OLD", IMSI: "old-imsi"}
	worker := &Worker{ID: "reused", Pool: pool}
	setWorkerSMSIdentity(worker, SMSIdentity{ICCID: "NEW", IMSI: "new-imsi"})
	worker.state.Identity.IMEI = "current-imei"
	pool.workers[worker.ID] = worker
	if _, err := pool.ResolveSMSIdentity(worker.ID); !errors.Is(err, ErrSMSIdentityConflict) {
		t.Fatalf("same physical modem conflict=%v", err)
	}
	worker.state.Identity.Phase = simIdentityPhaseTransitioning
	if _, err := pool.ResolveSMSIdentity(worker.ID); !errors.Is(err, ErrSMSIdentityTransitioning) {
		t.Fatalf("transition error=%v", err)
	}
}

func TestResolveSMSIdentityPCSCDoesNotUseStaleModemIMEI(t *testing.T) {
	pool, store := smsTestPool("reader", SMSIdentity{ICCID: "READER-CARD", IMSI: "reader-imsi"})
	store.identities["stale-imei"] = SMSIdentity{ICCID: "MODEM-CARD", IMSI: "modem-imsi"}
	worker := &Worker{ID: "reader", Pool: pool, Backend: &pcscDeviceBackend{}}
	worker.Config.DeviceBackend = backend.BackendPCSC
	worker.state.Identity.IMEI = "stale-imei"
	setWorkerSMSIdentity(worker, store.identities["reader"])
	pool.workers[worker.ID] = worker
	got, err := pool.ResolveSMSIdentity(worker.ID)
	if err != nil || got != store.identities["reader"] {
		t.Fatalf("PCSC identity=%+v err=%v", got, err)
	}
}
