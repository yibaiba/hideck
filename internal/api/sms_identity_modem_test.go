package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
)

func TestSMSContactsAfterPhysicalModemReplacement(t *testing.T) {
	for _, online := range []bool{true, false} {
		name := "offline"
		if online {
			name = "online"
		}
		t.Run(name, func(t *testing.T) {
			server, worker := smsReplacedModemServer(t)
			if online {
				setNestedPrivateField(t, server.pool, []string{"workers"}, map[string]*device.Worker{worker.ID: worker})
			}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/sms/contacts", server.handleGetSMSContacts)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sms/contacts?device_id=issue34-modem", nil))
			var contacts []SMSContactWithDevice
			if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &contacts) != nil {
				t.Fatalf("HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
			if len(contacts) != 1 || contacts[0].ICCID != "CARD-NEW" {
				t.Fatalf("contacts=%+v", contacts)
			}
			old, err := db.GetSMSContactsByICCID("CARD-OLD", 10, nil, "")
			if err != nil || len(old) != 1 {
				t.Fatalf("old card history changed: %+v err=%v", old, err)
			}
			if online {
				got, status, message := server.resolveSMSSendWorker("", "", "CARD-NEW")
				if status != 0 || got != worker {
					t.Fatalf("send worker=%v status=%d message=%s", got, status, message)
				}
			}
		})
	}
}

func smsReplacedModemServer(t *testing.T) (*Server, *device.Worker) {
	t.Helper()
	previousDB := db.DB
	if err := db.Init(filepath.Join(t.TempDir(), "modem-replacement.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB = previousDB })
	path := writeDeviceMgmtDiscoveryConfig(t, "devices:\n- id: issue34-modem\n  modem_imei: '860000000000002'\n  device_backend: qmi\n")
	if err := config.InitGlobalManager(path); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ imei, iccid, imsi string }{
		{"860000000000001", "CARD-OLD", "imsi-old"},
		{"860000000000002", "CARD-NEW", "imsi-new"},
	} {
		if err := db.DB.Create(&db.Device{IMEI: row.imei, Alias: "issue34-modem", CurrentICCID: &row.iccid}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.DB.Create(&db.SIMCard{ICCID: row.iccid, IMSI: row.imsi}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.SaveSMSForIdentity(db.SMSRecord{
			Identity: db.SMSIdentity{ICCID: row.iccid, IMSI: row.imsi}, Sender: "10086",
			Content: row.iccid, Type: 1, Timestamp: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	pool := device.NewPool(&config.Config{})
	t.Cleanup(func() { _ = pool.Shutdown() })
	worker := &device.Worker{ID: "issue34-modem", Pool: pool}
	for field, value := range map[string]string{"IMEI": "860000000000002", "ICCID": "CARD-NEW", "IMSI": "imsi-new", "Phase": "ready"} {
		setNestedPrivateField(t, worker, []string{"state", "Identity", field}, value)
	}
	return &Server{pool: pool}, worker
}

func TestSMSWorkerOwnershipIgnoresPreviousPhysicalModem(t *testing.T) {
	_, worker := smsReplacedModemServer(t)
	setNestedPrivateField(t, worker, []string{"state", "Identity", "ICCID"}, "")
	if smsWorkerMayOwnICCID(worker, "CARD-OLD") {
		t.Fatal("historical alias must not make this worker own the old modem's SIM")
	}
	if !smsWorkerMayOwnICCID(worker, "CARD-NEW") {
		t.Fatal("current modem's stored binding must still identify the worker")
	}
}

func TestSMSOfflineMissingCurrentModemDoesNotUseOldAlias(t *testing.T) {
	server, _ := smsReplacedModemServer(t)
	if err := db.DB.Where("imei = ?", "860000000000002").Delete(&db.Device{}).Error; err != nil {
		t.Fatal(err)
	}
	_, status, _ := server.resolveSMSICCID("issue34-modem", "")
	if status != http.StatusBadRequest {
		t.Fatalf("unknown current modem status=%d, must not serve historical alias", status)
	}
}
