package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

func TestSendSMSRecordsUnconfirmedSubmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name      string
		fallback  bool
		sendError error
		state     string
		confirmed int
		total     int
	}{
		{
			name: "direct unknown", state: db.SMSDeliveryStateUnknown, total: 1,
			sendError: modem.ErrSMSSubmitUnknown,
		},
		{
			name: "unknown after fallback", state: db.SMSDeliveryStateUnknown, fallback: true, confirmed: 1, total: 3,
			sendError: &modem.SMSSubmitError{PartsConfirmed: 1, PartsTotal: 3, Err: modem.ErrSMSSubmitUnknown},
		},
		{
			name: "partially accepted", state: db.SMSDeliveryStatePartialAck, confirmed: 1, total: 2,
			sendError: &modem.SMSSubmitError{PartsConfirmed: 1, PartsTotal: 2, Err: errors.New("modem rejected submit")},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			openTestDB(t)
			pool := device.NewPool(&config.Config{})
			worker := &device.Worker{ID: "sms-at", Config: config.DeviceConfig{VoWiFiEnabled: test.fallback}}
			identity := device.SMSIdentity{ICCID: "8944000000000000601", IMSI: "234150000000601"}
			pool.AttachWorkerWithSMSIdentityForTest(worker, identity)
			imsCalls, csCalls := 0, 0
			pool.SetRoutedSMSTestSenders(
				func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
					imsCalls++
					return messaging.SendOutcome{MessageID: "ims-only", DeliveryState: "failed",
						RecommendCSFallback: true}, messaging.ErrSMSNotReady
				},
				func(string, string, string) error { csCalls++; return test.sendError },
			)
			server := &Server{pool: pool}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/sms/send", strings.NewReader(
				`{"device_id":"sms-at","phone":"777","message":"test"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			server.handleSendSMS(c)
			var response struct {
				Status         string `json:"status"`
				DeliveryState  string `json:"delivery_state"`
				Message        string `json:"message"`
				MessageID      string `json:"message_id"`
				PartsTotal     int    `json:"parts_total"`
				PartsConfirmed int    `json:"parts_confirmed"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusAccepted || response.Status != "uncertain" || response.DeliveryState != test.state ||
				!strings.Contains(response.Message, "请勿重发整条") || response.MessageID != "" ||
				response.PartsTotal != test.total || response.PartsConfirmed != test.confirmed || csCalls != 1 || (imsCalls == 1) != test.fallback {
				t.Fatalf("HTTP %d: %s (IMS calls %d, CS calls %d)", recorder.Code, recorder.Body, imsCalls, csCalls)
			}
			messages, err := db.GetSMSByICCID(identity.ICCID, 10)
			if err != nil || len(messages) != 1 || messages[0].Type != 2 || messages[0].Status != db.SMSStatusSubmitUnconfirmed {
				t.Fatalf("SMS history = %+v, err = %v", messages, err)
			}
		})
	}
}
