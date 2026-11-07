package policy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestDefaultIMSRegisterTemplate(t *testing.T) {
	template := DefaultIMSRegisterTemplate()
	if template.ID != "defaultIMSRegisterTemplate" || template.Expires != 600000 ||
		template.SecAgreeMode != "auto" || template.RegisterPolicy.TemporaryRetrySeconds != 60 {
		t.Fatalf("default template = %+v", template)
	}
	if len(template.SecurityClientMechanisms) != 6 || template.SecurityClientMechanisms[0].Alg != "hmac-md5-96" {
		t.Fatalf("security mechanisms = %+v", template.SecurityClientMechanisms)
	}
	if template.SMSReceiverTransport != "" || NormalizeIMSRegisterTemplate(template).SMSReceiverTransport != "dual" {
		t.Fatalf("receiver defaults = raw %q normalized %q", template.SMSReceiverTransport, NormalizeIMSRegisterTemplate(template).SMSReceiverTransport)
	}
	if !reflect.DeepEqual(template.ContactParamOrder, defaultContactParams) {
		t.Fatalf("default contact order = %v", template.ContactParamOrder)
	}
	if template.SupportedHeader != "path,sec-agree,outbound" {
		t.Fatalf("default Supported = %q", template.SupportedHeader)
	}
}

func TestNormalizeIMSRegisterPolicy(t *testing.T) {
	input := IMSRegisterPolicy{
		ID: " custom ", TemporaryStatusCodes: []int{99, 503, 503, 700},
		ForbiddenStatusCodes: []int{}, InitialRejectFallbackStatusCodes: []int{400, 400},
		TemporaryRetrySeconds: 0,
	}
	got := NormalizeIMSRegisterPolicy(input)
	if got.ID != "custom" || !reflect.DeepEqual(got.TemporaryStatusCodes, []int{503}) ||
		len(got.ForbiddenStatusCodes) != 0 || !reflect.DeepEqual(got.InitialRejectFallbackStatusCodes, []int{400}) ||
		got.TemporaryRetrySeconds != 0 {
		t.Fatalf("NormalizeIMSRegisterPolicy() = %+v", got)
	}
	if got := NormalizeIMSRegisterPolicy(IMSRegisterPolicy{}); !isZeroIMSRegisterPolicy(got) {
		t.Fatalf("zero policy changed: %+v", got)
	}
}

func TestFallbackIMSRegisterTemplate(t *testing.T) {
	template := DefaultIMSRegisterTemplate()
	template.SecurityClientIncludesServerParams = true
	template.FallbackIncludesServerParamsInSecCl = false
	fallback := FallbackIMSRegisterTemplate(template)
	if fallback.ID != "minimal_generic" || fallback.EnableInitialRejectFallback ||
		fallback.SecurityClientIncludesServerParams {
		t.Fatalf("FallbackIMSRegisterTemplate = %+v", fallback)
	}
	template.FallbackIncludesServerParamsInSecCl = true
	if fallback := FallbackIMSRegisterTemplate(template); !fallback.SecurityClientIncludesServerParams {
		t.Fatal("fallback server-parameter policy was not projected")
	}
}

func TestNormalizeCarrierValues(t *testing.T) {
	if got := NormalizeIMSDomain(" SIPS:IMS.Example.COM;transport=tcp "); got != "IMS.Example.COM" {
		t.Fatalf("NormalizeIMSDomain() = %q", got)
	}
	if got := NormalizeIMSIdentitySource("usim"); got != "derived" {
		t.Fatalf("identity = %q", got)
	}
	if got := NormalizeIMSTransport("tls"); got != "auto" {
		t.Fatalf("transport = %q", got)
	}
	if got := NormalizeSMSReceiverTransport("both"); got != "dual" {
		t.Fatalf("receiver = %q", got)
	}
	if got := NormalizeSMSRoutingMethod("ipsmgw"); got != "ip_sm_gw" {
		t.Fatalf("routing = %q", got)
	}
	if got := NormalizeCarrierDNSServer("2001:db8::1"); got != "[2001:db8::1]:53" {
		t.Fatalf("DNS = %q", got)
	}
}

func TestVoWiFiBlocklistAndErrorChain(t *testing.T) {
	for _, mcc := range []string{"460", "461"} {
		if !IsVoWiFiBlockedMCC(mcc) {
			t.Fatalf("MCC %s should be blocked", mcc)
		}
	}
	if IsVoWiFiBlockedMCC("46") || IsVoWiFiBlockedMCC("310") {
		t.Fatal("invalid or US MCC blocked")
	}
	err := NewVoWiFiBlockedMCCError("460")
	if !errors.Is(err, ErrVoWiFiPolicyBlocked) || !IsVoWiFiPolicyBlockedError(err) {
		t.Fatalf("error chain = %v", err)
	}
}

func TestResolveEmbeddedCarrierPresets(t *testing.T) {
	giffgaff := ResolveEffectiveCarrierConfig("234", "10")
	if giffgaff.PresetID != "giffgaff_23410" || giffgaff.DeviceModel != "rmx3366" ||
		giffgaff.ReauthIntervalSeconds != 0 || giffgaff.IKERekeyIntervalSeconds != 9000 ||
		giffgaff.EPDGAddrSource != "standard" ||
		giffgaff.EmergencyEPDGAddr != DefaultCarrierEmergencyEPDGAddr("234", "10") {
		t.Fatalf("giffgaff = %+v", giffgaff)
	}
	if !reflect.DeepEqual(giffgaff.IKEProposals, []string{"aes256-sha512-prfsha512-modp2048"}) {
		t.Fatalf("giffgaff IKE = %+v", giffgaff.IKEProposals)
	}
	cteUK := ResolveEffectiveCarrierConfig("234", "33")
	if cteUK.PresetID != "CTEUK_23433" ||
		cteUK.IMSRegisterTemplate.SupportedHeader != "path,sec-agree,outbound" ||
		!reflect.DeepEqual(cteUK.IMSRegisterTemplate.ContactParamOrder, []string{
			"access_type", "sip_instance", "reg_id", "audio", "smsip", "smsip_msisdn_less", "icsi_ref",
		}) || !reflect.DeepEqual(cteUK.IMSRegisterTemplate.ContactOrder,
		cteUK.IMSRegisterTemplate.ContactParamOrder) {
		t.Fatalf("CTEUK outbound registration = %+v", cteUK.IMSRegisterTemplate)
	}
	vodafoneUK := ResolveEffectiveCarrierConfig("234", "15")
	if vodafoneUK.PresetID != "vodafone_uk_23415" || vodafoneUK.DeviceModel != "rmx3366" ||
		vodafoneUK.EPDGAddr != "epdg.epc.mnc015.mcc234.pub.3gppnetwork.org" ||
		vodafoneUK.EmergencyEPDGAddr != "sos.epdg.epc.mnc015.mcc234.pub.3gppnetwork.org" ||
		vodafoneUK.EPDGAddrSource != "standard" ||
		vodafoneUK.IMSRegisterTemplate.ID != "vodafone_uk_23415" {
		t.Fatalf("vodafone uk = %+v", vodafoneUK)
	}
	if !strings.Contains(vodafoneUK.IMSRegisterTemplate.ICSIRef, "ims.icsi.sms") {
		t.Fatalf("vodafone uk ICSI missing SMS: %q", vodafoneUK.IMSRegisterTemplate.ICSIRef)
	}
	if !reflect.DeepEqual(vodafoneUK.IMSRegisterTemplate.ContactParamOrder[:6], []string{
		"access_type", "sip_instance", "audio", "smsip", "smsip_msisdn_less", "icsi_ref",
	}) {
		t.Fatalf("vodafone uk contact order = %v", vodafoneUK.IMSRegisterTemplate.ContactParamOrder)
	}
	if vodafoneUK.XCAPAPN != "xcap" {
		t.Fatalf("vodafone uk xcap APN = %q", vodafoneUK.XCAPAPN)
	}
	if extra := AdditionalPDNs(vodafoneUK); len(extra) != 1 || extra[0].APN != "xcap" {
		t.Fatalf("vodafone uk extra PDN = %+v", extra)
	}
	if spark := ResolveEffectiveCarrierConfig("530", "05"); spark.PresetID != "spark_nz_53005" ||
		!spark.WithholdDeviceIdentity || !CarrierPlanFromEffectiveConfig(spark).Device.Withhold ||
		!spark.KeepChildSAOnRekeyDecline || !CarrierPlanFromEffectiveConfig(spark).IKE.KeepChildSAOnRekeyDecline ||
		!spark.DeferPeerChildRekeySwitch || !CarrierPlanFromEffectiveConfig(spark).IKE.DeferPeerChildRekeySwitch {
		t.Fatalf("spark nz = %+v", spark)
	}
	if one := ResolveEffectiveCarrierConfig("530", "01"); one.PresetID != "one_nz_53001" ||
		!one.WithholdDeviceIdentity || !CarrierPlanFromEffectiveConfig(one).Device.Withhold {
		t.Fatalf("one nz = %+v", one)
	}
	if twoDegrees := ResolveEffectiveCarrierConfig("530", "24"); twoDegrees.PresetID != "2degrees_nz_53024" ||
		!twoDegrees.IMSRegisterTemplate.AcceptRegisterContactWithoutUser || twoDegrees.IMSRegisterTemplate.RPReportOverPortUS ||
		!CarrierPlanFromEffectiveConfig(twoDegrees).IMS.RegisterTemplate.AcceptRegisterContactWithoutUser ||
		!CarrierPlanFromEffectiveConfig(twoDegrees).IMS.RegisterTemplate.RefreshOffersNewSecurityClient {
		t.Fatalf("2degrees nz = %+v", twoDegrees.IMSRegisterTemplate)
	}
	if vodafoneDE := ResolveEffectiveCarrierConfig("262", "02"); vodafoneDE.PresetID != "vodafone_de_26202" ||
		!CarrierPlanFromEffectiveConfig(vodafoneDE).IMS.RegisterTemplate.DisableMWISubscription {
		t.Fatalf("vodafone de = %+v", vodafoneDE.IMSRegisterTemplate)
	}
	if telekomDE := ResolveEffectiveCarrierConfig("262", "01"); CarrierPlanFromEffectiveConfig(telekomDE).IMS.RegisterTemplate.DisableMWISubscription {
		t.Fatalf("telekom de disables MWI: %+v", telekomDE.IMSRegisterTemplate)
	}
	if dito := ResolveEffectiveCarrierConfig("515", "66"); dito.PresetID != "dito_51566" ||
		!CarrierPlanFromEffectiveConfig(dito).IMS.RegisterTemplate.RefreshOffersNewSecurityClient {
		t.Fatalf("dito ph = %+v", dito.IMSRegisterTemplate)
	}
	for _, plmn := range [][2]string{{"234", "15"}, {"515", "02"}, {"530", "01"}, {"530", "05"}, {"234", "87"}} {
		if cfg := ResolveEffectiveCarrierConfig(plmn[0], plmn[1]); cfg.IMSRegisterTemplate.RefreshOffersNewSecurityClient {
			t.Fatalf("%s refresh_offers_new_security_client is on", cfg.PresetID)
		}
	}
	if spark := ResolveEffectiveCarrierConfig("530", "05"); spark.IMSRegisterTemplate.AcceptRegisterContactWithoutUser {
		t.Fatal("spark nz accepts a Contact without the user part")
	}
	for _, plmn := range [][2]string{{"234", "15"}, {"515", "02"}, {"530", "01"}, {"530", "05"}, {"234", "87"}} {
		cfg := ResolveEffectiveCarrierConfig(plmn[0], plmn[1])
		want := cfg.PresetID == "vodafone_uk_23415"
		if cfg.IMSRegisterTemplate.RPReportOverPortUS != want {
			t.Fatalf("%s rp_report_over_port_us = %t, want %t", cfg.PresetID, cfg.IMSRegisterTemplate.RPReportOverPortUS, want)
		}
	}
	if globe := ResolveEffectiveCarrierConfig("515", "02"); globe.PresetID != "globe_ph_51502" ||
		globe.EPDGAddr != "weconnect.globe.com.ph" || globe.EPDGAddrSource != "preset" ||
		globe.IMSTransport != "tcp" || CarrierPlanFromEffectiveConfig(globe).IMS.Transport != "tcp" {
		t.Fatalf("globe ph = %+v", globe)
	}
	lebaraUK := ResolveEffectiveCarrierConfig("234", "87")
	if lebaraUK.PresetID != "lebara_uk_23487" || lebaraUK.DeviceModel != "rmx3366" ||
		lebaraUK.EPDGAddr != "epdg.epc.mnc087.mcc234.pub.3gppnetwork.org" ||
		lebaraUK.EPDGAddrSource != "standard" || lebaraUK.IKERekeyIntervalSeconds != 9000 ||
		!lebaraUK.WithholdDeviceIdentity || lebaraUK.KeepChildSAOnRekeyDecline || lebaraUK.DeferPeerChildRekeySwitch ||
		lebaraUK.IMSRegisterTemplate.ID != "lebara_uk_23487" {
		t.Fatalf("lebara uk = %+v", lebaraUK)
	}
	if !strings.Contains(lebaraUK.IMSRegisterTemplate.ICSIRef, "ims.icsi.sms") {
		t.Fatalf("lebara uk ICSI missing SMS: %q", lebaraUK.IMSRegisterTemplate.ICSIRef)
	}
	if !reflect.DeepEqual(lebaraUK.IMSRegisterTemplate.ContactParamOrder[:6], []string{
		"access_type", "sip_instance", "audio", "smsip", "smsip_msisdn_less", "icsi_ref",
	}) {
		t.Fatalf("lebara uk contact order = %v", lebaraUK.IMSRegisterTemplate.ContactParamOrder)
	}
	if vodafoneNL := ResolveEffectiveCarrierConfig("204", "04"); vodafoneNL.PresetID != "vodafone_nl_20404" {
		t.Fatalf("vodafone nl stolen by lebara: %+v", vodafoneNL)
	}
	if kpn := ResolveEffectiveCarrierConfig("204", "08"); kpn.PresetID != "kpn_nl_20408" {
		t.Fatalf("kpn/simyo = %+v", kpn)
	}
	if o2 := ResolveEffectiveCarrierConfig("262", "03"); o2.PresetID != "O2_de_26203" ||
		o2.EPDGAddr != "epdg.epc.mnc003.mcc262.pub.3gppnetwork.org" {
		t.Fatalf("o2 de = %+v", o2)
	}
	if o2alias := ResolveEffectiveCarrierConfig("262", "07"); o2alias.PresetID != "O2_de_26207_alias" {
		t.Fatalf("o2 de alias = %+v", o2alias)
	}
	dito := ResolveEffectiveCarrierConfig("515", "66")
	if dito.PresetID != "dito_51566" || dito.DeviceModel != "rmx3366" ||
		dito.EPDGAddr != "epdg.epc.mnc066.mcc515.pub.3gppnetwork.org" ||
		dito.EmergencyEPDGAddr != DefaultCarrierEmergencyEPDGAddr("515", "66") ||
		dito.EPDGAddrSource != "standard" ||
		dito.IMSRegisterTemplate.ID != "dito_51566" {
		t.Fatalf("dito = %+v", dito)
	}
	if !strings.Contains(dito.IMSRegisterTemplate.ICSIRef, "ims.icsi.sms") {
		t.Fatalf("dito ICSI missing SMS: %q", dito.IMSRegisterTemplate.ICSIRef)
	}
	if !reflect.DeepEqual(dito.IMSRegisterTemplate.ContactParamOrder[:6], []string{
		"access_type", "sip_instance", "audio", "smsip", "smsip_msisdn_less", "icsi_ref",
	}) {
		t.Fatalf("dito contact order = %v", dito.IMSRegisterTemplate.ContactParamOrder)
	}
	for _, item := range []struct {
		mcc, mnc, id, epdg string
	}{
		{"208", "01", "orange_fr_20801", "epdg.epc.mnc001.mcc208.pub.3gppnetwork.org"},
		{"234", "25", "oneglobal_23425", "epdg.epc.mnc025.mcc234.pub.3gppnetwork.org"},
		{"234", "26", "lycamobile_uk_23426", "epdg.epc.mnc026.mcc234.pub.3gppnetwork.org"},
		{"234", "30", "ee_uk_23430", "epdg.epc.mnc030.mcc234.pub.3gppnetwork.org"},
		{"234", "31", "ee_uk_23431", "epdg.epc.mnc031.mcc234.pub.3gppnetwork.org"},
		{"234", "32", "ee_uk_23432", "epdg.epc.mnc032.mcc234.pub.3gppnetwork.org"},
		{"262", "01", "telekom_de_26201", "epdg.epc.mnc001.mcc262.pub.3gppnetwork.org"},
		{"262", "02", "vodafone_de_26202", "epdg.epc.mnc002.mcc262.pub.3gppnetwork.org"},
		{"454", "12", "cmhk_45412", "epdg.epc.mnc012.mcc454.pub.3gppnetwork.org"},
		{"454", "13", "cmhk_45413", "epdg.epc.mnc013.mcc454.pub.3gppnetwork.org"},
		{"440", "00", "ymobile_44000", "epdg.epc.mnc000.mcc440.pub.3gppnetwork.org"},
		{"440", "10", "docomo_44010", "epdg.epc.mnc010.mcc440.pub.3gppnetwork.org"},
		{"440", "20", "softbank_44020", "epdg.epc.mnc020.mcc440.pub.3gppnetwork.org"},
		{"440", "51", "kddi_44051", "epdg.epc.mnc051.mcc440.pub.3gppnetwork.org"},
		{"248", "02", "elisa_ee_24802", "epdg.epc.mnc002.mcc248.pub.3gppnetwork.org"},
		{"204", "08", "kpn_nl_20408", "epdg.epc.mnc008.mcc204.pub.3gppnetwork.org"},
		{"502", "12", "hotlink_my_50212", "epdg.epc.mnc012.mcc502.pub.3gppnetwork.org"},
		{"515", "03", "smart_ph_51503", "epdg.epc.mnc003.mcc515.pub.3gppnetwork.org"},
		{"520", "01", "ais_th_52001", "epdg.epc.mnc001.mcc520.pub.3gppnetwork.org"},
		{"520", "03", "ais_th_52003", "epdg.epc.mnc003.mcc520.pub.3gppnetwork.org"},
		{"621", "30", "mtn_ng_62130", "epdg.epc.mnc030.mcc621.pub.3gppnetwork.org"},
	} {
		got := ResolveEffectiveCarrierConfig(item.mcc, item.mnc)
		if got.PresetID != item.id || got.DeviceModel != "rmx3366" ||
			got.EPDGAddr != item.epdg ||
			got.EmergencyEPDGAddr != DefaultCarrierEmergencyEPDGAddr(item.mcc, item.mnc) ||
			got.EPDGAddrSource != "standard" ||
			got.IMSRegisterTemplate.ID != item.id {
			t.Fatalf("%s = %+v", item.id, got)
		}
		if !strings.Contains(got.IMSRegisterTemplate.ICSIRef, "ims.icsi.sms") {
			t.Fatalf("%s ICSI missing SMS: %q", item.id, got.IMSRegisterTemplate.ICSIRef)
		}
		if !reflect.DeepEqual(got.IMSRegisterTemplate.ContactParamOrder[:6], []string{
			"access_type", "sip_instance", "audio", "smsip", "smsip_msisdn_less", "icsi_ref",
		}) {
			t.Fatalf("%s contact order = %v", item.id, got.IMSRegisterTemplate.ContactParamOrder)
		}
	}
	if cte := ResolveEffectiveCarrierConfig("234", "33"); cte.PresetID != "CTEUK_23433" {
		t.Fatalf("EE aliases must not steal CTExcel 234/33: %+v", cte)
	}
	att := ResolveEffectiveCarrierConfig("310", "280")
	if att.PresetID != "att_310280" || att.EPDGAddr != "epdg.epc.att.net" ||
		att.EmergencyEPDGAddr != DefaultCarrierEmergencyEPDGAddr("310", "280") ||
		att.IMSRegisterTemplate.RegisterPolicy.ID != "att_main" || att.IMSRegisterPolicySource != "preset" {
		t.Fatalf("AT&T = %+v", att)
	}
}

func TestCarrierOverridesAreAtomicAndClearable(t *testing.T) {
	ClearCarrierOverrides()
	t.Cleanup(ClearCarrierOverrides)
	port, disabled := 4500, false
	err := SetCarrierOverrides(map[string]CarrierOverride{
		"310-260": {
			ID: "external", CustomEPDG: "epdg.example.com", EPDGPort: &port,
			E911: E911PolicyOverride{Enabled: &disabled}, IMSDomain: "ims.example.com",
		},
	})
	if err != nil {
		t.Fatalf("SetCarrierOverrides() error = %v", err)
	}
	cfg := ResolveEffectiveCarrierConfig("310", "260")
	if cfg.PresetID != "external" || cfg.EPDGAddr != "epdg.example.com" || cfg.EPDGPort != 4500 ||
		cfg.IMSDomain != "ims.example.com" {
		t.Fatalf("override config = %+v", cfg)
	}
	if err := SetCarrierOverrides(map[string]CarrierOverride{"bad": {}}); err == nil {
		t.Fatal("invalid replacement should fail")
	}
	if got := ResolveEffectiveCarrierConfig("310", "260"); got.PresetID != "external" {
		t.Fatal("failed replacement mutated active overrides")
	}
	ClearCarrierOverrides()
	if got := ResolveEffectiveCarrierConfig("310", "260"); got.PresetID == "external" {
		t.Fatal("ClearCarrierOverrides() retained override")
	}
}

func TestCarrierOverrideStoreDeepCopiesPointers(t *testing.T) {
	ClearCarrierOverrides()
	t.Cleanup(ClearCarrierOverrides)
	port := 4500
	values := map[string]CarrierOverride{"23410": {EPDGPort: &port}}
	if err := SetCarrierOverrides(values); err != nil {
		t.Fatal(err)
	}
	port = 1
	if got := ResolveEffectiveCarrierConfig("234", "10").EPDGPort; got != 4500 {
		t.Fatalf("stored pointer was aliased: %d", got)
	}
}

func TestCarrierOverrideStoreConcurrentReplacementAndResolution(t *testing.T) {
	ClearCarrierOverrides()
	t.Cleanup(ClearCarrierOverrides)
	const iterations = 100
	var workers sync.WaitGroup
	errCh := make(chan error, 1)
	workers.Add(2)
	go func() {
		defer workers.Done()
		for index := 0; index < iterations; index++ {
			err := SetCarrierOverrides(map[string]CarrierOverride{
				"23410": {ID: "concurrent", IKEProposals: []string{"aes128-sha256-modp2048"}},
			})
			if err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for index := 0; index < iterations; index++ {
			config := ResolveEffectiveCarrierConfig("234", "10")
			if len(config.IKEProposals) > 0 {
				config.IKEProposals[0] = "caller mutation"
			}
		}
	}()
	workers.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		t.Fatalf("SetCarrierOverrides() error = %v", err)
	}
	config := ResolveEffectiveCarrierConfig("234", "10")
	if len(config.IKEProposals) == 0 || config.IKEProposals[0] == "caller mutation" {
		t.Fatalf("concurrent resolution exposed stored slices: %+v", config.IKEProposals)
	}
}

func TestLoadCarrierOverridesYAML(t *testing.T) {
	ClearCarrierOverrides()
	t.Cleanup(ClearCarrierOverrides)
	path := filepath.Join(t.TempDir(), "carrier_overrides.yaml")
	data := []byte("carrier_overrides:\n  23410:\n    id: local\n    custom_epdg: epdg.local\n    ike_rekey_interval_seconds: 7200\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := LoadCarrierOverridesFile(path)
	if err != nil {
		t.Fatalf("LoadCarrierOverridesFile() error = %v", err)
	}
	if values["234010"].ID != "local" || values["234010"].IKERekeyIntervalSeconds != 7200 {
		t.Fatalf("values = %+v", values)
	}
	resolved, count, missing, err := LoadAndSetCarrierOverridesFile(path)
	if err != nil || resolved != path || count != 1 || missing {
		t.Fatalf("LoadAndSetCarrierOverridesFile() = %q %d %t %v", resolved, count, missing, err)
	}
	if got := ResolveEffectiveCarrierConfig("234", "10").IKERekeyIntervalSeconds; got != 7200 {
		t.Fatalf("resolved IKE rekey interval = %d, want 7200", got)
	}
}

func TestCarrierPlanRoundTripAndSliceIsolation(t *testing.T) {
	config := ResolveEffectiveCarrierConfig("234", "10")
	plan := CarrierPlanFromEffectiveConfig(config)
	back := EffectiveCarrierConfigFromCarrierPlan(plan)
	if back.MCC != config.MCC || back.PresetID != config.PresetID ||
		back.IKERekeyIntervalSeconds != config.IKERekeyIntervalSeconds ||
		!reflect.DeepEqual(back.IKEProposals, config.IKEProposals) {
		t.Fatalf("round trip = %+v", back)
	}
	plan.IKE.IKEProposals[0] = "changed"
	if config.IKEProposals[0] == "changed" {
		t.Fatal("plan aliases effective config")
	}
}

func TestIMSRegisterTemplateJSONCompatibility(t *testing.T) {
	var recovered IMSRegisterTemplate
	if err := json.Unmarshal([]byte(`{"Domain":"ims.example","RegisterPolicy":{"ID":"temporary"},"SecAgreeMode":"required"}`), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Domain != "ims.example" || recovered.RegisterPolicy.ID != "temporary" || recovered.SecAgreeMode != "required" {
		t.Fatalf("recovered JSON = %+v", recovered)
	}
	var interim IMSRegisterTemplate
	if err := json.Unmarshal([]byte(`{"RegisterPolicy":"manual","SecAgreeMode":true}`), &interim); err != nil {
		t.Fatal(err)
	}
	if interim.RegisterPolicyMode != "manual" || !interim.SecAgreeEnabled {
		t.Fatalf("interim JSON = %+v", interim)
	}
}
