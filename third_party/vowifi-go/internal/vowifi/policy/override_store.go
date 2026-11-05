package policy

import (
	"fmt"
	"sync"
)

var carrierOverrides = struct {
	sync.RWMutex
	values map[string]CarrierOverride
}{values: make(map[string]CarrierOverride)}

func SetCarrierOverrides(input any) error {
	values, err := carrierOverrideMap(input)
	if err != nil {
		return err
	}
	normalized := make(map[string]CarrierOverride, len(values))
	for rawKey, override := range values {
		mcc, mnc, ok := parsePLMNKey(rawKey)
		if !ok {
			return fmt.Errorf("invalid carrier override PLMN key %q", rawKey)
		}
		normalized[plmnKey(mcc, mnc)] = cloneCarrierOverride(NormalizeCarrierOverride(override))
	}
	carrierOverrides.Lock()
	carrierOverrides.values = normalized
	carrierOverrides.Unlock()
	return nil
}

func carrierOverrideMap(input any) (map[string]CarrierOverride, error) {
	switch values := input.(type) {
	case map[string]CarrierOverride:
		return values, nil
	case []CarrierOverride:
		result := make(map[string]CarrierOverride, len(values))
		for _, value := range values {
			if value.MCC == "" || value.MNC == "" {
				return nil, fmt.Errorf("carrier override slice entry has empty MCC or MNC")
			}
			result[plmnKey(value.MCC, value.MNC)] = value
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported carrier override collection %T", input)
	}
}

func ClearCarrierOverrides() {
	carrierOverrides.Lock()
	carrierOverrides.values = make(map[string]CarrierOverride)
	carrierOverrides.Unlock()
}

func carrierOverrideByKey(key string) (CarrierOverride, bool) {
	carrierOverrides.RLock()
	value, ok := carrierOverrides.values[key]
	carrierOverrides.RUnlock()
	return cloneCarrierOverride(value), ok
}

func hasExternalCarrierOverrideKey(key string) bool {
	_, ok := carrierOverrideByKey(key)
	return ok
}

func hasExternalCarrierOverrideRegisterPolicyKey(key string) bool {
	value, ok := carrierOverrideByKey(key)
	return ok && hasExplicitIMSRegisterPolicyOverride(value.IMSRegisterTemplate.RegisterPolicy)
}

func cloneCarrierOverride(value CarrierOverride) CarrierOverride {
	cloneCarrierOverridePointers(&value)
	value.AllowedLegacyCiphers = cloneStrings(value.AllowedLegacyCiphers)
	value.IKEProposals = cloneStrings(value.IKEProposals)
	value.ESPProposals = cloneStrings(value.ESPProposals)
	value.IMSRegisterTemplate.ContactParamOrder = cloneStrings(value.IMSRegisterTemplate.ContactParamOrder)
	value.IMSRegisterTemplate.SecurityClientMechanisms = cloneMechanisms(value.IMSRegisterTemplate.SecurityClientMechanisms)
	clonePolicyOverrideSlices(&value.IMSRegisterTemplate.RegisterPolicy)
	return value
}

func cloneCarrierOverridePointers(value *CarrierOverride) {
	value.E911.Enabled = cloneBoolPointer(value.E911.Enabled)
	value.EPDGPort = cloneIntPointer(value.EPDGPort)
	value.DeviceIdentityEnabled = cloneBoolPointer(value.DeviceIdentityEnabled)
	value.WithholdDeviceIdentity = cloneBoolPointer(value.WithholdDeviceIdentity)
	value.KeepChildSAOnRekeyDecline = cloneBoolPointer(value.KeepChildSAOnRekeyDecline)
	value.DeferPeerChildRekeySwitch = cloneBoolPointer(value.DeferPeerChildRekeySwitch)
	value.NATKeepaliveSeconds = cloneIntPointer(value.NATKeepaliveSeconds)
	value.DPDIntervalSeconds = cloneIntPointer(value.DPDIntervalSeconds)
	value.EnableLegacyCiphers = cloneBoolPointer(value.EnableLegacyCiphers)
	value.IMSLocalPort = cloneIntPointer(value.IMSLocalPort)
	value.IMSTCPKeepaliveSeconds = cloneIntPointer(value.IMSTCPKeepaliveSeconds)
	value.IMSOptionsPingIntervalSeconds = cloneIntPointer(value.IMSOptionsPingIntervalSeconds)
	value.ForceSMSCAuth = cloneBoolPointer(value.ForceSMSCAuth)
	value.AllowHandoverPDNWLANAndEPS = cloneBoolPointer(value.AllowHandoverPDNWLANAndEPS)
	template := &value.IMSRegisterTemplate
	template.UsePlainDigestPlaceholder = cloneBoolPointer(template.UsePlainDigestPlaceholder)
	template.Expires = cloneIntPointer(template.Expires)
	template.ForceHeaderPort5060 = cloneBoolPointer(template.ForceHeaderPort5060)
	template.IncludePANIAuthenticated = cloneBoolPointer(template.IncludePANIAuthenticated)
	template.IncludeConnectionKeepaliveInAuth = cloneBoolPointer(template.IncludeConnectionKeepaliveInAuth)
	template.SecurityClientIncludesServerParams = cloneBoolPointer(template.SecurityClientIncludesServerParams)
	template.StrictSecurityServerOffer = cloneBoolPointer(template.StrictSecurityServerOffer)
	template.AcceptRegisterContactWithoutUser = cloneBoolPointer(template.AcceptRegisterContactWithoutUser)
	template.RPReportOverPortUS = cloneBoolPointer(template.RPReportOverPortUS)
	template.RefreshOffersNewSecurityClient = cloneBoolPointer(template.RefreshOffersNewSecurityClient)
	template.EnableInitialRejectFallback = cloneBoolPointer(template.EnableInitialRejectFallback)
	template.DisableMWISubscription = cloneBoolPointer(template.DisableMWISubscription)
	template.FallbackIncludesServerParamsInSecCl = cloneBoolPointer(template.FallbackIncludesServerParamsInSecCl)
	template.RegisterPolicy.TemporaryRetrySeconds = cloneIntPointer(template.RegisterPolicy.TemporaryRetrySeconds)
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func clonePolicyOverrideSlices(value *IMSRegisterPolicyOverride) {
	if value.TemporaryStatusCodes != nil {
		cloned := cloneInts(*value.TemporaryStatusCodes)
		value.TemporaryStatusCodes = &cloned
	}
	if value.ForbiddenStatusCodes != nil {
		cloned := cloneInts(*value.ForbiddenStatusCodes)
		value.ForbiddenStatusCodes = &cloned
	}
	if value.InitialRejectFallbackStatusCodes != nil {
		cloned := cloneInts(*value.InitialRejectFallbackStatusCodes)
		value.InitialRejectFallbackStatusCodes = &cloned
	}
}
