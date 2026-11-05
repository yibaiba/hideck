package policy

import "strings"

func NormalizeCarrierOverride(override CarrierOverride) CarrierOverride {
	override.ID = valueOr(strings.TrimSpace(override.ID), strings.TrimSpace(override.PresetID))
	override.PresetID = override.ID
	override.MCC = strings.TrimSpace(override.MCC)
	override.MNC = strings.TrimSpace(override.MNC)
	override.E911 = normalizeE911PolicyOverride(override.E911)
	override.CustomEPDG = strings.TrimSpace(override.CustomEPDG)
	override.APN = strings.TrimSpace(override.APN)
	override.IPStackType = strings.TrimSpace(override.IPStackType)
	override.DNSServer = strings.TrimSpace(override.DNSServer)
	override.AlgorithmPolicy = strings.TrimSpace(override.AlgorithmPolicy)
	override.DeviceModel = strings.TrimSpace(override.DeviceModel)
	override.AKAChallengeMode = strings.TrimSpace(override.AKAChallengeMode)
	override.IKEIdentityMode = strings.TrimSpace(override.IKEIdentityMode)
	override.AKAIdentityMode = strings.TrimSpace(override.AKAIdentityMode)
	override.DeviceIdentityIMEI = strings.TrimSpace(override.DeviceIdentityIMEI)
	override.AllowedLegacyCiphers = normalizeOptionalStringList(override.AllowedLegacyCiphers)
	override.IKEProposals = normalizeOptionalStringList(override.IKEProposals)
	override.ESPProposals = normalizeOptionalStringList(override.ESPProposals)
	override.IMSDomain = strings.TrimSpace(override.IMSDomain)
	override.IMSRealm = strings.TrimSpace(override.IMSRealm)
	override.IMSRegistrar = strings.TrimSpace(override.IMSRegistrar)
	override.IMSPCSCF = strings.TrimSpace(override.IMSPCSCF)
	override.IMSUserAgent = strings.TrimSpace(override.IMSUserAgent)
	override.IMSTransport = strings.TrimSpace(override.IMSTransport)
	override.IMSIdentitySource = strings.TrimSpace(override.IMSIdentitySource)
	override.SMSRoutingMethod = strings.TrimSpace(override.SMSRoutingMethod)
	override.SMSRoutingGW = strings.TrimSpace(override.SMSRoutingGW)
	override.XCAPAPN = strings.TrimSpace(override.XCAPAPN)
	override.MediaTypeRestrictionPolicy = strings.TrimSpace(override.MediaTypeRestrictionPolicy)
	override.ToConRef = strings.TrimSpace(override.ToConRef)
	override.PreferredAccessNetworks = normalizeOptionalStringList(override.PreferredAccessNetworks)
	override.IMSRegisterTemplate = normalizeIMSRegisterTemplateOverride(override.IMSRegisterTemplate)
	applyCompatibilityOverride(&override)
	return override
}

func applyCarrierOverride(preset CarrierPreset, override CarrierOverride) CarrierPreset {
	override = NormalizeCarrierOverride(override)
	setStringIfPresent(&preset.ID, override.ID)
	preset.E911 = applyE911PolicyOverride(preset.E911, override.E911)
	setStringIfPresent(&preset.CustomEPDG, override.CustomEPDG)
	setStringIfPresent(&preset.APN, override.APN)
	setStringIfPresent(&preset.IPStackType, override.IPStackType)
	setStringIfPresent(&preset.DNSServer, override.DNSServer)
	setStringIfPresent(&preset.AlgorithmPolicy, override.AlgorithmPolicy)
	setStringIfPresent(&preset.DeviceModel, override.DeviceModel)
	setStringIfPresent(&preset.AKAChallengeMode, override.AKAChallengeMode)
	setStringIfPresent(&preset.IKEIdentityMode, override.IKEIdentityMode)
	setStringIfPresent(&preset.AKAIdentityMode, override.AKAIdentityMode)
	setStringIfPresent(&preset.DeviceIdentityIMEI, override.DeviceIdentityIMEI)
	copyOverridePointers(&preset, override)
	if len(override.AllowedLegacyCiphers) > 0 {
		preset.AllowedLegacyCiphers = cloneStrings(override.AllowedLegacyCiphers)
	}
	if len(override.IKEProposals) > 0 {
		preset.IKEProposals = cloneStrings(override.IKEProposals)
	}
	if len(override.ESPProposals) > 0 {
		preset.ESPProposals = cloneStrings(override.ESPProposals)
	}
	setStringIfPresent(&preset.IMSDomain, override.IMSDomain)
	setStringIfPresent(&preset.IMSRealm, override.IMSRealm)
	setStringIfPresent(&preset.IMSRegistrar, override.IMSRegistrar)
	setStringIfPresent(&preset.IMSPCSCF, override.IMSPCSCF)
	setStringIfPresent(&preset.IMSUserAgent, override.IMSUserAgent)
	setStringIfPresent(&preset.IMSTransport, override.IMSTransport)
	setStringIfPresent(&preset.IMSIdentitySource, override.IMSIdentitySource)
	if override.DPDKeepaliveIntervalSeconds > 0 {
		preset.DPDKeepaliveIntervalSeconds = override.DPDKeepaliveIntervalSeconds
	}
	if override.ReauthIntervalSeconds > 0 {
		preset.ReauthIntervalSeconds = override.ReauthIntervalSeconds
	}
	if override.IKERekeyIntervalSeconds > 0 {
		preset.IKERekeyIntervalSeconds = override.IKERekeyIntervalSeconds
	}
	applyBoolPtr(&preset.KeepChildSAOnRekeyDecline, override.KeepChildSAOnRekeyDecline)
	if override.WithholdDeviceIdentity != nil {
		value := *override.WithholdDeviceIdentity
		preset.WithholdDeviceIdentity = &value
	}
	applyBoolPtr(&preset.DeferPeerChildRekeySwitch, override.DeferPeerChildRekeySwitch)
	preset.IMSRegisterTemplate = applyIMSRegisterTemplateOverride(preset.IMSRegisterTemplate, override.IMSRegisterTemplate)
	setStringIfPresent(&preset.SMSRoutingMethod, override.SMSRoutingMethod)
	setStringIfPresent(&preset.SMSRoutingGW, override.SMSRoutingGW)
	setStringIfPresent(&preset.XCAPAPN, override.XCAPAPN)
	setStringIfPresent(&preset.MediaTypeRestrictionPolicy, override.MediaTypeRestrictionPolicy)
	setStringIfPresent(&preset.ToConRef, override.ToConRef)
	if len(override.PreferredAccessNetworks) > 0 {
		preset.PreferredAccessNetworks = cloneStrings(override.PreferredAccessNetworks)
	}
	return preset
}

func copyOverridePointers(preset *CarrierPreset, override CarrierOverride) {
	if override.EPDGPort != nil {
		value := *override.EPDGPort
		preset.EPDGPort = &value
	}
	if override.DeviceIdentityEnabled != nil {
		value := *override.DeviceIdentityEnabled
		preset.DeviceIdentityEnabled = &value
	}
	if override.NATKeepaliveSeconds != nil {
		value := *override.NATKeepaliveSeconds
		preset.NATKeepaliveSeconds = &value
	}
	if override.DPDIntervalSeconds != nil {
		value := *override.DPDIntervalSeconds
		preset.DPDIntervalSeconds = &value
	}
	if override.EnableLegacyCiphers != nil {
		value := *override.EnableLegacyCiphers
		preset.EnableLegacyCiphers = &value
	}
	if override.IMSLocalPort != nil {
		value := *override.IMSLocalPort
		preset.IMSLocalPort = &value
	}
	if override.IMSTCPKeepaliveSeconds != nil {
		value := *override.IMSTCPKeepaliveSeconds
		preset.IMSTCPKeepaliveSeconds = &value
	}
	if override.IMSOptionsPingIntervalSeconds != nil {
		value := *override.IMSOptionsPingIntervalSeconds
		preset.IMSOptionsPingIntervalSeconds = &value
	}
	if override.ForceSMSCAuth != nil {
		value := *override.ForceSMSCAuth
		preset.ForceSMSCAuth = &value
	}
	if override.AllowHandoverPDNWLANAndEPS != nil {
		value := *override.AllowHandoverPDNWLANAndEPS
		preset.AllowHandoverPDNWLANAndEPS = &value
	}
}

func normalizeIMSRegisterTemplateOverride(value IMSRegisterTemplateOverride) IMSRegisterTemplateOverride {
	value.ID = strings.TrimSpace(value.ID)
	value.SMSReceiverTransport = strings.TrimSpace(value.SMSReceiverTransport)
	value.ContactMode = strings.TrimSpace(value.ContactMode)
	value.FixedPANI = strings.TrimSpace(value.FixedPANI)
	value.SupportedHeader = strings.TrimSpace(value.SupportedHeader)
	value.AllowHeader = strings.TrimSpace(value.AllowHeader)
	value.AccessType = strings.TrimSpace(value.AccessType)
	value.ICSIRef = strings.TrimSpace(value.ICSIRef)
	value.SecAgreeMode = strings.TrimSpace(value.SecAgreeMode)
	value.ContactParamOrder = normalizeOptionalStringList(value.ContactParamOrder)
	value.SecurityClientMechanisms = normalizeIPSec3GPPMechanismOverrideList(value.SecurityClientMechanisms)
	value.RegisterPolicy = normalizeIMSRegisterPolicyOverride(value.RegisterPolicy)
	return value
}

func normalizeIMSRegisterPolicyOverride(value IMSRegisterPolicyOverride) IMSRegisterPolicyOverride {
	value.ID = strings.TrimSpace(value.ID)
	normalizeStatusCodePointer(&value.TemporaryStatusCodes)
	normalizeStatusCodePointer(&value.ForbiddenStatusCodes)
	normalizeStatusCodePointer(&value.InitialRejectFallbackStatusCodes)
	return value
}

func applyIMSRegisterTemplateOverride(target IMSRegisterTemplate, override IMSRegisterTemplateOverride) IMSRegisterTemplate {
	target = NormalizeIMSRegisterTemplate(target)
	setStringIfPresent(&target.ID, override.ID)
	applyBoolPtr(&target.UsePlainDigestPlaceholder, override.UsePlainDigestPlaceholder)
	if override.Expires != nil && *override.Expires > 0 {
		target.Expires = *override.Expires
	}
	setStringIfPresent(&target.SMSReceiverTransport, override.SMSReceiverTransport)
	setStringIfPresent(&target.ContactMode, override.ContactMode)
	setStringIfPresent(&target.FixedPANI, override.FixedPANI)
	setStringIfPresent(&target.SupportedHeader, override.SupportedHeader)
	setStringIfPresent(&target.AllowHeader, override.AllowHeader)
	setStringIfPresent(&target.AccessType, override.AccessType)
	setStringIfPresent(&target.ICSIRef, override.ICSIRef)
	if len(override.ContactParamOrder) > 0 {
		target.ContactParamOrder = cloneStrings(override.ContactParamOrder)
	}
	applyBoolPtr(&target.ForceHeaderPort5060, override.ForceHeaderPort5060)
	applyBoolPtr(&target.IncludePANIAuthenticated, override.IncludePANIAuthenticated)
	applyBoolPtr(&target.IncludeConnectionKeepaliveInAuth, override.IncludeConnectionKeepaliveInAuth)
	setStringIfPresent(&target.SecAgreeMode, override.SecAgreeMode)
	applyBoolPtr(&target.SecurityClientIncludesServerParams, override.SecurityClientIncludesServerParams)
	if len(override.SecurityClientMechanisms) > 0 {
		target.SecurityClientMechanisms = cloneMechanisms(override.SecurityClientMechanisms)
	}
	applyBoolPtr(&target.StrictSecurityServerOffer, override.StrictSecurityServerOffer)
	applyBoolPtr(&target.AcceptRegisterContactWithoutUser, override.AcceptRegisterContactWithoutUser)
	applyBoolPtr(&target.RPReportOverPortUS, override.RPReportOverPortUS)
	applyBoolPtr(&target.RefreshOffersNewSecurityClient, override.RefreshOffersNewSecurityClient)
	applyBoolPtr(&target.EnableInitialRejectFallback, override.EnableInitialRejectFallback)
	applyBoolPtr(&target.DisableMWISubscription, override.DisableMWISubscription)
	applyBoolPtr(&target.FallbackIncludesServerParamsInSecCl, override.FallbackIncludesServerParamsInSecCl)
	target.RegisterPolicy = applyIMSRegisterPolicyOverride(target.RegisterPolicy, override.RegisterPolicy)
	return NormalizeIMSRegisterTemplate(target)
}

func applyIMSRegisterPolicyOverride(target IMSRegisterPolicy, override IMSRegisterPolicyOverride) IMSRegisterPolicy {
	setStringIfPresent(&target.ID, override.ID)
	if override.TemporaryStatusCodes != nil {
		target.TemporaryStatusCodes = cloneInts(*override.TemporaryStatusCodes)
	}
	if override.ForbiddenStatusCodes != nil {
		target.ForbiddenStatusCodes = cloneInts(*override.ForbiddenStatusCodes)
	}
	if override.InitialRejectFallbackStatusCodes != nil {
		target.InitialRejectFallbackStatusCodes = cloneInts(*override.InitialRejectFallbackStatusCodes)
	}
	applyIntPtr(&target.TemporaryRetrySeconds, override.TemporaryRetrySeconds)
	return NormalizeIMSRegisterPolicy(target)
}

func hasExplicitIMSRegisterPolicyOverride(value IMSRegisterPolicyOverride) bool {
	return strings.TrimSpace(value.ID) != "" || value.TemporaryStatusCodes != nil ||
		value.ForbiddenStatusCodes != nil || value.InitialRejectFallbackStatusCodes != nil ||
		value.TemporaryRetrySeconds != nil
}

func normalizeOptionalStringList(values []string) []string {
	if values == nil {
		return nil
	}
	return normalizeStringList(values)
}

func normalizeIPSec3GPPMechanismOverrideList(values []IPSec3GPPSecurityMechanism) []IPSec3GPPSecurityMechanism {
	if values == nil {
		return nil
	}
	return normalizeIPSec3GPPMechanismList(values)
}

func normalizeStatusCodePointer(values **[]int) {
	if *values == nil {
		return
	}
	normalized := normalizeSIPStatusCodeList(**values)
	*values = &normalized
}

func applyCompatibilityOverride(override *CarrierOverride) {
	if isZeroIMSRegisterTemplate(override.IMS) {
		return
	}
	compat := override.IMS
	setStringIfPresent(&override.IMSDomain, compat.Domain)
	setStringIfPresent(&override.CustomEPDG, compat.EPDGAddr)
	setStringIfPresent(&override.IMSTransport, compat.Transport)
	setStringIfPresent(&override.SMSRoutingMethod, compat.SMSRoutingMethod)
	setStringIfPresent(&override.IMSIdentitySource, compat.IdentitySource)
	setStringIfPresent(&override.DNSServer, compat.DNSServer)
	if compat.ID != "" {
		override.IMSRegisterTemplate.ID = compat.ID
	}
}
