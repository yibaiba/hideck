package device

import (
	"context"
	"errors"
	"testing"

	"github.com/yibaiba/hideck/internal/cardpolicy"
	"github.com/yibaiba/hideck/internal/config"
)

func TestModemVoicePolicyKeepsExplicitModeAndDoesNotForceData(t *testing.T) {
	for _, imsi := range []string{"460011234567890", "234150000000001", "530240000000001"} {
		t.Run(imsi[:5], func(t *testing.T) {
			w := &Worker{ID: "test"}
			w.state.Identity.IMSI = imsi
			pol := cardpolicy.Policy{VoWiFiEnabled: true, PhoneMode: PhoneModeModemVoice, DataStrategy: "always"}
			applyTestCardPolicy(t, w, pol)
			if w.Config.PhoneMode != PhoneModeModemVoice || w.Config.AirplaneEnabled || w.Config.NetworkEnabled || w.cellularRadioIsSuppressed() {
				t.Fatalf("unexpected projection: %+v", w.Config)
			}
		})
	}
}

func TestModemVoiceCannotOverrideLebaraRadioLock(t *testing.T) {
	w := &Worker{ID: "test"}
	w.state.Identity.IMSI = "234870000000001"
	applyTestCardPolicy(t, w, cardpolicy.Policy{VoWiFiEnabled: true, PhoneMode: PhoneModeModemVoice})
	if !w.Config.AirplaneEnabled || w.Config.NetworkEnabled || !w.cellularRadioIsSuppressed() || w.Config.PhoneMode != PhoneModeWiFi {
		t.Fatalf("radio lock bypassed: %+v", w.Config)
	}
}

func TestModemVoiceExcludedFromSoftwareIMS(t *testing.T) {
	p := newDesiredVoWiFiTestPool(t, "test", true, "234150000000001")
	w := p.GetWorker("test")
	w.Config.PhoneMode = PhoneModeModemVoice
	if p.shouldReconcileVoWiFi(w) || p.ShouldRouteSMSViaVoWiFi(w.ID) {
		t.Fatal("modem voice routed into software IMS")
	}
	if err := p.EnableVoWiFi(w.ID); !errors.Is(err, ErrModemVoiceSoftwareIMS) {
		t.Fatalf("enable: %v", err)
	}
	if _, err := p.prepareVoWiFiStartContext(context.Background(), w.ID, "test", ""); !errors.Is(err, ErrModemVoiceSoftwareIMS) {
		t.Fatalf("delayed recovery: %v", err)
	}
	if shouldRetryVoWiFiAutoStart(ErrModemVoiceSoftwareIMS) {
		t.Fatal("mode mismatch must not retry software IMS")
	}
	w.Config.PhoneMode = PhoneModeWiFi
	p.SetPolicyResolver(&stubPolicyResolver{pol: cardpolicy.Policy{VoWiFiEnabled: true, PhoneMode: PhoneModeModemVoice}})
	if p.shouldReconcileVoWiFi(w) {
		t.Fatal("persisted modem mode allowed old runtime to recover")
	}
}

func TestModemVoiceRadioProjection(t *testing.T) {
	cfg := config.DeviceConfig{VoWiFiEnabled: true, PhoneMode: PhoneModeModemVoice, DataStrategy: "always", AirplaneEnabled: true}
	applyPhoneRadioPolicy(&cfg)
	if cfg.AirplaneEnabled || cfg.NetworkEnabled || !UsesModemPhoneControl(cfg.PhoneMode) {
		t.Fatalf("unexpected radio policy: %+v", cfg)
	}
}
