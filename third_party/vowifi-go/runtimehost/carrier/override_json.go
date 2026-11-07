package carrier

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/iniwex5/vowifi-go/internal/vowifi/common"
	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
)

const defaultJSONOverridesPath = "carrier_overrides.json"

// LoadCarrierOverridesResult retains the post-v1.5.5 result-struct API. JSON
// arrays remain supported when the selected path has a .json extension.
func LoadCarrierOverridesResult(path string) (LoadResult, error) {
	if strings.EqualFold(filepath.Ext(strings.TrimSpace(path)), ".json") {
		return LoadCarrierOverridesJSON(path)
	}
	resolved, count, missing, err := LoadCarrierOverrides(path)
	return LoadResult{Path: resolved, Count: count, Missing: missing}, err
}

// LoadCarrierOverridesCurrent is the explicit current-name alias.
func LoadCarrierOverridesCurrent(path string) (LoadResult, error) {
	return LoadCarrierOverridesJSON(path)
}

// LoadCarrierOverridesJSON loads the post-v1.5.5 JSON slice format atomically.
func LoadCarrierOverridesJSON(path string) (LoadResult, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultJSONOverridesPath
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return LoadResult{Path: path, Missing: true}, nil
	}
	if err != nil {
		return LoadResult{Path: path}, fmt.Errorf("open carrier overrides: %w", err)
	}
	defer file.Close()
	values, err := decodeJSONOverrides(file)
	if err != nil {
		return LoadResult{Path: path}, err
	}
	internal, err := validateAndConvertOverrides(values)
	if err != nil {
		return LoadResult{Path: path}, err
	}
	if err := policy.SetCarrierOverrides(internal); err != nil {
		return LoadResult{Path: path}, err
	}
	return LoadResult{Path: path, Count: len(values)}, nil
}

func decodeJSONOverrides(reader io.Reader) ([]CarrierOverride, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var values []CarrierOverride
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("parse carrier overrides: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("parse carrier overrides: multiple JSON values")
		}
		return nil, fmt.Errorf("parse carrier overrides: %w", err)
	}
	return values, nil
}

func validateAndConvertOverrides(values []CarrierOverride) ([]policy.CarrierOverride, error) {
	result := make([]policy.CarrierOverride, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if err := validateCarrierOverride(value); err != nil {
			return nil, fmt.Errorf("carrier override %d: %w", index, err)
		}
		key := common.Plmn3(value.MCC) + common.Plmn3(value.MNC)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("carrier override %d: duplicate PLMN %s", index, key)
		}
		seen[key] = struct{}{}
		result = append(result, carrierOverrideToInternal(value))
	}
	return result, nil
}

func validateCarrierOverride(value CarrierOverride) error {
	if err := validatePLMN(value.MCC, value.MNC); err != nil {
		return err
	}
	if value.ReauthIntervalSeconds < 0 {
		return fmt.Errorf("reauth interval must not be negative")
	}
	if value.IKERekeyIntervalSeconds < 0 {
		return fmt.Errorf("IKE rekey interval must not be negative")
	}
	if err := validateE911Override(value.E911); err != nil {
		return err
	}
	base := CarrierConfigFromInternal(policy.GetGlobalDefaultConfig(value.MCC, value.MNC))
	if len(value.IKEProposals) > 0 {
		base.IKEProposals = cloneStrings(value.IKEProposals)
	}
	if len(value.ESPProposals) > 0 {
		base.ESPProposals = cloneStrings(value.ESPProposals)
	}
	if err := validateProposalConfig(base); err != nil {
		return err
	}
	if err := validateTemplateOverride(value.IMS); err != nil {
		return err
	}
	candidate := policy.ResolveEffectiveCarrierConfigWithOverride(
		value.MCC, value.MNC, carrierOverrideToInternal(value),
	)
	return ValidateEffectiveCarrierConfig(CarrierConfigFromInternal(candidate))
}

func validateE911Override(value E911Config) error {
	endpoints := []struct{ name, value string }{
		{"entitlement URL", value.EntitlementURL},
		{"websheet", value.Websheet},
		{"entitlement endpoint", value.EntitlementEndpoint},
	}
	for _, endpoint := range endpoints {
		if strings.TrimSpace(endpoint.value) != "" && !isHTTPURL(endpoint.value) {
			return fmt.Errorf("carrier: E911 %s must be an HTTP URL", endpoint.name)
		}
	}
	return nil
}

func validateTemplateOverride(value IMSRegisterTemplate) error {
	if err := validateRegisterTemplateOverrideFields(value); err != nil {
		return err
	}
	if value.expiresSet && value.ExpiresSeconds <= 0 {
		return fmt.Errorf("IMS registration expiry must be positive")
	}
	if value.Expires < 0 || value.ExpiresSeconds < 0 {
		return fmt.Errorf("IMS registration expiry must be positive")
	}
	if int64(value.Expires) > maxIMSExpiresSeconds || int64(value.ExpiresSeconds) > maxIMSExpiresSeconds {
		return fmt.Errorf("IMS registration expiry overflows duration")
	}
	if value.Transport != "" {
		if err := validateIMSTransport(value.Transport); err != nil {
			return err
		}
	}
	if value.ContactMode != "" && value.ContactMode != "legacy" && value.ContactMode != "android_default" {
		return fmt.Errorf("unsupported IMS Contact mode %q", value.ContactMode)
	}
	if len(value.ContactParamOrder) > 0 {
		if err := validateContactOrder(value.ContactParamOrder); err != nil {
			return err
		}
	}
	if len(value.ContactOrder) > 0 {
		return validateContactOrder(value.ContactOrder)
	}
	return nil
}

func carrierOverrideToInternal(value CarrierOverride) policy.CarrierOverride {
	result := policy.CarrierOverride{
		ID: strings.TrimSpace(value.PresetID), PresetID: strings.TrimSpace(value.PresetID),
		MCC: strings.TrimSpace(value.MCC), MNC: strings.TrimSpace(value.MNC),
		DeviceModel:               strings.TrimSpace(value.DeviceModel),
		IKEProposals:              cloneStrings(value.IKEProposals),
		ESPProposals:              cloneStrings(value.ESPProposals),
		ReauthIntervalSeconds:     value.ReauthIntervalSeconds,
		IKERekeyIntervalSeconds:   value.IKERekeyIntervalSeconds,
		WithholdDeviceIdentity:    value.WithholdDeviceIdentity,
		KeepChildSAOnRekeyDecline: value.KeepChildSAOnRekeyDecline,
		DeferPeerChildRekeySwitch: value.DeferPeerChildRekeySwitch,
		IMSTransport:              strings.TrimSpace(value.IMS.Transport),
		E911: policy.E911PolicyOverride{
			Provider: value.E911.Provider, EntitlementURL: value.E911.EntitlementURL,
			WebsheetHostPolicy:  value.E911.WebsheetHostPolicy,
			Websheet:            value.E911.Websheet,
			EntitlementEndpoint: value.E911.EntitlementEndpoint,
		},
		IMSRegisterTemplate: templateOverrideToInternal(value.IMS),
	}
	if value.E911.Enabled {
		result.E911.Enabled = boolPointer(true)
	}
	return result
}

func templateOverrideToInternal(value IMSRegisterTemplate) policy.IMSRegisterTemplateOverride {
	result := policy.IMSRegisterTemplateOverride{
		ID: value.ID, SMSReceiverTransport: value.SMSReceiverTransport,
		ContactMode: value.ContactMode, FixedPANI: value.FixedPANI,
		SupportedHeader: value.SupportedHeader, AllowHeader: value.AllowHeader,
		AccessType: value.AccessType, ICSIRef: value.ICSIRef,
		ContactParamOrder:        cloneStrings(firstStrings(value.ContactParamOrder, value.ContactOrder)),
		SecAgreeMode:             value.SecAgreeMode,
		SecurityClientMechanisms: mechanismsToInternal(value.SecurityClientMechanisms),
		RegisterPolicy:           registerPolicyOverrideToInternal(value.RegisterPolicy),
	}
	copyTemplateScalarOverrides(&result, value)
	return result
}

func copyTemplateScalarOverrides(result *policy.IMSRegisterTemplateOverride, value IMSRegisterTemplate) {
	result.UsePlainDigestPlaceholder = trueBoolPointer(value.UsePlainDigestPlaceholder)
	result.Expires = positiveIntPointer(firstInt(value.Expires, value.ExpiresSeconds))
	if value.expiresSet && value.ExpiresSeconds == 0 {
		result.Expires = intPointer(0)
	}
	result.ForceHeaderPort5060 = trueBoolPointer(value.ForceHeaderPort5060)
	result.IncludePANIAuthenticated = trueBoolPointer(value.IncludePANIAuthenticated)
	result.IncludeConnectionKeepaliveInAuth = trueBoolPointer(value.IncludeConnectionKeepaliveInAuth)
	result.SecurityClientIncludesServerParams = trueBoolPointer(value.SecurityClientIncludesServerParams)
	result.StrictSecurityServerOffer = trueBoolPointer(value.StrictSecurityServerOffer)
	result.AcceptRegisterContactWithoutUser = trueBoolPointer(value.AcceptRegisterContactWithoutUser)
	result.RPReportOverPortUS = trueBoolPointer(value.RPReportOverPortUS)
	result.RefreshOffersNewSecurityClient = trueBoolPointer(value.RefreshOffersNewSecurityClient)
	result.EnableInitialRejectFallback = trueBoolPointer(value.EnableInitialRejectFallback)
	result.DisableMWISubscription = trueBoolPointer(value.DisableMWISubscription)
	result.FallbackIncludesServerParamsInSecCl = trueBoolPointer(value.FallbackIncludesServerParamsInSecCl)
}

func registerPolicyOverrideToInternal(value IMSRegisterPolicy) policy.IMSRegisterPolicyOverride {
	result := policy.IMSRegisterPolicyOverride{ID: value.ID}
	result.TemporaryStatusCodes = nonEmptyIntsPointer(value.TemporaryStatusCodes)
	result.ForbiddenStatusCodes = nonEmptyIntsPointer(value.ForbiddenStatusCodes)
	result.InitialRejectFallbackStatusCodes = nonEmptyIntsPointer(value.InitialRejectFallbackStatusCodes)
	result.TemporaryRetrySeconds = positiveIntPointer(value.TemporaryRetrySeconds)
	return result
}

func mechanismsToInternal(values []IPSec3GPPSecurityMechanism) []policy.IPSec3GPPSecurityMechanism {
	result := make([]policy.IPSec3GPPSecurityMechanism, len(values))
	for index, value := range values {
		result[index] = policy.IPSec3GPPSecurityMechanism(value)
	}
	return result
}

func firstStrings(primary, compatibility []string) []string {
	if len(primary) > 0 {
		return primary
	}
	return compatibility
}

func firstInt(primary, compatibility int) int {
	if primary != 0 {
		return primary
	}
	return compatibility
}

func boolPointer(value bool) *bool { return &value }
func intPointer(value int) *int    { return &value }

func trueBoolPointer(value bool) *bool {
	if !value {
		return nil
	}
	return boolPointer(true)
}

func positiveIntPointer(value int) *int {
	if value <= 0 {
		return nil
	}
	return intPointer(value)
}

func nonEmptyIntsPointer(values []int) *[]int {
	if len(values) == 0 {
		return nil
	}
	copy := cloneInts(values)
	return &copy
}
