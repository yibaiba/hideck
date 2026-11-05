package runtimecore

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
	"github.com/iniwex5/vowifi-go/internal/vowifi/netstack"
	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
)

const defaultIMSSIPPort = 5060

type imsConfigInput struct {
	session  SessionConfig
	result   *SessionResult
	aka      imscore.AKAProvider
	network  imscore.IMSNetwork
	eventBus *imscore.EventBus
}

func resolveIPSec3GPPInstaller(
	dataplaneMode string,
	userspaceNetwork *netstack.Network,
) imscore.IPSec3GPPInstaller {
	if userspaceNetwork != nil {
		return userspaceNetwork
	}
	if strings.ToLower(strings.TrimSpace(dataplaneMode)) == swu.DataplaneModeUserspace {
		return nil
	}
	return imscore.SystemIPSec3GPPInstaller{}
}

func buildIMSConfig(input imsConfigInput) (*imscore.IMSConfig, error) {
	prepared := input.session.Prepared
	imsPlan := prepared.CarrierPlan.IMS
	identity := prepared.IMSIdentity
	localIP := net.ParseIP(input.result.LocalAddr)
	value := imscore.BuildIMSConfigFromCarrier(
		input.session.DeviceID, prepared.Profile.IMSI, prepared.Profile.IMEI,
		prepared.Profile.MCC, prepared.Profile.MNC, prepared.Profile.IMSDomain,
		prepared.Profile.UserAgent, input.result.LocalAddr, prepared.CarrierPlan,
	)
	value.ICCID = strings.TrimSpace(prepared.Profile.ICCID)
	if err := imscore.ApplyResolvedIMSIdentityToConfig(&value, identity, prepared.Profile.MCC); err != nil {
		return nil, fmt.Errorf("runtimecore: apply IMS identity: %w", err)
	}
	value.SMSC = prepared.Profile.SMSC
	value.EPDGAddr = prepared.EPDGAddr
	value.LocalIP = localIP
	value.Registrar = firstNonEmpty(
		assignedPCSCF(input.result.Snapshot, localIP), imsPlan.PCSCF, imsPlan.Registrar, value.Domain,
	)
	logging.Info("IMS P-CSCF configuration from new tunnel",
		"device", input.session.DeviceID, "inner_ip", input.result.LocalAddr,
		"assigned_pcscf_v4", input.result.Snapshot.PCSCFv4,
		"assigned_pcscf_v6", input.result.Snapshot.PCSCFv6, "registrar", value.Registrar)
	value.KeepaliveInterval = time.Duration(imsPlan.OptionsPingIntervalSeconds) * time.Second
	value.AKAProvider, value.IMSNetwork = input.aka, input.network
	value.DeliveryStore = adaptDeliveryStore(input.session.DeliveryStore)
	value.EventBus, value.TraceID = input.eventBus, input.session.TraceID
	value.PAccessNetworkCountry = imscore.CountryISO2FromMCC(prepared.Profile.MCC)
	value.RegisterTemplate = convertRegisterTemplate(imsPlan.RegisterTemplate, imsPlan.Transport)
	value.RegistrarPenalties = input.session.RegistrarPenalties
	value.SubscriptionRegistrations = input.session.SubscriptionRegistrations
	value.MWIPolicyStore = input.session.MWIPolicyStore
	if input.result != nil && input.result.Session != nil {
		session := input.result.Session
		value.OnLocalAddressChange = func(oldIP, newIP net.IP) error {
			return session.UpdateAddresses(oldIP, newIP)
		}
	}
	return &value, nil
}

func assignedPCSCF(snapshot swu.SessionSnapshot, localIP net.IP) string {
	servers := snapshot.PCSCFv6
	if localIP != nil && localIP.To4() != nil {
		servers = snapshot.PCSCFv4
	}
	candidates := make([]string, 0, len(servers))
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if server == nil {
			continue
		}
		candidate := net.JoinHostPort(server.String(), fmt.Sprint(defaultIMSSIPPort))
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}
	return strings.Join(candidates, ";")
}

func convertRegisterTemplate(
	template policy.IMSRegisterTemplate,
	transport string,
) imscore.IMSRegisterTemplate {
	return imscore.IMSRegisterTemplate{
		Expires:         time.Duration(template.Expires) * time.Second,
		Transport:       firstNonEmpty(template.Transport, transport),
		SupportedHeader: template.SupportedHeader, AllowHeader: template.AllowHeader,
		ContactMode: template.ContactMode, AccessType: template.AccessType, ICSIRef: template.ICSIRef,
		ContactOrder:              append([]string(nil), template.ContactOrder...),
		IncludePANIAuthenticated:  template.IncludePANIAuthenticated,
		StrictSecurityServerOffer: template.StrictSecurityServerOffer,
		DisableMWISubscription:    template.DisableMWISubscription,
	}
}
