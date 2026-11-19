package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/iniwex5/vowifi-go/runtimehost/messaging"
	"github.com/yibaiba/hideck/internal/automation"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

func TestExecuteSMSFallsBackToCSWithoutDoubleSend(t *testing.T) {
	pool := device.NewPool(&config.Config{})
	worker := &device.Worker{ID: "wwan0", Config: config.DeviceConfig{
		ID: "wwan0", VoWiFiEnabled: true, PhoneMode: "wifi",
	}}
	setNestedPrivateField(t, worker, []string{"state", "Identity", "ICCID"}, "8944000000000000501")
	setNestedPrivateField(t, worker, []string{"state", "Identity", "IMSI"}, "234150000000501")
	pool.AttachWorkerForTest(worker)
	vowifiCalls, csCalls := 0, 0
	pool.SetRoutedSMSTestSenders(
		func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
			vowifiCalls++
			return messaging.SendOutcome{MessageID: "ims-1", SIPCode: 503, RecommendCSFallback: true}, errors.New("503")
		},
		func(string, string, string) error { csCalls++; return nil },
	)
	executor := &automaticTaskExecutor{server: &Server{pool: pool}}
	output, err := executor.executeSMS(context.Background(), worker, automation.Task{
		DeviceID: "wwan0", Environment: automation.EnvironmentVoWiFi,
		Payload: automation.Payload{Phone: "888", Message: "BAL"},
	}, nil)
	if err != nil {
		t.Fatalf("executeSMS() = %v", err)
	}
	if !strings.Contains(output, "回落") || !strings.Contains(output, "蜂窝") {
		t.Fatalf("output=%q", output)
	}
	if vowifiCalls != 1 || csCalls != 1 {
		t.Fatalf("calls vowifi=%d cs=%d", vowifiCalls, csCalls)
	}
}

func TestExecuteSMSDoesNotFallbackAfterIMSAccept(t *testing.T) {
	pool := device.NewPool(&config.Config{})
	worker := &device.Worker{ID: "wwan0", Config: config.DeviceConfig{
		ID: "wwan0", VoWiFiEnabled: true, PhoneMode: "wifi",
	}}
	setNestedPrivateField(t, worker, []string{"state", "Identity", "ICCID"}, "8944000000000000502")
	setNestedPrivateField(t, worker, []string{"state", "Identity", "IMSI"}, "234150000000502")
	pool.AttachWorkerForTest(worker)
	csCalls := 0
	pool.SetRoutedSMSTestSenders(
		func(context.Context, string, string, string, smscodec.SubmitOptions) (messaging.SendOutcome, error) {
			return messaging.SendOutcome{SIPCode: 403, RecommendCSFallback: false}, errors.New("403")
		},
		func(string, string, string) error { csCalls++; return nil },
	)
	executor := &automaticTaskExecutor{server: &Server{pool: pool}}
	_, err := executor.executeSMS(context.Background(), worker, automation.Task{
		DeviceID: "wwan0", Environment: automation.EnvironmentVoWiFi,
		Payload: automation.Payload{Phone: "888", Message: "BAL"},
	}, nil)
	if err == nil {
		t.Fatal("expected 403 to stay on VoWiFi")
	}
	if csCalls != 0 {
		t.Fatalf("cs calls=%d", csCalls)
	}
}

func TestExecuteSMSRejectsChangedTaskProfile(t *testing.T) {
	for _, environment := range []string{automation.EnvironmentVoWiFi, automation.EnvironmentCellular} {
		t.Run(environment, func(t *testing.T) {
			pool := device.NewPool(&config.Config{})
			worker := &device.Worker{ID: "wwan0", Pool: pool}
			original := device.SMSIdentity{ICCID: "8944000000000000501", IMSI: "234150000000501"}
			pool.AttachWorkerWithSMSIdentityForTest(worker, original)
			executor := &automaticTaskExecutor{server: &Server{pool: pool}}
			_, err := executor.executeSMS(context.Background(), worker, automation.Task{
				DeviceID: worker.ID, ProfileICCID: original.ICCID, Environment: environment,
				Payload: automation.Payload{Phone: "888", Message: "BAL"},
			}, func(string) error {
				pool.AttachWorkerWithSMSIdentityForTest(worker, device.SMSIdentity{
					ICCID: "8944000000000000502", IMSI: "234150000000502",
				})
				return nil
			})
			if !errors.Is(err, device.ErrSMSSelectorMismatch) {
				t.Fatalf("submit SMS after changing profile: %v", err)
			}
		})
	}
}

func TestVoWiFiReadyForSMSTaskUsesMOReadiness(t *testing.T) {
	state := runtimehost.State{
		IMSReady: true, SMSMOReady: true,
		SMSReadyReason: "IMS SMS receiver is not ready",
	}
	if !voWiFiReadyForTask(state, true) {
		t.Fatalf("MO-ready runtime blocked SMS task: %+v", state)
	}
	if voWiFiReadyForTask(runtimehost.State{IMSReady: true}, true) {
		t.Fatal("SMS task started without MO readiness")
	}
}
