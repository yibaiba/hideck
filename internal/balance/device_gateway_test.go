package balance

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

func TestPoolGatewaySendVoWiFiSMSFallsBackToCSWithoutDoubleSend(t *testing.T) {
	pool := device.NewPool(&config.Config{})
	pool.AttachWorkerWithSMSIdentityForTest(&device.Worker{ID: "wwan0", Config: config.DeviceConfig{
		ID: "wwan0", VoWiFiEnabled: true, PhoneMode: "wifi",
	}}, device.SMSIdentity{ICCID: "8944100000000000001", IMSI: "234150000000001"})
	vowifiCalls, csCalls := 0, 0
	pool.SetRoutedSMSTestSenders(
		func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
			vowifiCalls++
			return messaging.SendOutcome{SIPCode: 503, RecommendCSFallback: true}, errors.New("503")
		},
		func(string, string, string) error { csCalls++; return nil },
	)
	if err := NewPoolGateway(pool).SendVoWiFiSMS(context.Background(), DeviceSnapshot{DeviceID: "wwan0", ICCID: "8944100000000000001"}, "85075", "INFO"); err != nil {
		t.Fatalf("SendVoWiFiSMS() = %v", err)
	}
	if vowifiCalls != 1 || csCalls != 1 {
		t.Fatalf("calls vowifi=%d cs=%d", vowifiCalls, csCalls)
	}
}

func TestPoolGatewaySendVoWiFiSMSDoesNotFallbackAfterIMSAccept(t *testing.T) {
	pool := device.NewPool(&config.Config{})
	pool.AttachWorkerWithSMSIdentityForTest(&device.Worker{ID: "wwan0", Config: config.DeviceConfig{
		ID: "wwan0", VoWiFiEnabled: true, PhoneMode: "wifi",
	}}, device.SMSIdentity{ICCID: "8944100000000000001", IMSI: "234150000000001"})
	vowifiCalls, csCalls := 0, 0
	pool.SetRoutedSMSTestSenders(
		func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
			vowifiCalls++
			return messaging.SendOutcome{SIPCode: 403, RecommendCSFallback: false}, errors.New("403")
		},
		func(string, string, string) error { csCalls++; return nil },
	)
	if err := NewPoolGateway(pool).SendVoWiFiSMS(context.Background(), DeviceSnapshot{DeviceID: "wwan0", ICCID: "8944100000000000001"}, "85075", "INFO"); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Fatalf("expected IMS 403 to stay on VoWiFi, got %v", err)
	}
	if vowifiCalls != 1 || csCalls != 0 {
		t.Fatalf("calls vowifi=%d cs=%d", vowifiCalls, csCalls)
	}
}

func TestPoolGatewayRejectsSMSAfterCardChanges(t *testing.T) {
	pool := device.NewPool(&config.Config{})
	worker := &device.Worker{ID: "wwan0", Pool: pool}
	pool.AttachWorkerWithSMSIdentityForTest(worker, device.SMSIdentity{ICCID: "8944100000000000002", IMSI: "234150000000002"})
	gateway := NewPoolGateway(pool)
	stale := DeviceSnapshot{DeviceID: "wwan0", ICCID: "8944100000000000001"}
	for _, send := range []func(context.Context, DeviceSnapshot, string, string) error{gateway.SendVoWiFiSMS, gateway.SendBackendSMS} {
		if err := send(context.Background(), stale, "85075", "INFO"); !errors.Is(err, device.ErrSMSSelectorMismatch) {
			t.Fatalf("send with stale ICCID = %v", err)
		}
	}
}
