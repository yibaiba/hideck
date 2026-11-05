// Package runtimehostcarrier converts between the internal carrier
// configuration and the runtimehost carrier surface.
//
// Reconstructed from the decompiled internal/runtimehostcarrier.
package runtimehostcarrier

import (
	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
	"github.com/iniwex5/vowifi-go/runtimehost/carrier"
)

// ToInternal converts a runtimehost carrier config to the internal form.
func ToInternal(cfg carrier.EffectiveCarrierConfig) policy.EffectiveCarrierConfig {
	result := toInternalBase(cfg)
	toInternalIKE(&result, cfg)
	toInternalIMS(&result, cfg)
	return result
}

// FromInternal converts an internal carrier config to the runtimehost form.
func FromInternal(cfg policy.EffectiveCarrierConfig) carrier.EffectiveCarrierConfig {
	result := fromInternalBase(cfg)
	fromInternalIKE(&result, cfg)
	fromInternalIMS(&result, cfg)
	return result
}

// TemplateToInternal converts a runtimehost IMS register template to the
// internal form.
func TemplateToInternal(t carrier.IMSRegisterTemplate) policy.IMSRegisterTemplate {
	return policy.IMSRegisterTemplate{
		ID: t.ID, UsePlainDigestPlaceholder: t.UsePlainDigestPlaceholder, Expires: t.Expires,
		SMSReceiverTransport: t.SMSReceiverTransport, ContactMode: t.ContactMode, FixedPANI: t.FixedPANI,
		SupportedHeader: t.SupportedHeader, AllowHeader: t.AllowHeader, AccessType: t.AccessType,
		ICSIRef: t.ICSIRef, ContactParamOrder: cloneStrings(t.ContactParamOrder),
		VoiceSupportedHeader: t.VoiceSupportedHeader, VoiceAllowHeader: t.VoiceAllowHeader,
		VoiceAcceptContact: t.VoiceAcceptContact, VoicePPreferredService: t.VoicePPreferredService,
		ForceHeaderPort5060:                 t.ForceHeaderPort5060,
		IncludePANIAuthenticated:            t.IncludePANIAuthenticated,
		IncludeConnectionKeepaliveInAuth:    t.IncludeConnectionKeepaliveInAuth,
		SecAgreeMode:                        t.SecAgreeMode,
		SecurityClientIncludesServerParams:  t.SecurityClientIncludesServerParams,
		SecurityClientMechanisms:            mechanismsToInternal(t.SecurityClientMechanisms),
		StrictSecurityServerOffer:           t.StrictSecurityServerOffer,
		AcceptRegisterContactWithoutUser:    t.AcceptRegisterContactWithoutUser,
		RPReportOverPortUS:                  t.RPReportOverPortUS,
		RefreshOffersNewSecurityClient:      t.RefreshOffersNewSecurityClient,
		EnableInitialRejectFallback:         t.EnableInitialRejectFallback,
		DisableMWISubscription:              t.DisableMWISubscription,
		FallbackIncludesServerParamsInSecCl: t.FallbackIncludesServerParamsInSecCl,
		RegisterPolicy:                      registerPolicyToInternal(t.RegisterPolicy),
		ExpiresSeconds:                      t.ExpiresSeconds, Transport: t.Transport, ContactOrder: cloneStrings(t.ContactOrder),
	}
}

// TemplateFromInternal converts an internal IMS register template to the
// runtimehost form.
func TemplateFromInternal(t policy.IMSRegisterTemplate) carrier.IMSRegisterTemplate {
	return carrier.IMSRegisterTemplate{
		ID: t.ID, UsePlainDigestPlaceholder: t.UsePlainDigestPlaceholder, Expires: t.Expires,
		SMSReceiverTransport: t.SMSReceiverTransport, ContactMode: t.ContactMode, FixedPANI: t.FixedPANI,
		SupportedHeader: t.SupportedHeader, AllowHeader: t.AllowHeader, AccessType: t.AccessType,
		ICSIRef: t.ICSIRef, ContactParamOrder: cloneStrings(t.ContactParamOrder),
		VoiceSupportedHeader: t.VoiceSupportedHeader, VoiceAllowHeader: t.VoiceAllowHeader,
		VoiceAcceptContact: t.VoiceAcceptContact, VoicePPreferredService: t.VoicePPreferredService,
		ForceHeaderPort5060:                 t.ForceHeaderPort5060,
		IncludePANIAuthenticated:            t.IncludePANIAuthenticated,
		IncludeConnectionKeepaliveInAuth:    t.IncludeConnectionKeepaliveInAuth,
		SecAgreeMode:                        t.SecAgreeMode,
		SecurityClientIncludesServerParams:  t.SecurityClientIncludesServerParams,
		SecurityClientMechanisms:            mechanismsFromInternal(t.SecurityClientMechanisms),
		StrictSecurityServerOffer:           t.StrictSecurityServerOffer,
		AcceptRegisterContactWithoutUser:    t.AcceptRegisterContactWithoutUser,
		RPReportOverPortUS:                  t.RPReportOverPortUS,
		RefreshOffersNewSecurityClient:      t.RefreshOffersNewSecurityClient,
		EnableInitialRejectFallback:         t.EnableInitialRejectFallback,
		DisableMWISubscription:              t.DisableMWISubscription,
		FallbackIncludesServerParamsInSecCl: t.FallbackIncludesServerParamsInSecCl,
		RegisterPolicy:                      registerPolicyFromInternal(t.RegisterPolicy),
		ExpiresSeconds:                      t.ExpiresSeconds, Transport: t.Transport, ContactOrder: cloneStrings(t.ContactOrder),
	}
}

func toInternalBase(cfg carrier.EffectiveCarrierConfig) policy.EffectiveCarrierConfig {
	return policy.EffectiveCarrierConfig{
		MCC: cfg.MCC, MNC: cfg.MNC, PresetID: cfg.PresetID, MatchedTemplate: cfg.MatchedTemplate,
		E911: policy.E911Policy{
			Enabled: cfg.E911.Enabled, Provider: cfg.E911.Provider,
			EntitlementURL: cfg.E911.EntitlementURL, WebsheetHostPolicy: cfg.E911.WebsheetHostPolicy,
			Websheet: cfg.E911.Websheet, EntitlementEndpoint: cfg.E911.EntitlementEndpoint,
		},
		IPStackType: cfg.IPStackType, EPDGAddr: cfg.EPDGAddr, EPDGAddrSource: cfg.EPDGAddrSource,
		EmergencyEPDGAddr: cfg.EmergencyEPDGAddr, EPDGPort: cfg.EPDGPort, APN: cfg.APN, DNSServer: cfg.DNSServer,
		XCAPAPN: cfg.XCAPAPN,
	}
}

func toInternalIKE(result *policy.EffectiveCarrierConfig, cfg carrier.EffectiveCarrierConfig) {
	result.NATKeepaliveSeconds = cfg.NATKeepaliveSeconds
	result.DPDIntervalSeconds = cfg.DPDIntervalSeconds
	result.AKAChallengeMode = cfg.AKAChallengeMode
	result.IKEIdentityMode = cfg.IKEIdentityMode
	result.AKAIdentityMode = cfg.AKAIdentityMode
	result.IKEProposals = cloneStrings(cfg.IKEProposals)
	result.ESPProposals = cloneStrings(cfg.ESPProposals)
	result.EnableLegacyCiphers = cfg.EnableLegacyCiphers
	result.AllowedLegacyCiphers = cloneStrings(cfg.AllowedLegacyCiphers)
	result.AlgorithmPolicy = cfg.AlgorithmPolicy
	result.DeviceIdentityIMEI = cfg.DeviceIdentityIMEI
	result.DeviceIdentityEnabled = cfg.DeviceIdentityEnabled
	result.DeviceModel = cfg.DeviceModel
	result.DPDKeepaliveIntervalSeconds = cfg.DPDKeepaliveIntervalSeconds
	result.ReauthIntervalSeconds = cfg.ReauthIntervalSeconds
	result.IKERekeyIntervalSeconds = cfg.IKERekeyIntervalSeconds
	result.WithholdDeviceIdentity = cfg.WithholdDeviceIdentity
	result.KeepChildSAOnRekeyDecline = cfg.KeepChildSAOnRekeyDecline
	result.DeferPeerChildRekeySwitch = cfg.DeferPeerChildRekeySwitch
}

func toInternalIMS(result *policy.EffectiveCarrierConfig, cfg carrier.EffectiveCarrierConfig) {
	result.IMSDomain = cfg.IMSDomain
	result.IMSRealm = cfg.IMSRealm
	result.IMSRegistrar = cfg.IMSRegistrar
	result.IMSPCSCF = cfg.IMSPCSCF
	result.IMSUserAgent = cfg.IMSUserAgent
	result.IMSTransport = cfg.IMSTransport
	result.IMSIdentitySource = cfg.IMSIdentitySource
	result.IMSLocalPort = cfg.IMSLocalPort
	result.IMSTCPKeepaliveSeconds = cfg.IMSTCPKeepaliveSeconds
	result.IMSOptionsPingIntervalSeconds = cfg.IMSOptionsPingIntervalSeconds
	result.IMSRegisterTemplate = TemplateToInternal(cfg.IMSRegisterTemplate)
	result.IMSRegisterPolicySource = cfg.IMSRegisterPolicySource
	result.SMSRoutingMethod = cfg.SMSRoutingMethod
	result.SMSRoutingGW = cfg.SMSRoutingGW
	result.ForceSMSCAuth = cfg.ForceSMSCAuth
	result.IMS = TemplateToInternal(cfg.IMS)
}

func fromInternalBase(cfg policy.EffectiveCarrierConfig) carrier.EffectiveCarrierConfig {
	return carrier.EffectiveCarrierConfig{
		MCC: cfg.MCC, MNC: cfg.MNC, PresetID: cfg.PresetID, MatchedTemplate: cfg.MatchedTemplate,
		E911: carrier.E911Policy{
			Enabled: cfg.E911.Enabled, Provider: cfg.E911.Provider,
			EntitlementURL: cfg.E911.EntitlementURL, WebsheetHostPolicy: cfg.E911.WebsheetHostPolicy,
			Websheet: cfg.E911.Websheet, EntitlementEndpoint: cfg.E911.EntitlementEndpoint,
		},
		IPStackType: cfg.IPStackType, EPDGAddr: cfg.EPDGAddr, EPDGAddrSource: cfg.EPDGAddrSource,
		EmergencyEPDGAddr: cfg.EmergencyEPDGAddr, EPDGPort: cfg.EPDGPort, APN: cfg.APN, DNSServer: cfg.DNSServer,
		XCAPAPN: cfg.XCAPAPN,
	}
}

func fromInternalIKE(result *carrier.EffectiveCarrierConfig, cfg policy.EffectiveCarrierConfig) {
	result.NATKeepaliveSeconds = cfg.NATKeepaliveSeconds
	result.DPDIntervalSeconds = cfg.DPDIntervalSeconds
	result.AKAChallengeMode = cfg.AKAChallengeMode
	result.IKEIdentityMode = cfg.IKEIdentityMode
	result.AKAIdentityMode = cfg.AKAIdentityMode
	result.IKEProposals = cloneStrings(cfg.IKEProposals)
	result.ESPProposals = cloneStrings(cfg.ESPProposals)
	result.EnableLegacyCiphers = cfg.EnableLegacyCiphers
	result.AllowedLegacyCiphers = cloneStrings(cfg.AllowedLegacyCiphers)
	result.AlgorithmPolicy = cfg.AlgorithmPolicy
	result.DeviceIdentityIMEI = cfg.DeviceIdentityIMEI
	result.DeviceIdentityEnabled = cfg.DeviceIdentityEnabled
	result.DeviceModel = cfg.DeviceModel
	result.DPDKeepaliveIntervalSeconds = cfg.DPDKeepaliveIntervalSeconds
	result.ReauthIntervalSeconds = cfg.ReauthIntervalSeconds
	result.IKERekeyIntervalSeconds = cfg.IKERekeyIntervalSeconds
	result.WithholdDeviceIdentity = cfg.WithholdDeviceIdentity
	result.KeepChildSAOnRekeyDecline = cfg.KeepChildSAOnRekeyDecline
	result.DeferPeerChildRekeySwitch = cfg.DeferPeerChildRekeySwitch
}

func fromInternalIMS(result *carrier.EffectiveCarrierConfig, cfg policy.EffectiveCarrierConfig) {
	result.IMSDomain = cfg.IMSDomain
	result.IMSRealm = cfg.IMSRealm
	result.IMSRegistrar = cfg.IMSRegistrar
	result.IMSPCSCF = cfg.IMSPCSCF
	result.IMSUserAgent = cfg.IMSUserAgent
	result.IMSTransport = cfg.IMSTransport
	result.IMSIdentitySource = cfg.IMSIdentitySource
	result.IMSLocalPort = cfg.IMSLocalPort
	result.IMSTCPKeepaliveSeconds = cfg.IMSTCPKeepaliveSeconds
	result.IMSOptionsPingIntervalSeconds = cfg.IMSOptionsPingIntervalSeconds
	result.IMSRegisterTemplate = TemplateFromInternal(cfg.IMSRegisterTemplate)
	result.IMSRegisterPolicySource = cfg.IMSRegisterPolicySource
	result.SMSRoutingMethod = cfg.SMSRoutingMethod
	result.SMSRoutingGW = cfg.SMSRoutingGW
	result.ForceSMSCAuth = cfg.ForceSMSCAuth
	result.IMS = TemplateFromInternal(cfg.IMS)
}

func registerPolicyToInternal(value carrier.IMSRegisterPolicy) policy.IMSRegisterPolicy {
	return policy.IMSRegisterPolicy{
		ID: value.ID, TemporaryStatusCodes: cloneInts(value.TemporaryStatusCodes),
		ForbiddenStatusCodes:             cloneInts(value.ForbiddenStatusCodes),
		InitialRejectFallbackStatusCodes: cloneInts(value.InitialRejectFallbackStatusCodes),
		TemporaryRetrySeconds:            value.TemporaryRetrySeconds,
	}
}

func registerPolicyFromInternal(value policy.IMSRegisterPolicy) carrier.IMSRegisterPolicy {
	return carrier.IMSRegisterPolicy{
		ID: value.ID, TemporaryStatusCodes: cloneInts(value.TemporaryStatusCodes),
		ForbiddenStatusCodes:             cloneInts(value.ForbiddenStatusCodes),
		InitialRejectFallbackStatusCodes: cloneInts(value.InitialRejectFallbackStatusCodes),
		TemporaryRetrySeconds:            value.TemporaryRetrySeconds,
	}
}

func mechanismsToInternal(values []carrier.IPSec3GPPSecurityMechanism) []policy.IPSec3GPPSecurityMechanism {
	result := make([]policy.IPSec3GPPSecurityMechanism, len(values))
	for index, value := range values {
		result[index] = policy.IPSec3GPPSecurityMechanism(value)
	}
	return result
}

func mechanismsFromInternal(values []policy.IPSec3GPPSecurityMechanism) []carrier.IPSec3GPPSecurityMechanism {
	result := make([]carrier.IPSec3GPPSecurityMechanism, len(values))
	for index, value := range values {
		result[index] = carrier.IPSec3GPPSecurityMechanism(value)
	}
	return result
}

func cloneStrings(values []string) []string { return append([]string(nil), values...) }

func cloneInts(values []int) []int { return append([]int(nil), values...) }
