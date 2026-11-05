package carrier

import "github.com/iniwex5/vowifi-go/internal/vowifi/policy"

// RegisterTemplateFromInternal converts and detaches an internal template.
func RegisterTemplateFromInternal(value policy.IMSRegisterTemplate) IMSRegisterTemplate {
	return IMSRegisterTemplate{
		ID: value.ID, UsePlainDigestPlaceholder: value.UsePlainDigestPlaceholder,
		Expires: value.Expires, SMSReceiverTransport: value.SMSReceiverTransport,
		ContactMode: value.ContactMode, FixedPANI: value.FixedPANI,
		SupportedHeader: value.SupportedHeader, AllowHeader: value.AllowHeader,
		AccessType: value.AccessType, ICSIRef: value.ICSIRef,
		ContactParamOrder:                   cloneStrings(value.ContactParamOrder),
		VoiceSupportedHeader:                value.VoiceSupportedHeader,
		VoiceAllowHeader:                    value.VoiceAllowHeader,
		VoiceAcceptContact:                  value.VoiceAcceptContact,
		VoicePPreferredService:              value.VoicePPreferredService,
		ForceHeaderPort5060:                 value.ForceHeaderPort5060,
		IncludePANIAuthenticated:            value.IncludePANIAuthenticated,
		IncludeConnectionKeepaliveInAuth:    value.IncludeConnectionKeepaliveInAuth,
		SecAgreeMode:                        value.SecAgreeMode,
		SecurityClientIncludesServerParams:  value.SecurityClientIncludesServerParams,
		SecurityClientMechanisms:            mechanismsFromInternal(value.SecurityClientMechanisms),
		StrictSecurityServerOffer:           value.StrictSecurityServerOffer,
		AcceptRegisterContactWithoutUser:    value.AcceptRegisterContactWithoutUser,
		RPReportOverPortUS:                  value.RPReportOverPortUS,
		RefreshOffersNewSecurityClient:      value.RefreshOffersNewSecurityClient,
		EnableInitialRejectFallback:         value.EnableInitialRejectFallback,
		DisableMWISubscription:              value.DisableMWISubscription,
		FallbackIncludesServerParamsInSecCl: value.FallbackIncludesServerParamsInSecCl,
		RegisterPolicy:                      registerPolicyFromInternal(value.RegisterPolicy),
		ExpiresSeconds:                      value.ExpiresSeconds, Transport: value.Transport,
		ContactOrder: cloneStrings(value.ContactOrder), expiresSet: value.ExpiresSeconds != 0,
	}
}

// CarrierConfigFromInternal converts and detaches an internal carrier config.
func CarrierConfigFromInternal(value policy.EffectiveCarrierConfig) EffectiveCarrierConfig {
	result := carrierConfigBaseFromInternal(value)
	copyCarrierIKEFromInternal(&result, value)
	copyCarrierIMSFromInternal(&result, value)
	return result
}

func carrierConfigBaseFromInternal(value policy.EffectiveCarrierConfig) EffectiveCarrierConfig {
	return EffectiveCarrierConfig{
		MCC: value.MCC, MNC: value.MNC, PresetID: value.PresetID,
		MatchedTemplate: value.MatchedTemplate,
		E911: E911Policy{
			Enabled: value.E911.Enabled, Provider: value.E911.Provider,
			EntitlementURL:      value.E911.EntitlementURL,
			WebsheetHostPolicy:  value.E911.WebsheetHostPolicy,
			Websheet:            value.E911.Websheet,
			EntitlementEndpoint: value.E911.EntitlementEndpoint,
		},
		IPStackType: value.IPStackType, EPDGAddr: value.EPDGAddr,
		EPDGAddrSource: value.EPDGAddrSource, EmergencyEPDGAddr: value.EmergencyEPDGAddr,
		EPDGPort: value.EPDGPort,
		APN:      value.APN, DNSServer: value.DNSServer, XCAPAPN: value.XCAPAPN,
	}
}

func copyCarrierIKEFromInternal(result *EffectiveCarrierConfig, value policy.EffectiveCarrierConfig) {
	result.NATKeepaliveSeconds = value.NATKeepaliveSeconds
	result.DPDIntervalSeconds = value.DPDIntervalSeconds
	result.AKAChallengeMode = value.AKAChallengeMode
	result.IKEIdentityMode = value.IKEIdentityMode
	result.AKAIdentityMode = value.AKAIdentityMode
	result.IKEProposals = cloneStrings(value.IKEProposals)
	result.ESPProposals = cloneStrings(value.ESPProposals)
	result.EnableLegacyCiphers = value.EnableLegacyCiphers
	result.AllowedLegacyCiphers = cloneStrings(value.AllowedLegacyCiphers)
	result.AlgorithmPolicy = value.AlgorithmPolicy
	result.DeviceIdentityIMEI = value.DeviceIdentityIMEI
	result.DeviceIdentityEnabled = value.DeviceIdentityEnabled
	result.DeviceModel = value.DeviceModel
	result.DPDKeepaliveIntervalSeconds = value.DPDKeepaliveIntervalSeconds
	result.ReauthIntervalSeconds = value.ReauthIntervalSeconds
	result.IKERekeyIntervalSeconds = value.IKERekeyIntervalSeconds
	result.WithholdDeviceIdentity = value.WithholdDeviceIdentity
	result.KeepChildSAOnRekeyDecline = value.KeepChildSAOnRekeyDecline
	result.DeferPeerChildRekeySwitch = value.DeferPeerChildRekeySwitch
}

func copyCarrierIMSFromInternal(result *EffectiveCarrierConfig, value policy.EffectiveCarrierConfig) {
	result.IMSDomain = value.IMSDomain
	result.IMSRealm = value.IMSRealm
	result.IMSRegistrar = value.IMSRegistrar
	result.IMSPCSCF = value.IMSPCSCF
	result.IMSUserAgent = value.IMSUserAgent
	result.IMSTransport = value.IMSTransport
	result.IMSIdentitySource = value.IMSIdentitySource
	result.IMSLocalPort = value.IMSLocalPort
	result.IMSTCPKeepaliveSeconds = value.IMSTCPKeepaliveSeconds
	result.IMSOptionsPingIntervalSeconds = value.IMSOptionsPingIntervalSeconds
	result.IMSRegisterTemplate = RegisterTemplateFromInternal(value.IMSRegisterTemplate)
	result.IMSRegisterPolicySource = value.IMSRegisterPolicySource
	result.SMSRoutingMethod = value.SMSRoutingMethod
	result.SMSRoutingGW = value.SMSRoutingGW
	result.ForceSMSCAuth = value.ForceSMSCAuth
	result.IMS = RegisterTemplateFromInternal(value.IMS)
}

func registerPolicyFromInternal(value policy.IMSRegisterPolicy) IMSRegisterPolicy {
	return IMSRegisterPolicy{
		ID: value.ID, TemporaryStatusCodes: cloneInts(value.TemporaryStatusCodes),
		ForbiddenStatusCodes:             cloneInts(value.ForbiddenStatusCodes),
		InitialRejectFallbackStatusCodes: cloneInts(value.InitialRejectFallbackStatusCodes),
		TemporaryRetrySeconds:            value.TemporaryRetrySeconds,
	}
}

func mechanismsFromInternal(values []policy.IPSec3GPPSecurityMechanism) []IPSec3GPPSecurityMechanism {
	result := make([]IPSec3GPPSecurityMechanism, len(values))
	for index, value := range values {
		result[index] = IPSec3GPPSecurityMechanism(value)
	}
	return result
}

func cloneStrings(values []string) []string { return append([]string(nil), values...) }
func cloneInts(values []int) []int          { return append([]int(nil), values...) }
