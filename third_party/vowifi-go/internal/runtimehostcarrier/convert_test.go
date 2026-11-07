package runtimehostcarrier

import (
	"reflect"
	"strings"
	"testing"

	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
	"github.com/iniwex5/vowifi-go/runtimehost/carrier"
)

var (
	_ func(carrier.IMSRegisterTemplate) policy.IMSRegisterTemplate       = TemplateToInternal
	_ func(policy.IMSRegisterTemplate) carrier.IMSRegisterTemplate       = TemplateFromInternal
	_ func(carrier.EffectiveCarrierConfig) policy.EffectiveCarrierConfig = ToInternal
	_ func(policy.EffectiveCarrierConfig) carrier.EffectiveCarrierConfig = FromInternal
)

func TestEffectiveCarrierConfigRoundTrip(t *testing.T) {
	source := populatedCarrierConfig()
	got := FromInternal(ToInternal(source))
	if !reflect.DeepEqual(got, source) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", got, source)
	}
}

// The round trip only proves fields the fixture sets, so a new template
// field must be populated here before the round trip can catch a drop.
func TestPopulatedTemplateSetsEveryField(t *testing.T) {
	value := reflect.ValueOf(populatedTemplate("all"))
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.IsExported() && value.Field(i).IsZero() {
			t.Errorf("populatedTemplate leaves %s unset", field.Name)
		}
	}
}

func TestConversionsOwnNestedSlices(t *testing.T) {
	source := populatedCarrierConfig()
	internal := ToInternal(source)
	mutateExternalSlices(&source)
	assertInternalSlicesUnchanged(t, internal)

	external := FromInternal(internal)
	mutateInternalSlices(&internal)
	assertExternalSlicesUnchanged(t, external)
}

func TestConversionPreservesValidationErrors(t *testing.T) {
	cfg := carrier.ResolveEffectiveCarrierConfig("234", "10")
	cfg.IMS.Transport = "sctp"
	roundTrip := FromInternal(ToInternal(cfg))
	err := carrier.ValidateEffectiveCarrierConfig(roundTrip)
	if err == nil || !strings.Contains(err.Error(), "unsupported IMS transport") {
		t.Fatalf("ValidateEffectiveCarrierConfig() error = %v", err)
	}
}

func TestCurrentCarrierDefaultsSurviveRoundTrip(t *testing.T) {
	source := carrier.ResolveEffectiveCarrierConfig("234", "10")
	got := FromInternal(ToInternal(source))
	if got.PresetID != source.PresetID || got.DeviceModel != source.DeviceModel ||
		got.IKERekeyIntervalSeconds != source.IKERekeyIntervalSeconds ||
		!reflect.DeepEqual(got.IKEProposals, source.IKEProposals) ||
		!reflect.DeepEqual(got.ESPProposals, source.ESPProposals) ||
		got.IMS.ExpiresSeconds != source.IMS.ExpiresSeconds || got.IMS.Transport != source.IMS.Transport ||
		!reflect.DeepEqual(got.IMS.ContactOrder, source.IMS.ContactOrder) {
		t.Fatalf("carrier defaults changed\n got: %#v\nwant: %#v", got, source)
	}
	if err := carrier.ValidateEffectiveCarrierConfig(got); err != nil {
		t.Fatalf("ValidateEffectiveCarrierConfig() error = %v", err)
	}
}

func TestVodafoneUKXCAPAPNSurvivesRoundTrip(t *testing.T) {
	source := carrier.ResolveEffectiveCarrierConfig("234", "15")
	if source.XCAPAPN != "xcap" {
		t.Fatalf("resolved xcap APN = %q", source.XCAPAPN)
	}
	got := FromInternal(ToInternal(source))
	if got.XCAPAPN != "xcap" {
		t.Fatalf("round trip xcap APN = %q", got.XCAPAPN)
	}
}

func populatedCarrierConfig() carrier.EffectiveCarrierConfig {
	return carrier.EffectiveCarrierConfig{
		MCC: "234", MNC: "10", PresetID: "preset", MatchedTemplate: "template",
		E911: carrier.E911Policy{
			Enabled: true, Provider: "provider", EntitlementURL: "https://entitlement.example",
			WebsheetHostPolicy: "strict", Websheet: "https://websheet.example",
			EntitlementEndpoint: "https://endpoint.example",
		},
		IPStackType: "dual", EPDGAddr: "epdg.example", EPDGAddrSource: "carrier",
		EmergencyEPDGAddr: "sos.epdg.example",
		EPDGPort:          4500, APN: "ims", DNSServer: "192.0.2.53", XCAPAPN: "xcap", NATKeepaliveSeconds: 20,
		DPDIntervalSeconds: 30, AKAChallengeMode: "relay", IKEIdentityMode: "impi",
		AKAIdentityMode: "isim", IKEProposals: []string{"ike-a", "ike-b"},
		ESPProposals: []string{"esp-a", "esp-b"}, EnableLegacyCiphers: true,
		AllowedLegacyCiphers: []string{"legacy-a", "legacy-b"}, AlgorithmPolicy: "strict",
		DeviceIdentityIMEI: "490154203237518", DeviceIdentityEnabled: true, DeviceModel: "model",
		IMSDomain: "ims.example", IMSRealm: "realm.example", IMSRegistrar: "registrar.example",
		IMSPCSCF: "pcscf.example", IMSUserAgent: "user-agent", IMSTransport: "tcp",
		IMSIdentitySource: "isim", IMSLocalPort: 5060, IMSTCPKeepaliveSeconds: 40,
		IMSOptionsPingIntervalSeconds: 50, DPDKeepaliveIntervalSeconds: 60,
		ReauthIntervalSeconds: 70, IKERekeyIntervalSeconds: 80,
		WithholdDeviceIdentity: true, KeepChildSAOnRekeyDecline: true, DeferPeerChildRekeySwitch: true,
		IMSRegisterTemplate:     populatedTemplate("flattened"),
		IMSRegisterPolicySource: "preset", SMSRoutingMethod: "sip", SMSRoutingGW: "gw.example",
		ForceSMSCAuth: true, IMS: populatedTemplate("compatibility"),
	}
}

func populatedTemplate(id string) carrier.IMSRegisterTemplate {
	return carrier.IMSRegisterTemplate{
		ID: id, UsePlainDigestPlaceholder: true, Expires: 3600, SMSReceiverTransport: "tcp",
		ContactMode: "android_default", FixedPANI: "pani", SupportedHeader: "path,sec-agree",
		AllowHeader: "REGISTER,INVITE", AccessType: "wlan1", ICSIRef: "icsi",
		ContactParamOrder: []string{"access_type", "audio"}, VoiceSupportedHeader: "timer",
		VoiceAllowHeader: "INVITE", VoiceAcceptContact: "audio", VoicePPreferredService: "mmtel",
		ForceHeaderPort5060: true, IncludePANIAuthenticated: true,
		IncludeConnectionKeepaliveInAuth: true, SecAgreeMode: "required",
		SecurityClientIncludesServerParams: true,
		SecurityClientMechanisms: []carrier.IPSec3GPPSecurityMechanism{
			{Alg: "hmac-md5-96", EAlg: "aes-cbc", Prot: "esp", Mode: "trans"},
		},
		StrictSecurityServerOffer: true, AcceptRegisterContactWithoutUser: true, RPReportOverPortUS: true,
		RefreshOffersNewSecurityClient: true,
		EnableInitialRejectFallback:    true, FallbackIncludesServerParamsInSecCl: true,
		DisableMWISubscription: true,
		RegisterPolicy: carrier.IMSRegisterPolicy{
			ID: "register", TemporaryStatusCodes: []int{408, 500}, ForbiddenStatusCodes: []int{403},
			InitialRejectFallbackStatusCodes: []int{404, 488}, TemporaryRetrySeconds: 15,
		},
		ExpiresSeconds: 600, Transport: "auto", ContactOrder: []string{"access_type", "audio"},
	}
}

func mutateExternalSlices(cfg *carrier.EffectiveCarrierConfig) {
	cfg.IKEProposals[0] = "changed"
	cfg.ESPProposals[0] = "changed"
	cfg.AllowedLegacyCiphers[0] = "changed"
	cfg.IMSRegisterTemplate.ContactParamOrder[0] = "changed"
	cfg.IMSRegisterTemplate.SecurityClientMechanisms[0].Alg = "changed"
	cfg.IMSRegisterTemplate.RegisterPolicy.TemporaryStatusCodes[0] = 999
	cfg.IMS.ContactOrder[0] = "changed"
}

func mutateInternalSlices(cfg *policy.EffectiveCarrierConfig) {
	cfg.IKEProposals[0] = "changed"
	cfg.ESPProposals[0] = "changed"
	cfg.AllowedLegacyCiphers[0] = "changed"
	cfg.IMSRegisterTemplate.ContactParamOrder[0] = "changed"
	cfg.IMSRegisterTemplate.SecurityClientMechanisms[0].Alg = "changed"
	cfg.IMSRegisterTemplate.RegisterPolicy.TemporaryStatusCodes[0] = 999
	cfg.IMS.ContactOrder[0] = "changed"
}

func assertInternalSlicesUnchanged(t *testing.T, cfg policy.EffectiveCarrierConfig) {
	t.Helper()
	if cfg.IKEProposals[0] != "ike-a" || cfg.ESPProposals[0] != "esp-a" ||
		cfg.AllowedLegacyCiphers[0] != "legacy-a" ||
		cfg.IMSRegisterTemplate.ContactParamOrder[0] != "access_type" ||
		cfg.IMSRegisterTemplate.SecurityClientMechanisms[0].Alg != "hmac-md5-96" ||
		cfg.IMSRegisterTemplate.RegisterPolicy.TemporaryStatusCodes[0] != 408 ||
		cfg.IMS.ContactOrder[0] != "access_type" {
		t.Fatalf("internal config aliases external slices: %#v", cfg)
	}
}

func assertExternalSlicesUnchanged(t *testing.T, cfg carrier.EffectiveCarrierConfig) {
	t.Helper()
	if cfg.IKEProposals[0] != "ike-a" || cfg.ESPProposals[0] != "esp-a" ||
		cfg.AllowedLegacyCiphers[0] != "legacy-a" ||
		cfg.IMSRegisterTemplate.ContactParamOrder[0] != "access_type" ||
		cfg.IMSRegisterTemplate.SecurityClientMechanisms[0].Alg != "hmac-md5-96" ||
		cfg.IMSRegisterTemplate.RegisterPolicy.TemporaryStatusCodes[0] != 408 ||
		cfg.IMS.ContactOrder[0] != "access_type" {
		t.Fatalf("external config aliases internal slices: %#v", cfg)
	}
}
