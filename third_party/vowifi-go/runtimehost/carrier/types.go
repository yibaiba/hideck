// Package carrier resolves the effective carrier (PLMN) configuration for a
// VoWiFi session: e911 availability, IMS registration template and VoWiFi
// policy (blocked MCCs).
//
// Reconstructed from the decompiled engine/runtimehost/carrier.
package carrier

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// EffectiveCarrierConfigInput selects the carrier by PLMN.
type EffectiveCarrierConfigInput struct {
	MCC string
	MNC string
}

// E911Policy is the recovered emergency-address policy. Websheet and
// EntitlementEndpoint preserve the current endpoint-oriented surface.
type E911Policy struct {
	Enabled             bool   `json:"enabled"`
	Provider            string `json:"provider"`
	EntitlementURL      string `json:"entitlement_url"`
	WebsheetHostPolicy  string `json:"websheet_host_policy"`
	Websheet            string `json:"websheet"`
	EntitlementEndpoint string `json:"entitlement_endpoint"`
}

type E911Config = E911Policy

type IPSec3GPPSecurityMechanism struct {
	Alg  string
	EAlg string
	Prot string
	Mode string
}

type IMSRegisterPolicy struct {
	ID                               string
	TemporaryStatusCodes             []int
	ForbiddenStatusCodes             []int
	InitialRejectFallbackStatusCodes []int
	TemporaryRetrySeconds            int
}

// IMSRegisterTemplate is the IMS registration template for the carrier.
type IMSRegisterTemplate struct {
	ID                                  string
	UsePlainDigestPlaceholder           bool
	Expires                             int
	SMSReceiverTransport                string
	ContactMode                         string
	FixedPANI                           string
	SupportedHeader                     string
	AllowHeader                         string
	AccessType                          string
	ICSIRef                             string
	ContactParamOrder                   []string
	VoiceSupportedHeader                string
	VoiceAllowHeader                    string
	VoiceAcceptContact                  string
	VoicePPreferredService              string
	ForceHeaderPort5060                 bool
	IncludePANIAuthenticated            bool
	IncludeConnectionKeepaliveInAuth    bool
	SecAgreeMode                        string
	SecurityClientIncludesServerParams  bool
	SecurityClientMechanisms            []IPSec3GPPSecurityMechanism
	StrictSecurityServerOffer           bool
	AcceptRegisterContactWithoutUser    bool
	RPReportOverPortUS                  bool
	RefreshOffersNewSecurityClient      bool
	EnableInitialRejectFallback         bool
	DisableMWISubscription              bool
	FallbackIncludesServerParamsInSecCl bool
	RegisterPolicy                      IMSRegisterPolicy

	ExpiresSeconds int
	Transport      string
	ContactOrder   []string
	expiresSet     bool
}

// UnmarshalJSON records whether expiry was explicitly present so a JSON zero
// cannot be mistaken for an omitted override value.
func (t *IMSRegisterTemplate) UnmarshalJSON(data []byte) error {
	type templateAlias IMSRegisterTemplate
	var decoded templateAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*t = IMSRegisterTemplate(decoded)
	for name := range fields {
		if strings.EqualFold(name, "ExpiresSeconds") {
			t.expiresSet = true
			break
		}
	}
	return nil
}

// EffectiveCarrierConfig is the resolved carrier configuration.
type EffectiveCarrierConfig struct {
	MCC                           string
	MNC                           string
	PresetID                      string
	MatchedTemplate               string
	E911                          E911Policy
	IPStackType                   string
	EPDGAddr                      string
	EPDGAddrSource                string
	EmergencyEPDGAddr             string
	EPDGPort                      uint16
	APN                           string
	DNSServer                     string
	XCAPAPN                       string
	NATKeepaliveSeconds           int
	DPDIntervalSeconds            int
	AKAChallengeMode              string
	IKEIdentityMode               string
	AKAIdentityMode               string
	IKEProposals                  []string
	ESPProposals                  []string
	EnableLegacyCiphers           bool
	AllowedLegacyCiphers          []string
	AlgorithmPolicy               string
	DeviceIdentityIMEI            string
	DeviceIdentityEnabled         bool
	DeviceModel                   string
	IMSDomain                     string
	IMSRealm                      string
	IMSRegistrar                  string
	IMSPCSCF                      string
	IMSUserAgent                  string
	IMSTransport                  string
	IMSIdentitySource             string
	IMSLocalPort                  int
	IMSTCPKeepaliveSeconds        int
	IMSOptionsPingIntervalSeconds int
	DPDKeepaliveIntervalSeconds   int
	ReauthIntervalSeconds         int
	IKERekeyIntervalSeconds       int
	WithholdDeviceIdentity        bool
	KeepChildSAOnRekeyDecline     bool
	DeferPeerChildRekeySwitch     bool
	IMSRegisterTemplate           IMSRegisterTemplate
	IMSRegisterPolicySource       string
	SMSRoutingMethod              string
	SMSRoutingGW                  string
	ForceSMSCAuth                 bool

	IMS IMSRegisterTemplate
}

// CarrierOverride overrides a carrier's configuration at runtime.
type CarrierOverride struct {
	WithholdDeviceIdentity    *bool
	KeepChildSAOnRekeyDecline *bool
	DeferPeerChildRekeySwitch *bool
	MCC                       string
	MNC                       string
	PresetID                  string
	DeviceModel               string
	IKEProposals              []string
	ESPProposals              []string
	ReauthIntervalSeconds     int
	IKERekeyIntervalSeconds   int
	E911                      E911Config
	IMS                       IMSRegisterTemplate
}

// ErrVoWiFiBlockedMCC is returned when the carrier's MCC is blocked for
// VoWiFi.
type ErrVoWiFiBlockedMCC struct {
	MCC string
}

func (e *ErrVoWiFiBlockedMCC) Error() string {
	return "vowifi blocked for MCC " + e.MCC
}

// NewVoWiFiBlockedMCCError returns an error indicating VoWiFi is blocked for
// the given MCC.
func NewVoWiFiBlockedMCCError(mcc string) error {
	return &ErrVoWiFiBlockedMCC{MCC: mcc}
}

// IsVoWiFiPolicyBlockedError reports whether err is a VoWiFi policy block.
func IsVoWiFiPolicyBlockedError(err error) bool {
	var e *ErrVoWiFiBlockedMCC
	return errors.As(err, &e)
}

// blockedMCCs is the set of MCCs where VoWiFi is not offered.
var blockedMCCs = map[string]bool{
	// China Mobile: VoWiFi not offered on the standard IMS path.
	"460": true,
}

// IsVoWiFiBlockedMCC reports whether VoWiFi is blocked for the given MCC.
func IsVoWiFiBlockedMCC(mcc string) bool {
	return blockedMCCs[mcc]
}

// LoadResult describes the outcome of loading carrier overrides.
type LoadResult struct {
	Path    string
	Missing bool
	Count   int
}
