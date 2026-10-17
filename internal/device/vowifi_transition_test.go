package device

import (
	"context"
	"testing"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/vowifihost"
)

func TestSoftwareIMSTransitionInvalidatesOldOwnership(t *testing.T) {
	for _, stage := range []string{"idle", "starting", "active"} {
		for _, target := range []string{"volte", "modem_voice", "cellular_idle", "disable", "remove"} {
			t.Run(stage+"_to_"+target, func(t *testing.T) {
				p, w := atTestPool(t)
				host := p.voWiFiHost()
				claim := host.BeginStart(w.ID)
				if stage == "idle" {
					host.FailStart(w.ID, claim.Epoch, runtimehost.State{}, nil)
				}
				if stage == "active" && !host.ClaimStarted(w.ID, claim.Epoch, &runtimehost.Instance{}) {
					t.Fatal("initial instance rejected")
				}
				var err error
				switch target {
				case "volte", "modem_voice":
					w.Config.PhoneMode = target
					err = p.StopSoftwareIMS(w.ID)
				case "cellular_idle":
					w.Config.PhoneMode = "cellular"
					err = p.StopVoWiFiRuntimeForCellularIdle(w.ID)
				case "disable":
					err = p.DisableVoWiFi(w.ID)
				case "remove":
					err = p.AbandonDevice(w.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if host.Active(w.ID) || host.Starting(w.ID) || host.ClaimStarted(w.ID, claim.Epoch, &runtimehost.Instance{}) {
					t.Fatal("transition retained or accepted old software IMS")
				}
				if next := host.BeginStart(w.ID); !next.Accepted || next.Epoch <= claim.Epoch {
					t.Fatalf("replacement cannot start with fresh ownership: %+v", next)
				}
			})
		}
	}
}

func TestPolicyModeChangeDrainsSoftwareStartupBeforeProjection(t *testing.T) {
	p, port := testModemVoicePort(t)
	w := port.worker
	w.Config.PhoneMode = PhoneModeWiFi
	p.SetPolicyResolver(&stubPolicyResolver{pol: cardpolicy.Policy{ICCID: port.iccid, PhoneMode: PhoneModeModemVoice, VoWiFiEnabled: true}})
	started, release := make(chan context.Context, 1), make(chan struct{})
	p.voWiFiHost().SetLifecycleRunForTest(func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind == vowifihost.LifecycleCommandEnable {
			p.voWiFiHost().BeginStart(w.ID)
			started <- ctx
			<-release
		}
		return nil
	})
	enableDone := make(chan error, 1)
	go func() { enableDone <- p.voWiFiHost().Enable(p.Context(), w.ID) }()
	oldCtx := <-started
	changed := make(chan policyApplyResult, 1)
	go func() { changed <- p.resolveAndApplyPolicy(w, "handoff_test") }()
	select {
	case <-oldCtx.Done():
	case <-time.After(time.Second):
		close(release)
		t.Fatal("mode switch did not cancel startup")
	}
	if w.Config.PhoneMode != PhoneModeWiFi {
		t.Error("new policy projected while old startup could still change RF")
	}
	close(release)
	<-enableDone
	if result := <-changed; !result.Applied || result.Err != nil {
		t.Fatalf("mode transition failed: %+v", result)
	}
}

func TestNativePhoneModesRejectDelayedSoftwarePreparation(t *testing.T) {
	for _, mode := range []string{PhoneModeVoLTE, PhoneModeModemVoice} {
		t.Run(mode, func(t *testing.T) {
			p, w := atTestPool(t)
			w.Config.PhoneMode = mode
			if _, err := p.prepareVoWiFiStartContext(w.ID, "test", ""); err == nil {
				t.Fatal("native mode allowed software IMS preparation")
			}
		})
	}
}

// Card policy projection stops software IMS without restoring the radio; the
// projected policy decides the radio right after. A restore here would turn the
// radio on from the stale config for a moment.
func TestPolicyKeepsRadioOffOnlyWhenTheProjectionTurnsItOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		pol  cardpolicy.Policy
		want bool
	}{
		{"wifi calling", cardpolicy.Policy{VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi}, true},
		{"airplane", cardpolicy.Policy{AirplaneEnabled: true, PhoneMode: PhoneModeCellular}, true},
		// Restoring here also re-enables native IMS for the cellular phone.
		{"cellular on demand", cardpolicy.Policy{VoWiFiEnabled: true, PhoneMode: PhoneModeCellular, DataStrategy: "on_demand"}, false},
		{"native volte", cardpolicy.Policy{VoWiFiEnabled: true, PhoneMode: PhoneModeVoLTE}, false},
		{"phone off", cardpolicy.Policy{PhoneMode: PhoneModeWiFi}, false},
	} {
		if got := policyKeepsRadioOff(tc.pol); got != tc.want {
			t.Errorf("%s: policyKeepsRadioOff = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPolicyApplyRestoresRadioOnlyWhenTheNewPolicyKeepsItOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		pol  cardpolicy.Policy
		want bool
	}{
		{"airplane", cardpolicy.Policy{AirplaneEnabled: true, PhoneMode: PhoneModeWiFi}, false},
		{"cellular on demand", cardpolicy.Policy{VoWiFiEnabled: true, PhoneMode: PhoneModeCellular, DataStrategy: "on_demand"}, true},
	} {
		p, w := atTestPool(t)
		w.Config.PhoneMode, w.Config.VoWiFiEnabled = PhoneModeWiFi, true
		w.state.Identity.ICCID = "test-sim"
		tc.pol.ICCID = "test-sim"
		host := p.voWiFiHost()
		host.BeginStart(w.ID)
		var got *vowifihost.LifecycleCommand
		host.SetLifecycleRunForTest(func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
			if cmd.Kind == vowifihost.LifecycleCommandDisable {
				got = &cmd
			}
			return nil
		})
		p.SetPolicyResolver(&stubPolicyResolver{pol: tc.pol})
		p.resolveAndApplyPolicy(w, "test_projection")
		if got == nil || got.RestoreRadio != tc.want {
			t.Errorf("%s: disable command=%+v, want RestoreRadio=%v", tc.name, got, tc.want)
		}
	}
}

func TestPolicyProjectionStopsSoftwareIMSWithoutRestoringRadio(t *testing.T) {
	for _, restore := range []bool{true, false} {
		p, w := atTestPool(t)
		host := p.voWiFiHost()
		host.BeginStart(w.ID)
		var got *vowifihost.LifecycleCommand
		host.SetLifecycleRunForTest(func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
			if cmd.Kind == vowifihost.LifecycleCommandDisable {
				got = &cmd
			}
			return nil
		})
		var err error
		if restore {
			err = p.StopSoftwareIMS(w.ID)
		} else {
			err = p.stopSoftwareIMS(w.ID, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.RestoreRadio != restore {
			t.Fatalf("restore=%v disable command=%+v", restore, got)
		}
	}
}
