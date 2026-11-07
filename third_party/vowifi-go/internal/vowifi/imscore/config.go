package imscore

import (
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
)

// IMSConfig keeps the original v1.5.5 field prefix intact. Fields after
// ForceSMSCAuth are additive runtime compatibility fields.
type IMSConfig struct {
	Enabled                    bool                       `mapstructure:"enabled"`
	DeviceID                   string                     `mapstructure:"device_id"`
	PCSCF                      string                     `mapstructure:"pcscf"`
	Registrar                  string                     `mapstructure:"registrar"`
	Domain                     string                     `mapstructure:"domain"`
	Realm                      string                     `mapstructure:"realm"`
	IMPI                       string                     `mapstructure:"impi"`
	IMPU                       string                     `mapstructure:"impu"`
	CarrierPresetID            string                     `mapstructure:"-"`
	IMSRegisterTemplate        policy.IMSRegisterTemplate `mapstructure:"-"`
	IMSRegisterPolicySource    string                     `mapstructure:"-"`
	LocalAddr                  string                     `mapstructure:"local_addr"`
	LocalPort                  int                        `mapstructure:"local_port"`
	Transport                  string                     `mapstructure:"transport"`
	UserAgent                  string                     `mapstructure:"user_agent"`
	PAccessNetworkInfo         string                     `mapstructure:"p_access_network_info"`
	CellularNetworkInfo        string                     `mapstructure:"cellular_network_info"`
	SIPInstance                string                     `mapstructure:"sip_instance"`
	IcsiRef                    string                     `mapstructure:"icsi_ref"`
	TCPKeepaliveSeconds        int                        `mapstructure:"tcp_keepalive_seconds"`
	OptionsPingIntervalSeconds int                        `mapstructure:"options_ping_interval_seconds"`
	IMScore                    IMScoreConfig              `mapstructure:"imscore"`
	EnableIPSec3GPP            *bool                      `mapstructure:"-"`
	SMSRoutingMethod           string                     `mapstructure:"-"`
	SMSRoutingGW               string                     `mapstructure:"-"`
	ForceSMSCAuth              bool                       `mapstructure:"-"`

	IMEI                       string
	ICCID                      string
	IMSI                       string
	IMPUs                      []string
	SMSC                       string
	EPDGAddr                   string
	LocalIP                    net.IP
	Expires                    time.Duration
	KeepaliveInterval          time.Duration
	KeepaliveTimeout           time.Duration
	AKAProvider                AKAProvider
	IMSNetwork                 IMSNetwork
	DeliveryStore              DeliveryStore
	EventBus                   *EventBus
	TraceID                    string
	PAccessNetworkCountry      string
	RegisterTemplate           IMSRegisterTemplate
	RegistrarPenalties         *RegistrarPenaltyStore
	SubscriptionRegistrations  *SubscriptionRegistrationStore
	MWIPolicyStore             MWISubscriptionPolicyStore
	AllowEmergencyRegistration bool
	OnLocalAddressChange       func(oldIP, newIP net.IP) error
}

type IMScoreConfig struct {
	Enabled           bool   `mapstructure:"enabled"`
	TargetURI         string `mapstructure:"target_uri"`
	ReceiverTransport string `mapstructure:"receiver_transport"`
}

func (cfg IMSConfig) IPSec3GPPEnabled() bool {
	return cfg.EnableIPSec3GPP == nil || *cfg.EnableIPSec3GPP
}

func (cfg *IMSConfig) SetEnableIPSec3GPP(enabled bool) {
	if cfg == nil {
		return
	}
	value := enabled
	cfg.EnableIPSec3GPP = &value
}

func (cfg IMSConfig) SMSReceiverTransport() string {
	return policy.NormalizeSMSReceiverTransport(cfg.IMScore.ReceiverTransport)
}

func (cfg *IMSConfig) publicIdentities() []string {
	if cfg == nil {
		return nil
	}
	values := make([]string, 0, len(cfg.IMPUs)+1)
	seen := make(map[string]struct{}, len(cfg.IMPUs)+1)
	for _, value := range append([]string{cfg.IMPU}, cfg.IMPUs...) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, value)
	}
	return values
}

func (cfg *IMSConfig) syncCompatibilityFields() {
	if cfg == nil {
		return
	}
	if cfg.IMPU == "" {
		cfg.IMPU = firstNonBlank(cfg.IMPUs...)
	}
	if cfg.SIPInstance == "" {
		cfg.SIPInstance = strings.TrimSpace(cfg.IMEI)
	}
	if cfg.IMEI == "" {
		cfg.IMEI = strings.TrimSpace(cfg.SIPInstance)
	}
	if cfg.LocalIP == nil && strings.TrimSpace(cfg.LocalAddr) != "" {
		cfg.LocalIP = net.ParseIP(strings.TrimSpace(cfg.LocalAddr))
	}
	if cfg.LocalAddr == "" && cfg.LocalIP != nil {
		cfg.LocalAddr = cfg.LocalIP.String()
	}
	mergeOriginalRegisterTemplate(cfg)
	if cfg.KeepaliveInterval <= 0 && cfg.OptionsPingIntervalSeconds > 0 {
		cfg.KeepaliveInterval = time.Duration(cfg.OptionsPingIntervalSeconds) * time.Second
	}
}

func mergeOriginalRegisterTemplate(cfg *IMSConfig) {
	if reflect.DeepEqual(cfg.IMSRegisterTemplate, policy.IMSRegisterTemplate{}) {
		return
	}
	template := policy.NormalizeIMSRegisterTemplate(cfg.IMSRegisterTemplate)
	current := &cfg.RegisterTemplate
	if current.Expires <= 0 && template.Expires > 0 {
		current.Expires = time.Duration(template.Expires) * time.Second
	}
	if current.SupportedHeader == "" {
		current.SupportedHeader = template.SupportedHeader
	}
	if current.AllowHeader == "" {
		current.AllowHeader = template.AllowHeader
	}
	if current.ContactMode == "" {
		current.ContactMode = template.ContactMode
	}
	if current.AccessType == "" {
		current.AccessType = template.AccessType
	}
	if current.ICSIRef == "" {
		current.ICSIRef = template.ICSIRef
	}
	if len(current.ContactOrder) == 0 {
		current.ContactOrder = append([]string(nil), template.ContactParamOrder...)
	}
	current.IncludePANIAuthenticated = current.IncludePANIAuthenticated || template.IncludePANIAuthenticated
	current.StrictSecurityServerOffer = current.StrictSecurityServerOffer || template.StrictSecurityServerOffer
	current.DisableMWISubscription = current.DisableMWISubscription || template.DisableMWISubscription
}
