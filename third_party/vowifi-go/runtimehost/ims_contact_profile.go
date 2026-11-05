package runtimehost

import (
	"fmt"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/profile"
	"github.com/iniwex5/vowifi-go/runtimehost/carrier"
	"github.com/iniwex5/vowifi-go/runtimehost/identity"
)

func imsRegisterConfigForPrepared(prepared *identity.PreparedSession) (imscore.IMSRegisterTemplate, string, error) {
	if prepared == nil {
		return imscore.IMSRegisterTemplate{}, "", fmt.Errorf("runtimehost: nil prepared session")
	}
	carrierConfig := prepared.ResolvedCarrierConfig()
	if carrierConfig.MCC == "" || carrierConfig.MNC == "" {
		carrierConfig = carrier.ResolveEffectiveCarrierConfig(prepared.Profile.MCC, prepared.Profile.MNC)
	}
	if err := carrier.ValidateEffectiveCarrierConfig(carrierConfig); err != nil {
		return imscore.IMSRegisterTemplate{}, "", fmt.Errorf("runtimehost: invalid carrier IMS template: %w", err)
	}
	template := carrierConfig.IMS
	registerTemplate := imscore.IMSRegisterTemplate{
		Expires:                   time.Duration(template.ExpiresSeconds) * time.Second,
		Transport:                 strings.ToLower(strings.TrimSpace(template.Transport)),
		SupportedHeader:           strings.TrimSpace(template.SupportedHeader),
		AllowHeader:               strings.TrimSpace(template.AllowHeader),
		ContactMode:               strings.TrimSpace(template.ContactMode),
		AccessType:                strings.TrimSpace(template.AccessType),
		ICSIRef:                   strings.TrimSpace(template.ICSIRef),
		ContactOrder:              append([]string(nil), template.ContactOrder...),
		IncludePANIAuthenticated:  template.IncludePANIAuthenticated,
		StrictSecurityServerOffer: template.StrictSecurityServerOffer,
		DisableMWISubscription:    template.DisableMWISubscription,
	}
	return registerTemplate, profile.ResolveUserAgentForModel(defaultIMSDeviceModel), nil
}

const defaultIMSDeviceModel = "iphone15,4"
