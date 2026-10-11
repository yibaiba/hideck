package device

import (
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
)

func TestSMSIdentityConvergesAfterSIMSwapAndReturn(t *testing.T) {
	initDevicePhoneNumberTestDB(t)
	pool := NewPool(nil)
	t.Cleanup(func() { _ = pool.Shutdown() })
	source := &workerStartupIdentityBackendStub{}
	worker := &Worker{ID: "sim-swap", Pool: pool, Backend: source,
		Config: config.DeviceConfig{ModemIMEI: "860000000000034"}}
	worker.state.Identity.IMEI = worker.Config.ModemIMEI
	pool.workers[worker.ID] = worker
	pool.PersistRuntimeState(worker)
	previous := SMSIdentity{}
	for _, current := range []SMSIdentity{
		{ICCID: "CARD-A", IMSI: "234150000000001"},
		{ICCID: "CARD-B", IMSI: "234150000000002"},
		{ICCID: "CARD-A", IMSI: "234150000000001"},
	} {
		source.liveICCID, source.liveIMSI = current.ICCID, current.IMSI
		ready, err := pool.refreshPostSwitchIdentityWithPolling(worker.ID, worker, esimSwitchContext{
			ICCIDBefore: previous.ICCID, IMSIBefore: previous.IMSI, TargetICCID: current.ICCID,
		}, time.Second, time.Millisecond)
		if err != nil || !ready {
			t.Fatalf("switch ready=%v err=%v", ready, err)
		}
		got, err := pool.ResolveSMSIdentity(worker.ID)
		if err != nil || got != current {
			t.Fatalf("after switch: identity=%+v want=%+v err=%v", got, current, err)
		}
		if err := worker.processSMS("10086", current.ICCID, time.Now()); err != nil {
			t.Fatal(err)
		}
		previous = current
	}
	for _, card := range []string{"CARD-A", "CARD-B"} {
		contacts, err := db.GetSMSContactsByICCID(card, 10, nil, "")
		if err != nil || len(contacts) != 1 || contacts[0].ICCID != card || contacts[0].LastContent != card {
			t.Fatalf("card=%s contacts=%+v err=%v", card, contacts, err)
		}
	}
}
