package device

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/internal/vowifihost"
)

func TestWorkerSoftwareIMSBlockedChinaMCC(t *testing.T) {
	if WorkerSoftwareIMSBlocked(nil) {
		t.Fatal("nil worker")
	}
	china := &Worker{ID: "wwan1"}
	china.state.Identity.NativeMCC = "460"
	if !WorkerSoftwareIMSBlocked(china) {
		t.Fatal("MCC 460 should block software IMS")
	}
	imsiOnly := &Worker{ID: "wwan1"}
	imsiOnly.state.Identity.IMSI = "460011234567890"
	if !WorkerSoftwareIMSBlocked(imsiOnly) {
		t.Fatal("IMSI 460 should block software IMS")
	}
	vf := &Worker{ID: "wwan0"}
	vf.state.Identity.NativeMCC = "234"
	if WorkerSoftwareIMSBlocked(vf) {
		t.Fatal("MCC 234 must keep software IMS")
	}
}

func TestPhoneServiceEnabledReadsVoWiFiBit(t *testing.T) {
	if PhoneServiceEnabled(config.DeviceConfig{}) {
		t.Fatal("empty config is phone-off")
	}
	if !PhoneServiceEnabled(config.DeviceConfig{VoWiFiEnabled: true}) {
		t.Fatal("VoWiFiEnabled remains the persisted phone-on bit")
	}
}

func TestApplyPhoneRadioPolicyByMode(t *testing.T) {
	wifi := config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: PhoneModeWiFi, NetworkEnabled: true}
	applyPhoneRadioPolicy(&wifi)
	if !wifi.AirplaneEnabled || wifi.NetworkEnabled {
		t.Fatalf("WiFi calling should take RF: %+v", wifi)
	}

	cell := config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: PhoneModeCellular, DataStrategy: "always"}
	applyPhoneRadioPolicy(&cell)
	if cell.AirplaneEnabled || !cell.NetworkEnabled {
		t.Fatalf("cellular always should camp and open data: %+v", cell)
	}

	volte := config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: PhoneModeVoLTE, DataStrategy: "always"}
	applyPhoneRadioPolicy(&volte)
	if volte.AirplaneEnabled || volte.NetworkEnabled {
		t.Fatalf("volte should camp without forcing data: %+v", volte)
	}
}

func TestML307CPhonePolicyKeepsExplicitRadioSettings(t *testing.T) {
	for _, tc := range []struct {
		name, imsi, mode string
		airplane         bool
	}{
		{name: "China WiFi", imsi: "460011234567890", mode: PhoneModeWiFi},
		{name: "China cellular", imsi: "460011234567890", mode: PhoneModeCellular},
		{name: "overseas cellular", imsi: "530240000000001", mode: PhoneModeCellular},
		{name: "overseas cellular in airplane", imsi: "530240000000001", mode: PhoneModeCellular, airplane: true},
		{name: "overseas VoLTE", imsi: "530240000000001", mode: PhoneModeVoLTE},
		{name: "overseas direct dial", imsi: "530240000000001", mode: PhoneModeModemVoice, airplane: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDesiredVoWiFiTestPool(t, "ml307c", false, tc.imsi)
			t.Cleanup(p.cancel)
			w := p.GetWorker("ml307c")
			w.Modem = newML307CPhoneTestModem(t, w.ID)
			w.state.Identity.Phase = simIdentityPhaseReady
			radio := &esimSwitchRestoreBackendStub{mode: backend.BackendAT, getMode: backend.ModeOnline}
			w.Backend = radio
			pol := cardpolicy.Policy{
				ICCID: w.CurrentICCID(), VoWiFiEnabled: true, AirplaneEnabled: tc.airplane,
				NetworkEnabled: !tc.airplane, PhoneMode: tc.mode, DataStrategy: "on_demand",
			}
			resolver := &stubPolicyResolver{pol: pol}
			p.SetPolicyResolver(resolver)

			result := p.resolveAndApplyPolicy(w, "test")
			if !result.Applied || result.Err != nil {
				t.Fatalf("apply policy: %+v", result)
			}
			if w.Config.VoWiFiEnabled || p.IsNativeVoLTE(w.ID) || p.IsModemVoice(w.ID) ||
				w.Config.PhoneMode != tc.mode || w.Config.AirplaneEnabled != tc.airplane ||
				w.Config.NetworkEnabled != pol.NetworkEnabled || !w.Config.SMSEnabled {
				t.Fatalf("unexpected effective config: %+v", w.Config)
			}
			if tc.airplane {
				if len(radio.setCalls) != 1 || radio.setCalls[0] != backend.ModeRFOff {
					t.Fatalf("explicit airplane mode was not applied: %v", radio.setCalls)
				}
			} else if len(radio.setCalls) != 0 || w.cellularRadioIsSuppressed() {
				t.Fatalf("unsupported voice mode changed radio state: %v", radio.setCalls)
			}
			if resolver.pol != pol || p.currentCardPolicyAllowsVoWiFi(w, pol.ICCID, "test", LebaraUKClass{}) {
				t.Fatalf("card policy changed or still enables recovery: %+v", resolver.pol)
			}
			if !tc.airplane && (tc.mode == PhoneModeWiFi || tc.mode == PhoneModeCellular) {
				var unsupported *PhoneModeUnsupportedError
				if err := p.EnableVoWiFi(w.ID); !errors.As(err, &unsupported) {
					t.Fatalf("direct software IMS start error = %v", err)
				}
				if len(radio.setCalls) != 0 || w.cellularRadioIsSuppressed() {
					t.Fatalf("direct software IMS start changed radio state: %v", radio.setCalls)
				}
			}
		})
	}
}

func TestML307CCellularIMSCapabilityWithNetworkController(t *testing.T) {
	w := &Worker{ID: "ml307c", netOverride: &fakeController{}}
	w.Modem = newML307CPhoneTestModem(t, w.ID)
	if err := w.CheckPhoneModeSupported(PhoneModeCellular); err != nil {
		t.Fatalf("cellular IMS with a controller: %v", err)
	}
}

func TestML307COneNZWiFiPolicyStillStarts(t *testing.T) {
	p := newDesiredVoWiFiTestPool(t, "ml307c", false, "530010000000001")
	t.Cleanup(p.cancel)
	w := p.GetWorker("ml307c")
	w.Modem = newML307CPhoneTestModem(t, w.ID)
	w.state.Identity.Phase = simIdentityPhaseReady
	p.SetPolicyResolver(&stubPolicyResolver{pol: cardpolicy.Policy{
		ICCID: w.CurrentICCID(), PhoneMode: PhoneModeWiFi, VoWiFiEnabled: true,
	}})
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(_ context.Context, cmd vowifihost.LifecycleCommand) error {
		commands <- cmd
		return nil
	}
	if result := p.resolveAndApplyPolicy(w, "test"); !result.Applied || result.Err != nil {
		t.Fatalf("apply policy: %+v", result)
	}
	if !w.Config.VoWiFiEnabled || !w.Config.AirplaneEnabled || w.Config.PhoneMode != PhoneModeWiFi {
		t.Fatalf("WiFi policy changed: %+v", w.Config)
	}
	if !p.currentCardPolicyAllowsVoWiFi(w, w.CurrentICCID(), "test", LebaraUKClass{}) {
		t.Fatal("One NZ lost software IMS recovery")
	}
	select {
	case cmd := <-commands:
		if cmd.DeviceID != w.ID {
			t.Fatalf("unexpected recovery: %+v", cmd)
		}
	case <-time.After(time.Second):
		t.Fatal("WiFi recovery was not scheduled")
	}
}

func TestML307CLebaraSavedVoLTERecoversEffectiveWiFi(t *testing.T) {
	p := newDesiredVoWiFiTestPool(t, "ml307c", false, "234870000000001")
	t.Cleanup(p.cancel)
	w := p.GetWorker("ml307c")
	w.Modem = newML307CPhoneTestModem(t, w.ID)
	w.state.Identity.Phase = simIdentityPhaseReady
	pol := cardpolicy.Policy{
		ICCID: w.CurrentICCID(), PhoneMode: PhoneModeVoLTE, VoWiFiEnabled: true,
	}
	resolver := &stubPolicyResolver{pol: pol}
	p.SetPolicyResolver(resolver)
	commands := make(chan vowifihost.LifecycleCommand, 1)
	p.voWiFiHost().LifecycleControllerForTest().TestRun = func(_ context.Context, command vowifihost.LifecycleCommand) error {
		commands <- command
		return nil
	}
	if result := p.resolveAndApplyPolicy(w, "test"); !result.Applied || result.Err != nil {
		t.Fatalf("apply policy: %+v", result)
	}
	if !w.Config.VoWiFiEnabled || !w.Config.AirplaneEnabled || w.Config.NetworkEnabled ||
		w.Config.PhoneMode != PhoneModeWiFi || resolver.pol != pol {
		t.Fatalf("effective config = %+v, saved policy = %+v", w.Config, resolver.pol)
	}
	class, err := ClassifyWorkerLebaraUK(w)
	if err != nil || !class.IsLebara || !p.currentCardPolicyAllowsVoWiFi(w, pol.ICCID, "test", class) {
		t.Fatalf("saved native mode blocked effective WiFi recovery: %+v, %v", class, err)
	}
	select {
	case <-commands:
	case <-time.After(time.Second):
		t.Fatal("WiFi recovery was not scheduled")
	}
}

func newML307CPhoneTestModem(t *testing.T, id string) *modem.Manager {
	t.Helper()
	m, err := modem.NewWithATProfile(config.DeviceConfig{ID: id, DeviceBackend: "at", ATPort: "/dev/test-ml307c"}, modem.ATProfileML307C)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
