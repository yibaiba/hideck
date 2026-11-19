package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/internal/apduarbiter"
	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/internal/e911"
	"github.com/yibaiba/hideck/internal/esim"
	"github.com/yibaiba/hideck/internal/modem"
	"github.com/yibaiba/hideck/internal/pcsc"
	proxytraffic "github.com/yibaiba/hideck/internal/proxy/traffic"
	"github.com/yibaiba/hideck/internal/volte"
	"github.com/yibaiba/hideck/pkg/logger"

	"github.com/gin-gonic/gin"
)

type deviceConfigDTO struct {
	ID                    string  `json:"id"`
	Name                  string  `json:"name"`
	ModemIMEI             string  `json:"modem_imei"`
	USBPath               string  `json:"usb_path"`
	ATPort                string  `json:"at_port"`
	ProxyPort             int     `json:"proxy_port"`
	Interface             string  `json:"interface"`
	ControlDevice         string  `json:"control_device,omitempty"`
	QMIUseProxy           *bool   `json:"qmi_use_proxy,omitempty"`
	QMIProxyPath          *string `json:"qmi_proxy_path,omitempty"`
	QMIProxyExecutable    *string `json:"qmi_proxy_executable,omitempty"`
	ESIMTransport         string  `json:"esim_transport,omitempty"`
	BaudRate              int     `json:"baud_rate,omitempty"`
	DataBits              int     `json:"data_bits,omitempty"`
	StopBits              int     `json:"stop_bits,omitempty"`
	Parity                string  `json:"parity,omitempty"`
	OperatorSelectionMode string  `json:"operator_selection_mode,omitempty"`
	OperatorSelectionPLMN string  `json:"operator_selection_plmn,omitempty"`
	OperatorSelectionRAT  string  `json:"operator_selection_rat,omitempty"`
	SMSEnabled            bool    `json:"sms_enabled"`
	APN                   string  `json:"apn,omitempty"`
	IPVersion             string  `json:"ip_version,omitempty"`
	NetworkEnabled        bool    `json:"network_enabled"`
	VoWiFiEnabled         bool    `json:"vowifi_enabled"`
	DeviceBackend         string  `json:"device_backend,omitempty"`
	PCSCReaderName        string  `json:"pcsc_reader_name,omitempty"`
	PCSCUSBPath           string  `json:"pcsc_usb_path,omitempty"`
	SIMPINEnv             string  `json:"sim_pin_env,omitempty"`
}

func deviceConfigToDTO(c config.DeviceConfig) deviceConfigDTO {
	return deviceConfigDTO{
		ID:                    c.ID,
		Name:                  c.Name,
		ModemIMEI:             c.ModemIMEI,
		USBPath:               c.USBPath,
		ATPort:                c.ATPort,
		ProxyPort:             c.ProxyPort,
		Interface:             c.Interface,
		ControlDevice:         c.ControlDevice,
		QMIUseProxy:           boolPtr(c.QMIUseProxy),
		QMIProxyPath:          stringPtr(c.QMIProxyPath),
		QMIProxyExecutable:    stringPtr(c.QMIProxyExecutable),
		ESIMTransport:         config.NormalizeESIMTransport(c.ESIMTransport),
		BaudRate:              c.BaudRate,
		DataBits:              c.DataBits,
		StopBits:              c.StopBits,
		Parity:                c.Parity,
		OperatorSelectionMode: c.OperatorSelectionMode,
		OperatorSelectionPLMN: c.OperatorSelectionPLMN,
		OperatorSelectionRAT:  c.OperatorSelectionRAT,
		SMSEnabled:            c.SMSEnabled,
		APN:                   c.APN,
		IPVersion:             c.IPVersion,
		NetworkEnabled:        c.NetworkEnabled,
		VoWiFiEnabled:         c.VoWiFiEnabled,
		DeviceBackend:         c.DeviceBackend,
		PCSCReaderName:        c.PCSCReaderName,
		PCSCUSBPath:           c.PCSCUSBPath,
		SIMPINEnv:             c.SIMPINEnv,
	}
}

func deviceConfigFromDTO(d deviceConfigDTO) config.DeviceConfig {
	return deviceConfigFromDTOWithBase(d, nil)
}

func deviceConfigFromDTOWithBase(d deviceConfigDTO, base *config.DeviceConfig) config.DeviceConfig {
	id := strings.TrimSpace(d.ID)
	if id == "" {
		id = strings.TrimSpace(d.Interface)
	}
	qmiUseProxy := false
	qmiProxyPath := ""
	qmiProxyExecutable := ""
	var usbNetMode *int
	if base != nil {
		qmiUseProxy = base.QMIUseProxy
		qmiProxyPath = base.QMIProxyPath
		qmiProxyExecutable = base.QMIProxyExecutable
		if base.USBNetMode != nil {
			mode := *base.USBNetMode
			usbNetMode = &mode
		}
	}
	if d.QMIUseProxy != nil {
		qmiUseProxy = *d.QMIUseProxy
	}
	if d.QMIProxyPath != nil {
		qmiProxyPath = strings.TrimSpace(*d.QMIProxyPath)
	}
	if d.QMIProxyExecutable != nil {
		qmiProxyExecutable = strings.TrimSpace(*d.QMIProxyExecutable)
	}
	return config.DeviceConfig{
		ID:                    id,
		Name:                  strings.TrimSpace(d.Name),
		ModemIMEI:             strings.TrimSpace(d.ModemIMEI),
		USBPath:               strings.TrimSpace(d.USBPath),
		ATPort:                strings.TrimSpace(d.ATPort),
		ProxyPort:             d.ProxyPort,
		Interface:             strings.TrimSpace(d.Interface),
		ControlDevice:         strings.TrimSpace(d.ControlDevice),
		QMIUseProxy:           qmiUseProxy,
		QMIProxyPath:          qmiProxyPath,
		QMIProxyExecutable:    qmiProxyExecutable,
		ESIMTransport:         config.NormalizeESIMTransport(d.ESIMTransport),
		BaudRate:              d.BaudRate,
		DataBits:              d.DataBits,
		StopBits:              d.StopBits,
		Parity:                strings.TrimSpace(d.Parity),
		OperatorSelectionMode: strings.TrimSpace(d.OperatorSelectionMode),
		OperatorSelectionPLMN: strings.TrimSpace(d.OperatorSelectionPLMN),
		OperatorSelectionRAT:  strings.TrimSpace(d.OperatorSelectionRAT),
		SMSEnabled:            true, // 短信功能始终启用
		APN:                   strings.TrimSpace(d.APN),
		IPVersion:             strings.TrimSpace(d.IPVersion),
		NetworkEnabled:        d.NetworkEnabled,
		VoWiFiEnabled:         d.VoWiFiEnabled,
		DeviceBackend:         d.DeviceBackend,
		PCSCReaderName:        strings.TrimSpace(d.PCSCReaderName),
		PCSCUSBPath:           strings.TrimSpace(d.PCSCUSBPath),
		SIMPINEnv:             strings.TrimSpace(d.SIMPINEnv),
		USBNetMode:            usbNetMode,
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func stringPtr(v string) *string {
	return &v
}

type deviceMgmtOverviewItem struct {
	ID                     string             `json:"id"`
	Name                   string             `json:"name"`
	Running                bool               `json:"running"`
	Healthy                bool               `json:"healthy"`
	ControlOnline          bool               `json:"control_online"`
	PhysicalPresent        bool               `json:"physical_present"`
	WorkerRunning          bool               `json:"worker_running"`
	DataConnected          bool               `json:"data_connected"`
	RadioRegistered        bool               `json:"radio_registered"`
	LifecyclePhase         string             `json:"lifecycle_phase"`
	LifecycleReason        string             `json:"lifecycle_reason,omitempty"`
	PrivateIP              string             `json:"private_ip,omitempty"`
	PrivateIPv6            string             `json:"private_ipv6,omitempty"`
	PublicIP               string             `json:"public_ip"`
	PublicIPv6             string             `json:"public_ipv6,omitempty"`
	Config                 *deviceConfigDTO   `json:"config,omitempty"`
	Modem                  modem.DeviceStatus `json:"modem"`
	Traffic                map[string]string  `json:"traffic,omitempty"`
	TrafficRaw             map[string]int64   `json:"traffic_raw,omitempty"`
	TrafficMeta            *deviceTrafficMeta `json:"traffic_meta,omitempty"`
	BackendMode            string             `json:"backend_mode,omitempty"`
	NetworkConnected       bool               `json:"network_connected"`
	RegistrationStateLabel string             `json:"registration_state_label"`
	// Interface / ControlDevice / ATPort / USBPath 是 worker 运行时解析出的当前路径
	// (零路径持久化后不入库),前端据此显示与判定 QMI 后端可用性、流量接口,
	// 不再依赖持久化 config 的路径字段。
	Interface     string `json:"interface,omitempty"`
	ControlDevice string `json:"control_device,omitempty"`
	ATPort        string `json:"at_port,omitempty"`
	USBPath       string `json:"usb_path,omitempty"`
}

func modemSummaryStatus(status modem.DeviceStatus) modem.DeviceStatus {
	status.GID1 = ""
	status.GID2 = ""
	status.PNN = nil
	status.OPL = nil
	status.SIMServiceTable = nil
	return status
}

func registrationStateLabel(regStatus int) string {
	switch regStatus {
	case 1, 5:
		return "registered"
	case 2:
		return "searching"
	case 3:
		return "denied"
	default:
		return "unknown"
	}
}

// overviewDisplayConfig 合并设备的展示配置:身份与用户意图取自持久化 config,
// 而 interface / control_device / at_port 等运行时路径取自 worker 内存 config。
// 零路径持久化后持久化侧已不含这些路径,必须用运行时真实值展示,否则在线设备会被
// 误判为“未探测到数据控制端”、流量按空接口名也统计不到。
// cardPolicyVoWiFiEnabled 从卡策略读取用户意图，ICCID 为空或查不到时降级用 fallback。
func overviewPhoneMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "cellular":
		return "cellular"
	case "volte":
		return "volte"
	case "modem_voice":
		return "modem_voice"
	default:
		return "wifi"
	}
}

func cardPolicyVoWiFiEnabled(iccid string, fallback bool) bool {
	if iccid == "" {
		return fallback
	}
	pol, err := db.GetCardPolicy(iccid)
	if err != nil {
		return fallback
	}
	return pol.VoWiFiEnabled
}

func overviewDisplayConfig(runtime, persisted config.DeviceConfig, hasPersisted bool) config.DeviceConfig {
	if !hasPersisted {
		return runtime
	}
	cfg := persisted
	cfg.Interface = runtime.Interface
	cfg.ControlDevice = runtime.ControlDevice
	cfg.ATPort = runtime.ATPort
	cfg.ManagePort = runtime.ManagePort
	cfg.USBPath = runtime.USBPath
	cfg.QMIDevice = runtime.QMIDevice
	cfg.AudioDevice = runtime.AudioDevice
	// 策略字段（network/vowifi/airplane/ip/apn/phone_mode/data_strategy）已改为跟卡走、只存在于运行时投影，
	// 不再来自 persisted(config.yaml)。必须取 runtime，否则概览显示恒为 off / wifi。
	cfg.NetworkEnabled = runtime.NetworkEnabled
	cfg.VoWiFiEnabled = runtime.VoWiFiEnabled
	cfg.AirplaneEnabled = runtime.AirplaneEnabled
	cfg.IPVersion = runtime.IPVersion
	cfg.APN = runtime.APN
	cfg.PhoneMode = runtime.PhoneMode
	cfg.DataStrategy = runtime.DataStrategy
	// SMS 是系统不变量（恒开），不随卡策略/投影时序变化，直接置真，
	// 否则 worker 投影完成前或离线时 sms_enabled 为 false，会被短信中心设备过滤掉。
	cfg.SMSEnabled = true
	return cfg
}

func (s *Server) handleDeviceMgmtOverview(c *gin.Context) {
	s.useDeviceMgmtOverview(c)
}

func (s *Server) useDeviceMgmtOverview(c businessContext) {
	includeConfig := strings.TrimSpace(c.DefaultQuery("include_config", "1")) != "0"
	workers := s.pool.GetAllWorkers()
	managed := config.ListDevices()
	cfgByID := map[string]config.DeviceConfig{}
	for _, d := range managed {
		cfgByID[d.ID] = d
	}
	tagByID := map[string]string{}
	tags := make([]string, 0, len(workers))
	for _, w := range workers {
		cfg := w.Config
		if v, ok := cfgByID[w.ID]; ok {
			cfg = overviewDisplayConfig(w.Config, v, true)
		}
		if cfg.Interface == "" {
			continue
		}
		tag := w.ID + "@" + cfg.Interface
		tagByID[w.ID] = tag
		tags = append(tags, tag)
	}
	byTag, _ := db.GetLatestMinuteDeltasBatch("iface", tags)
	now := time.Now()

	workerByID := map[string]bool{}
	items := make([]deviceMgmtOverviewItem, 0, len(workers))
	for _, w := range workers {
		workerByID[w.ID] = true
		cfg := w.Config
		if v, ok := cfgByID[w.ID]; ok {
			cfg = overviewDisplayConfig(w.Config, v, true)
		}
		status := w.GetCachedDeviceStatus() // 设备管理总览列表读缓存，0 IPC
		controlOnline := w.GetCachedHealthy()
		item := deviceMgmtOverviewItem{
			ID:                     w.ID,
			Name:                   cfg.Name,
			Running:                true,
			Healthy:                controlOnline, // 兼容旧客户端：healthy 表示控制面在线
			ControlOnline:          controlOnline,
			PublicIP:               w.GetCachedIP(),
			PublicIPv6:             w.GetCachedIPv6(),
			Modem:                  modemSummaryStatus(status),
			NetworkConnected:       w.NetworkConnected(),
			RegistrationStateLabel: registrationStateLabel(status.RegStatus),
			BackendMode: func() string {
				if w.Backend != nil {
					return w.Backend.Mode()
				}
				return "at"
			}(),
		}
		item.Interface = cfg.Interface
		item.ControlDevice = cfg.ControlDevice
		item.ATPort = w.ResolvedATPort()
		item.USBPath = cfg.USBPath
		if includeConfig {
			dto := deviceConfigToDTO(cfg)
			item.Config = &dto
		}
		if nc := w.NetworkController(); nc != nil {
			item.PrivateIP = nc.GetPrivateIP()
			item.PrivateIPv6 = nc.GetPrivateIPv6()
		}
		item.Traffic, item.TrafficRaw, item.TrafficMeta = buildTrafficOverviewFields(cfg.Interface, byTag[tagByID[w.ID]], now)
		s.applyLifecycleToOverviewItem(&item, true, cfg)
		items = append(items, item)
	}
	for _, dc := range managed {
		if workerByID[dc.ID] {
			continue
		}
		var cfgDTO *deviceConfigDTO
		if includeConfig {
			dto := deviceConfigToDTO(dc)
			cfgDTO = &dto
		}
		item := deviceMgmtOverviewItem{
			ID:                     dc.ID,
			Name:                   dc.Name,
			Running:                false,
			Healthy:                false,
			ControlOnline:          false,
			PublicIP:               "",
			Config:                 cfgDTO,
			Modem:                  modem.DeviceStatus{},
			Traffic:                nil,
			BackendMode:            resolveOfflineBackendMode(dc),
			NetworkConnected:       false,
			RegistrationStateLabel: registrationStateLabel(0),
		}
		s.applyLifecycleToOverviewItem(&item, false, dc)
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"devices": items})
}

type deviceMgmtOverviewLiteItem struct {
	ID                     string                            `json:"id"`
	Name                   string                            `json:"name"`
	Running                bool                              `json:"running"`
	Healthy                bool                              `json:"healthy"`
	ControlOnline          bool                              `json:"control_online"`
	PhysicalPresent        bool                              `json:"physical_present"`
	WorkerRunning          bool                              `json:"worker_running"`
	DataConnected          bool                              `json:"data_connected"`
	RadioRegistered        bool                              `json:"radio_registered"`
	LifecyclePhase         string                            `json:"lifecycle_phase"`
	LifecycleReason        string                            `json:"lifecycle_reason,omitempty"`
	PrivateIP              string                            `json:"private_ip,omitempty"`
	PrivateIPv6            string                            `json:"private_ipv6,omitempty"`
	PublicIP               string                            `json:"public_ip"`
	PublicIPv6             string                            `json:"public_ipv6,omitempty"`
	Interface              string                            `json:"interface,omitempty"`
	ControlDevice          string                            `json:"control_device,omitempty"`
	ESIMTransport          string                            `json:"esim_transport,omitempty"`
	ATPort                 string                            `json:"at_port,omitempty"`
	USBPath                string                            `json:"usb_path,omitempty"`
	AudioDevice            string                            `json:"audio_device,omitempty"`
	LocalPhone             string                            `json:"local_phone,omitempty"`
	E911SetupAvailable     bool                              `json:"e911_setup_available,omitempty"`
	ActiveESIMProfileName  string                            `json:"active_esim_profile_name,omitempty"`
	RFLock                 string                            `json:"rf_lock,omitempty"`
	LebaraIdentityStatus   string                            `json:"lebara_identity_status,omitempty"`
	LebaraIdentityMessage  string                            `json:"lebara_identity_message,omitempty"`
	SMSEnabled             bool                              `json:"sms_enabled"`
	NetworkEnabled         bool                              `json:"network_enabled"`
	PhoneMode              string                            `json:"phone_mode,omitempty"`
	VoWiFiEnabled          bool                              `json:"vowifi_enabled"`
	VoWiFiActive           bool                              `json:"vowifi_active"`
	VoWiFiRuntime          *voWiFiRuntimeDTO                 `json:"vowifi_runtime,omitempty"`
	VoWiFiHealth           *device.WiFiCallingHealthSnapshot `json:"vowifi_health,omitempty"`
	NativeVoLTE            volte.Status                      `json:"native_volte,omitempty"`
	ModemVoice             map[string]interface{}            `json:"modem_voice,omitempty"`
	RadioLiveOK            *bool                             `json:"radio_live_ok,omitempty"`
	Modem                  modem.DeviceStatus                `json:"modem"`
	Traffic                map[string]string                 `json:"traffic,omitempty"`
	TrafficRaw             map[string]int64                  `json:"traffic_raw,omitempty"`
	TrafficMeta            *deviceTrafficMeta                `json:"traffic_meta,omitempty"`
	BackendMode            string                            `json:"backend_mode"`
	NetworkConnected       bool                              `json:"network_connected"`
	RegistrationStateLabel string                            `json:"registration_state_label"`
}

type deviceMgmtListModem struct {
	Operator      string `json:"operator"`
	NativeSPN     string `json:"native_spn,omitempty"`
	NativeMCC     string `json:"native_mcc,omitempty"`
	NativeMNC     string `json:"native_mnc,omitempty"`
	NetworkMode   string `json:"network_mode"`
	NetworkDuplex string `json:"network_duplex"`
	RadioBand     string `json:"radio_band,omitempty"`
	RadioChannel  uint32 `json:"radio_channel,omitempty"`
	SignalDBM     int    `json:"signal_dbm"`
	SignalSINR    int    `json:"signal_sinr,omitempty"`
	IMEI          string `json:"imei,omitempty"`
	ICCID         string `json:"iccid,omitempty"`
	RegStatus     int    `json:"reg_status"`
	PSAttached    bool   `json:"ps_attached"`
}

type deviceMgmtListItem struct {
	ID                     string                            `json:"id"`
	Name                   string                            `json:"name"`
	Running                bool                              `json:"running"`
	Healthy                bool                              `json:"healthy"`
	ControlOnline          bool                              `json:"control_online"`
	PhysicalPresent        bool                              `json:"physical_present"`
	WorkerRunning          bool                              `json:"worker_running"`
	DataConnected          bool                              `json:"data_connected"`
	RadioRegistered        bool                              `json:"radio_registered"`
	LifecyclePhase         string                            `json:"lifecycle_phase"`
	LifecycleReason        string                            `json:"lifecycle_reason,omitempty"`
	PublicIP               string                            `json:"public_ip"`
	PublicIPv6             string                            `json:"public_ipv6,omitempty"`
	Interface              string                            `json:"interface,omitempty"`
	ESIMTransport          string                            `json:"esim_transport,omitempty"`
	SMSEnabled             bool                              `json:"sms_enabled"`
	NetworkEnabled         bool                              `json:"network_enabled"`
	PhoneMode              string                            `json:"phone_mode,omitempty"`
	VoWiFiEnabled          bool                              `json:"vowifi_enabled"`
	VoWiFiActive           bool                              `json:"vowifi_active"`
	VoWiFiRuntime          *voWiFiRuntimeDTO                 `json:"vowifi_runtime,omitempty"`
	VoWiFiHealth           *device.WiFiCallingHealthSnapshot `json:"vowifi_health,omitempty"`
	NativeVoLTE            *volte.Status                     `json:"native_volte,omitempty"`
	ModemVoice             map[string]interface{}            `json:"modem_voice,omitempty"`
	Modem                  deviceMgmtListModem               `json:"modem"`
	NetworkConnected       bool                              `json:"network_connected"`
	RegistrationStateLabel string                            `json:"registration_state_label"`
}

type voWiFiRuntimeDTO struct {
	DeviceID           string    `json:"device_id"`
	Phase              string    `json:"phase"`
	DataplaneMode      string    `json:"dataplane_mode"`
	ICCID              string    `json:"iccid,omitempty"`
	IMSI               string    `json:"imsi,omitempty"`
	SIMReady           bool      `json:"sim_ready"`
	AccessReady        bool      `json:"access_ready"`
	TunnelReady        bool      `json:"tunnel_ready"`
	IMSReady           bool      `json:"ims_ready"`
	SMSReady           bool      `json:"sms_ready"`
	SMSMOReady         bool      `json:"sms_mo_ready"`
	SMSRXPending       bool      `json:"sms_rx_pending"`
	SMSReadyReason     string    `json:"sms_ready_reason,omitempty"`
	RegStatus          int       `json:"reg_status"`
	RegStatusText      string    `json:"reg_status_text"`
	NetworkMode        string    `json:"network_mode"`
	LastErrorClass     string    `json:"last_error_class"`
	LastError          string    `json:"last_error"`
	LastReason         string    `json:"last_reason"`
	UpdatedAt          time.Time `json:"updated_at"`
	MWIKnown           bool      `json:"mwi_known"`
	MWIMessagesWaiting bool      `json:"mwi_messages_waiting"`
	MWIVoiceNew        int       `json:"mwi_voice_new,omitempty"`
	MWIVoiceOld        int       `json:"mwi_voice_old,omitempty"`
	MWIAccount         string    `json:"mwi_account,omitempty"`
	MWIUpdatedAt       time.Time `json:"mwi_updated_at,omitempty"`
}

func runtimeStateToDTO(st runtimehost.State, status modem.DeviceStatus) *voWiFiRuntimeDTO {
	return &voWiFiRuntimeDTO{
		DeviceID:       st.DeviceID,
		Phase:          string(st.Phase),
		DataplaneMode:  st.DataplaneMode,
		ICCID:          strings.TrimSpace(status.ICCID),
		IMSI:           strings.TrimSpace(status.IMSI),
		SIMReady:       st.SIMReady,
		AccessReady:    st.AccessReady,
		TunnelReady:    st.TunnelReady,
		IMSReady:       st.IMSReady,
		SMSReady:       st.SMSReady,
		SMSMOReady:     st.SMSMOReady,
		SMSRXPending:   st.SMSRXPending,
		SMSReadyReason: st.SMSReadyReason,
		RegStatus:      st.RegStatus,
		RegStatusText:  st.RegStatusText,
		NetworkMode:    st.NetworkMode,
		LastErrorClass: st.LastErrorClass,
		LastError:      st.LastError,
		LastReason:     st.LastReason,
		UpdatedAt:      st.UpdatedAt,
	}
}

func runtimeStateSnapshotToDTO(st runtimehost.State, status modem.DeviceStatus) *voWiFiRuntimeDTO {
	if st.Phase == "" && st.UpdatedAt.IsZero() {
		return nil
	}
	return runtimeStateToDTO(st, status)
}

func (s *Server) getVoWiFiRuntimeDTO(deviceID string) *voWiFiRuntimeDTO {
	st, ok := s.pool.GetVoWiFiRuntimeState(deviceID)
	if !ok {
		return nil
	}
	status := modem.DeviceStatus{}
	if w := s.pool.GetWorker(deviceID); w != nil {
		status = w.ProjectDeviceStatus()
	}
	return s.attachVoWiFiMWI(runtimeStateToDTO(st, status), deviceID)
}

func (s *Server) getWiFiCallingHealth(deviceID string) *device.WiFiCallingHealthSnapshot {
	if s == nil || s.pool == nil {
		return nil
	}
	health, ok := s.pool.GetWiFiCallingHealth(deviceID)
	if !ok {
		return nil
	}
	return &health
}

func (s *Server) attachVoWiFiMWI(dto *voWiFiRuntimeDTO, deviceID string) *voWiFiRuntimeDTO {
	if dto == nil || s == nil || s.pool == nil {
		return dto
	}
	mwi := s.pool.GetVoWiFiMWI(deviceID)
	dto.MWIKnown = mwi.Known
	dto.MWIMessagesWaiting = mwi.MessagesWaiting
	dto.MWIVoiceNew = mwi.VoiceNew
	dto.MWIVoiceOld = mwi.VoiceOld
	dto.MWIAccount = mwi.Account
	dto.MWIUpdatedAt = mwi.UpdatedAt
	return dto
}

func isLifecycleActiveForAPI(phase string) bool {
	switch phase {
	case string(device.LifecyclePhaseRebooting),
		string(device.LifecyclePhaseUSBWait),
		string(device.LifecyclePhaseWorkerStarting),
		string(device.LifecyclePhaseQMIStarting),
		string(device.LifecyclePhaseRecovering):
		return true
	default:
		return false
	}
}

func lifecyclePhaseString(snap device.LifecycleSnapshot) string {
	if snap.Phase == "" {
		return string(device.LifecyclePhaseOffline)
	}
	return string(snap.Phase)
}

func lifecycleSnapshotForAPI(pool *device.Pool, deviceID string) device.LifecycleSnapshot {
	if pool == nil {
		return device.LifecycleSnapshot{Phase: device.LifecyclePhaseOffline}
	}
	return pool.LifecycleSnapshot(deviceID)
}

func lifecyclePhaseForAPI(snap device.LifecycleSnapshot, workerRunning bool, controlOnline bool) string {
	phase := lifecyclePhaseString(snap)
	if workerRunning && controlOnline {
		switch phase {
		case string(device.LifecyclePhaseOffline),
			string(device.LifecyclePhaseUSBWait),
			string(device.LifecyclePhaseWorkerStarting),
			string(device.LifecyclePhaseQMIStarting),
			string(device.LifecyclePhaseRecovering):
			return string(device.LifecyclePhaseOnline)
		}
	}
	return phase
}

func (s *Server) applyLifecycleToOverviewItem(item *deviceMgmtOverviewItem, workerRunning bool, cfg config.DeviceConfig) {
	if item == nil {
		return
	}
	snap := lifecycleSnapshotForAPI(s.pool, cfg.ID)
	phase := lifecyclePhaseForAPI(snap, workerRunning, item.ControlOnline)
	item.WorkerRunning = workerRunning
	item.DataConnected = item.NetworkConnected
	item.RadioRegistered = item.Modem.RegStatus == 1 || item.Modem.RegStatus == 5
	item.LifecyclePhase = phase
	item.LifecycleReason = snap.Reason
	item.PhysicalPresent = workerRunning || isLifecycleActiveForAPI(phase)
}

func (s *Server) applyLifecycleToListItem(item *deviceMgmtListItem, workerRunning bool, cfg config.DeviceConfig) {
	if item == nil {
		return
	}
	snap := lifecycleSnapshotForAPI(s.pool, cfg.ID)
	phase := lifecyclePhaseForAPI(snap, workerRunning, item.ControlOnline)
	item.WorkerRunning = workerRunning
	item.DataConnected = item.NetworkConnected
	item.RadioRegistered = item.Modem.RegStatus == 1 || item.Modem.RegStatus == 5
	item.LifecyclePhase = phase
	item.LifecycleReason = snap.Reason
	item.PhysicalPresent = workerRunning || isLifecycleActiveForAPI(phase)
}

func (s *Server) applyLifecycleToOverviewLiteItem(item *deviceMgmtOverviewLiteItem, worker *device.Worker, cfg config.DeviceConfig) {
	if item == nil {
		return
	}
	workerRunning := worker != nil
	snap := lifecycleSnapshotForAPI(s.pool, cfg.ID)
	phase := lifecyclePhaseForAPI(snap, workerRunning, item.ControlOnline)
	item.WorkerRunning = workerRunning
	item.DataConnected = item.NetworkConnected
	item.RadioRegistered = item.Modem.RegStatus == 1 || item.Modem.RegStatus == 5
	item.LifecyclePhase = phase
	item.LifecycleReason = snap.Reason
	item.PhysicalPresent = workerRunning || isLifecycleActiveForAPI(phase)
}

func (s *Server) buildOverviewLiteItemFromWorker(w *device.Worker, cfg config.DeviceConfig, status modem.DeviceStatus, radioLiveOK *bool) deviceMgmtOverviewLiteItem {
	item := s.buildOverviewLiteItemFromWorkerWithModem(w, cfg, status, radioLiveOK, status)
	item.Modem = modemSummaryStatus(item.Modem)
	return item
}

func (s *Server) buildOverviewLiteDetailItemFromWorker(w *device.Worker, cfg config.DeviceConfig, status modem.DeviceStatus, radioLiveOK *bool) deviceMgmtOverviewLiteItem {
	return s.buildOverviewLiteItemFromWorkerWithModem(w, cfg, status, radioLiveOK, status)
}

func (s *Server) buildOverviewLiteItemFromWorkerWithModem(w *device.Worker, cfg config.DeviceConfig, status modem.DeviceStatus, radioLiveOK *bool, modemStatus modem.DeviceStatus) deviceMgmtOverviewLiteItem {
	controlOnline := w.GetCachedHealthy()
	item := deviceMgmtOverviewLiteItem{
		ID:                     w.ID,
		Name:                   cfg.Name,
		Running:                true,
		Healthy:                controlOnline,
		ControlOnline:          controlOnline,
		PublicIP:               w.GetCachedIP(),
		PublicIPv6:             w.GetCachedIPv6(),
		Interface:              cfg.Interface,
		ControlDevice:          cfg.ControlDevice,
		ESIMTransport:          config.NormalizeESIMTransport(cfg.ESIMTransport),
		ATPort:                 w.ResolvedATPort(),
		USBPath:                cfg.USBPath,
		AudioDevice:            cfg.AudioDevice,
		LocalPhone:             overviewLocalPhone(effectiveOverviewIMSI(w, status), strings.TrimSpace(status.ICCID)),
		E911SetupAvailable:     e911.SetupAvailable(modemStatus),
		SMSEnabled:             cfg.SMSEnabled,
		NetworkEnabled:         cfg.NetworkEnabled,
		PhoneMode:              overviewPhoneMode(cfg.PhoneMode),
		VoWiFiEnabled:          cardPolicyVoWiFiEnabled(strings.TrimSpace(status.ICCID), cfg.VoWiFiEnabled),
		VoWiFiActive:           s.pool.IsVoWiFiActive(w.ID),
		VoWiFiRuntime:          s.getVoWiFiRuntimeDTO(w.ID),
		VoWiFiHealth:           s.getWiFiCallingHealth(w.ID),
		NativeVoLTE:            s.pool.NativeVoLTEStatus(w.ID),
		ModemVoice:             s.pool.ModemVoiceStatus(w.ID),
		RadioLiveOK:            radioLiveOK,
		Modem:                  modemStatus,
		NetworkConnected:       w.NetworkConnected(),
		RegistrationStateLabel: registrationStateLabel(status.RegStatus),
		BackendMode: func() string {
			if w.Backend != nil {
				return w.Backend.Mode()
			}
			return "at"
		}(),
	}
	if nc := w.NetworkController(); nc != nil {
		item.PrivateIP = nc.GetPrivateIP()
		item.PrivateIPv6 = nc.GetPrivateIPv6()
	}
	if w.EsimMgr != nil {
		if name, err := w.EsimMgr.ActiveProfileName(); err == nil {
			item.ActiveESIMProfileName = name
		}
	}
	class, err := device.ClassifyWorkerLebaraUK(w)
	if err != nil {
		logger.Warn("设备概览识别 Lebara UK 射频策略失败", "device", w.ID, "err", err)
	}
	item.RFLock = class.RFLock()
	if recoverSnap := s.pool.LebaraUKIdentityRecoverSnapshot(w.ID); recoverSnap.Status != "" {
		item.LebaraIdentityStatus = recoverSnap.Status
		item.LebaraIdentityMessage = recoverSnap.Message
	}
	s.applyLifecycleToOverviewLiteItem(&item, w, cfg)
	return item
}

type overviewStreamEmitVersion struct {
	VoWiFiActive          bool
	LifecyclePhase        string
	LifecycleReason       string
	LocalPhone            string
	ICCID                 string
	IMSI                  string
	HasRuntime            bool
	Phase                 string
	SIMReady              bool
	AccessReady           bool
	TunnelReady           bool
	IMSReady              bool
	SMSReady              bool
	SMSMOReady            bool
	LastErrorClass        string
	LebaraIdentityStatus  string
	LebaraIdentityMessage string
}

func newOverviewStreamEmitVersion(item deviceMgmtOverviewLiteItem) overviewStreamEmitVersion {
	v := overviewStreamEmitVersion{
		VoWiFiActive:          item.VoWiFiActive,
		LifecyclePhase:        item.LifecyclePhase,
		LifecycleReason:       item.LifecycleReason,
		LocalPhone:            strings.TrimSpace(item.LocalPhone),
		ICCID:                 strings.TrimSpace(item.Modem.ICCID),
		IMSI:                  strings.TrimSpace(item.Modem.IMSI),
		LebaraIdentityStatus:  item.LebaraIdentityStatus,
		LebaraIdentityMessage: item.LebaraIdentityMessage,
	}
	if item.VoWiFiRuntime != nil {
		v.HasRuntime = true
		v.Phase = item.VoWiFiRuntime.Phase
		v.SIMReady = item.VoWiFiRuntime.SIMReady
		v.AccessReady = item.VoWiFiRuntime.AccessReady
		v.TunnelReady = item.VoWiFiRuntime.TunnelReady
		v.IMSReady = item.VoWiFiRuntime.IMSReady
		v.SMSReady = item.VoWiFiRuntime.SMSReady
		v.SMSMOReady = item.VoWiFiRuntime.SMSMOReady
		v.LastErrorClass = item.VoWiFiRuntime.LastErrorClass
	}
	return v
}

func shouldSkipOverviewStatePush(last *overviewStreamEmitVersion, curr overviewStreamEmitVersion) bool {
	if last == nil {
		return false
	}
	return *last == curr
}

func effectiveOverviewIMSI(w *device.Worker, status modem.DeviceStatus) string {
	if w != nil && w.SIMIdentitySuppressesOverviewIMSI() {
		return ""
	}
	imsi := strings.TrimSpace(status.IMSI)
	if imsi != "" {
		return imsi
	}
	if w == nil {
		return ""
	}
	if !w.SIMIdentityAllowsOverviewFallback() {
		return ""
	}
	return strings.TrimSpace(w.GetCachedIMSI())
}

func overviewLocalPhone(imsi, iccid string) string {
	imsi = strings.TrimSpace(imsi)
	iccid = strings.TrimSpace(iccid)
	if imsi == "" && iccid == "" {
		return ""
	}
	phone, err := db.GetPhoneNumberForSMSIdentity(db.SMSIdentity{ICCID: iccid, IMSI: imsi})
	if err != nil {
		logger.Error("读取设备本机号码失败", "err", err)
		return ""
	}
	return strings.TrimSpace(phone)
}

func (s *Server) handleDeviceMgmtList(c *gin.Context) {
	s.useDeviceMgmtList(c)
}

func (s *Server) useDeviceMgmtList(c businessContext) {
	workers := s.pool.GetAllWorkers()
	managed := config.ListDevices()
	cfgByID := map[string]config.DeviceConfig{}
	for _, d := range managed {
		cfgByID[d.ID] = d
	}

	workerByID := map[string]bool{}
	items := make([]deviceMgmtListItem, 0, len(workers))
	for _, w := range workers {
		workerByID[w.ID] = true
		cfg := w.Config
		if v, ok := cfgByID[w.ID]; ok {
			cfg = overviewDisplayConfig(w.Config, v, true)
		}
		status := w.GetCachedDeviceStatus()
		controlOnline := w.GetCachedHealthy()
		item := deviceMgmtListItem{
			ID:                     w.ID,
			Name:                   cfg.Name,
			Running:                true,
			Healthy:                controlOnline,
			ControlOnline:          controlOnline,
			PublicIP:               w.GetCachedIP(),
			PublicIPv6:             w.GetCachedIPv6(),
			Interface:              cfg.Interface,
			ESIMTransport:          config.NormalizeESIMTransport(cfg.ESIMTransport),
			SMSEnabled:             cfg.SMSEnabled,
			NetworkEnabled:         cfg.NetworkEnabled,
			PhoneMode:              overviewPhoneMode(cfg.PhoneMode),
			VoWiFiEnabled:          cfg.VoWiFiEnabled,
			VoWiFiActive:           s.pool.IsVoWiFiActive(w.ID),
			VoWiFiRuntime:          s.getVoWiFiRuntimeDTO(w.ID),
			VoWiFiHealth:           s.getWiFiCallingHealth(w.ID),
			NetworkConnected:       w.NetworkConnected(),
			RegistrationStateLabel: registrationStateLabel(status.RegStatus),
			Modem: deviceMgmtListModem{
				Operator:      status.Operator,
				NativeSPN:     status.NativeSPN,
				NativeMCC:     status.NativeMCC,
				NativeMNC:     status.NativeMNC,
				NetworkMode:   status.NetworkMode,
				NetworkDuplex: status.NetworkDuplex,
				RadioBand:     status.RadioBand,
				RadioChannel:  status.RadioChannel,
				SignalDBM:     status.SignalDBM,
				SignalSINR:    status.SignalSINR,
				IMEI:          status.IMEI,
				ICCID:         status.ICCID,
				RegStatus:     status.RegStatus,
				PSAttached:    status.PSAttached,
			},
		}
		if device.IsNativeVoLTEMode(cfg.PhoneMode) {
			volteStatus := s.pool.NativeVoLTEStatus(w.ID)
			item.NativeVoLTE = &volteStatus
		}
		item.ModemVoice = s.pool.ModemVoiceStatus(w.ID)
		s.applyLifecycleToListItem(&item, true, cfg)
		items = append(items, item)
	}

	for _, dc := range managed {
		if workerByID[dc.ID] {
			continue
		}
		item := deviceMgmtListItem{
			ID:                     dc.ID,
			Name:                   dc.Name,
			Running:                false,
			Healthy:                false,
			ControlOnline:          false,
			PublicIP:               "",
			Interface:              dc.Interface,
			ESIMTransport:          config.NormalizeESIMTransport(dc.ESIMTransport),
			SMSEnabled:             true, // SMS 恒开（系统不变量）
			NetworkEnabled:         dc.NetworkEnabled,
			PhoneMode:              overviewPhoneMode(dc.PhoneMode),
			VoWiFiEnabled:          false, // 非运行设备无活跃 VoWiFi
			VoWiFiActive:           false,
			VoWiFiHealth:           s.getWiFiCallingHealth(dc.ID),
			NetworkConnected:       false,
			RegistrationStateLabel: registrationStateLabel(0),
			Modem:                  deviceMgmtListModem{},
		}
		s.applyLifecycleToListItem(&item, false, dc)
		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{"devices": items, "device_limit": device.DefaultFreeDeviceLimit})
}

// handleDeviceMgmtRefreshInfo 主动触发设备底层重新采集各种信息（SIM、信号等）
func (s *Server) handleDeviceMgmtRefreshInfo(c *gin.Context) {
	s.useDeviceMgmtRefreshInfo(c)
}

func (s *Server) useDeviceMgmtRefreshInfo(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	if id == "" || s == nil || s.pool == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "设备参数无效"})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()
	if s.pool.IsESIMSwitching(id) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "设备正在切换 SIM，暂不能刷新设备信息"})
		return
	}
	worker := s.pool.GetWorker(id)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到或未运行"})
		return
	}

	// 阻塞式刷新，后续执行的 overview-lite 就能马上获取到最新的状态
	if worker.Backend != nil && worker.Backend.Mode() != "at" {
		_ = worker.RefreshRuntime(businessRequestContext(c), "manual_refresh")
		_ = worker.RefreshIdentityLive(businessRequestContext(c), "manual_refresh")
		s.pool.PersistRuntimeState(worker)
		s.pool.PersistIdentityState(worker)
	} else if worker.Modem != nil {
		worker.Modem.RefreshDeviceInfo()
		_ = worker.RefreshRuntime(businessRequestContext(c), "manual_refresh")
		_ = worker.RefreshIdentityLive(businessRequestContext(c), "manual_refresh")
		s.pool.PersistRuntimeState(worker)
		s.pool.PersistIdentityState(worker)
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "设备信息刷新完成"})
}

func (s *Server) handleDeviceMgmtOverviewLite(c *gin.Context) {
	s.useDeviceMgmtOverviewLite(c)
}

func (s *Server) useDeviceMgmtOverviewLite(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	if id == "" {
		id = strings.TrimSpace(c.Query("id"))
	}
	liveRefresh := overviewDetailLiveRefreshRequested(c)
	workers := s.pool.GetAllWorkers()
	managed := config.ListDevices()
	cfgByID := map[string]config.DeviceConfig{}
	for _, d := range managed {
		cfgByID[d.ID] = d
	}

	if id != "" {
		for _, w := range workers {
			if w.ID != id {
				continue
			}
			cfg := w.Config
			if v, ok := cfgByID[w.ID]; ok {
				cfg = overviewDisplayConfig(w.Config, v, true)
			}
			if liveRefresh {
				_ = w.RefreshRuntime(businessRequestContext(c), "overview_detail")
				_ = w.RefreshIdentityLive(businessRequestContext(c), "overview_detail")
			}
			status := w.ProjectDeviceStatus()
			var radioLiveOK *bool
			if liveRefresh && (w.Backend == nil || w.Backend.Mode() == backend.BackendAT) {
				radioLiveOK = new(bool)
				*radioLiveOK = true
			}
			item := s.buildOverviewLiteDetailItemFromWorker(w, cfg, status, radioLiveOK)
			controlOnline := w.GetCachedHealthy()
			item.Healthy = controlOnline
			item.ControlOnline = controlOnline
			s.applyLifecycleToOverviewLiteItem(&item, w, cfg)
			tag := w.ID + "@" + cfg.Interface
			ps, rx, tx, _ := db.GetLatestMinuteDeltas("iface", tag)
			item.Traffic, item.TrafficRaw, item.TrafficMeta = buildTrafficOverviewFields(cfg.Interface, db.LatestMinuteDeltas{
				PeriodStart: ps,
				RxBytes:     rx,
				TxBytes:     tx,
			}, time.Now())
			c.JSON(http.StatusOK, gin.H{"devices": []deviceMgmtOverviewLiteItem{item}})
			return
		}

		if dc, err := config.GetDeviceByID(id); err == nil && dc != nil {
			pol := resolveOfflineDevicePolicy(id)
			item := deviceMgmtOverviewLiteItem{
				ID:                     dc.ID,
				Name:                   dc.Name,
				Running:                false,
				Healthy:                false,
				ControlOnline:          false,
				PublicIP:               "",
				Interface:              dc.Interface,
				ControlDevice:          dc.ControlDevice,
				ESIMTransport:          config.NormalizeESIMTransport(dc.ESIMTransport),
				ATPort:                 dc.ATPort,
				USBPath:                dc.USBPath,
				SMSEnabled:             pol.SMSEnabled,
				NetworkEnabled:         pol.NetworkEnabled,
				VoWiFiEnabled:          pol.VoWiFiEnabled,
				VoWiFiActive:           false,
				VoWiFiHealth:           s.getWiFiCallingHealth(id),
				NetworkConnected:       false,
				RegistrationStateLabel: registrationStateLabel(0),
				Modem:                  offlineModemStatus(id),
				Traffic:                nil,
			}
			s.applyLifecycleToOverviewLiteItem(&item, nil, *dc)
			c.JSON(http.StatusOK, gin.H{"devices": []deviceMgmtOverviewLiteItem{item}})
			return
		}

		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到: " + id})
		return
	}

	tagByID := map[string]string{}
	tags := make([]string, 0, len(workers))
	for _, w := range workers {
		cfg := w.Config
		if v, ok := cfgByID[w.ID]; ok {
			cfg = overviewDisplayConfig(w.Config, v, true)
		}
		if cfg.Interface == "" {
			continue
		}
		tag := w.ID + "@" + cfg.Interface
		tagByID[w.ID] = tag
		tags = append(tags, tag)
	}
	byTag, _ := db.GetLatestMinuteDeltasBatch("iface", tags)
	now := time.Now()

	workerByID := map[string]bool{}
	items := make([]deviceMgmtOverviewLiteItem, 0, len(workers))
	for _, w := range workers {
		workerByID[w.ID] = true
		cfg := w.Config
		if v, ok := cfgByID[w.ID]; ok {
			cfg = overviewDisplayConfig(w.Config, v, true)
		}
		status := w.GetCachedDeviceStatus() // 批量列表读缓存，0 IPC
		item := s.buildOverviewLiteItemFromWorker(w, cfg, status, nil)
		item.Traffic, item.TrafficRaw, item.TrafficMeta = buildTrafficOverviewFields(cfg.Interface, byTag[tagByID[w.ID]], now)
		items = append(items, item)
	}
	for _, dc := range managed {
		if workerByID[dc.ID] {
			continue
		}
		item := deviceMgmtOverviewLiteItem{
			ID:                     dc.ID,
			Name:                   dc.Name,
			Running:                false,
			Healthy:                false,
			ControlOnline:          false,
			PublicIP:               "",
			Interface:              dc.Interface,
			ControlDevice:          dc.ControlDevice,
			ESIMTransport:          config.NormalizeESIMTransport(dc.ESIMTransport),
			ATPort:                 dc.ATPort,
			SMSEnabled:             true, // SMS 恒开（系统不变量）
			NetworkEnabled:         dc.NetworkEnabled,
			VoWiFiActive:           false, // 非运行设备无活跃 VoWiFi
			VoWiFiHealth:           s.getWiFiCallingHealth(dc.ID),
			NetworkConnected:       false,
			RegistrationStateLabel: registrationStateLabel(0),
			Modem:                  modem.DeviceStatus{},
			Traffic:                nil,
		}
		s.applyLifecycleToOverviewLiteItem(&item, nil, dc)
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"devices": items})
}

func overviewDetailLiveRefreshRequested(c businessContext) bool {
	if c == nil {
		return false
	}
	for _, key := range []string{"refresh", "live"} {
		switch strings.ToLower(strings.TrimSpace(c.Query(key))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func (s *Server) handleDeviceMgmtGetDeviceConfig(c *gin.Context) {
	s.useDeviceMgmtGetDeviceConfig(c)
}

func (s *Server) useDeviceMgmtGetDeviceConfig(c businessContext) {
	id := deviceIDParam(c)
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}
	md, err := config.GetDeviceByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "读取设备配置失败: " + err.Error()})
		return
	}
	if md == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}
	cfgDTO := deviceConfigToDTO(*md)
	if worker := s.pool.GetWorker(id); worker != nil {
		cfgDTO.Interface = worker.Config.Interface
		cfgDTO.ControlDevice = worker.Config.ControlDevice
		cfgDTO.ATPort = worker.ResolvedATPort()
		cfgDTO.USBPath = worker.Config.USBPath
	}
	c.JSON(http.StatusOK, gin.H{"config": cfgDTO})
}

type discoveredDevice struct {
	DiscoveryKey   string   `json:"discovery_key"`
	ControlPath    string   `json:"control_path"`
	NetInterface   string   `json:"net_interface"`
	USBPath        string   `json:"usb_path"`
	IMEI           string   `json:"imei,omitempty"`
	VendorID       uint16   `json:"vendor_id"`
	ProductID      uint16   `json:"product_id"`
	DriverName     string   `json:"driver_name"`
	ATPorts        []string `json:"at_ports"`
	ATPort         string   `json:"at_port"`
	AudioDevice    string   `json:"audio_device,omitempty"`
	Mode           string   `json:"mode,omitempty"`  // qmi/mbim/ecm/rndis/ncm/unknown
	NetworkCapable bool     `json:"network_capable"` // 是否可由 QMI Core 接管
	Configured     bool     `json:"configured"`
	ConfiguredID   string   `json:"configured_id,omitempty"`
	Degraded       bool     `json:"degraded,omitempty"` // 探不到 IMEI,无法确立身份,不可直接添加
	HardwareKind   string   `json:"hardware_kind,omitempty"`
	ReaderName     string   `json:"reader_name,omitempty"`
	CardPresent    bool     `json:"card_present,omitempty"`
	ATR            string   `json:"atr,omitempty"`
}

var discoverQMIForMgmtFn = device.DiscoverQMIDevices
var discoverCompatibleModemsFromQMIFn = device.DiscoverCompatibleModemsFromQMI
var enrichDiscoveredCompatibleModemFn = device.EnrichDiscoveredCompatibleModem
var probeIMEIForAddFn = device.ProbeIMEIViaQMI
var probeIMEIViaMBIMForMgmtFn = device.ProbeIMEIViaMBIM
var discoverPCSCReadersForMgmtFn = func(ctx context.Context) ([]pcsc.Reader, error) {
	return pcsc.New().Readers(ctx)
}

func ensureAddDeviceIMEI(cfg config.DeviceConfig, probe func(string) (string, error)) (config.DeviceConfig, error) {
	if strings.TrimSpace(cfg.ControlDevice) == "" || config.NormalizeIMEI(cfg.ModemIMEI) != "" {
		return cfg, nil
	}
	probed, err := probe(cfg.ControlDevice)
	if err != nil || strings.TrimSpace(probed) == "" {
		return cfg, fmt.Errorf("IMEI 探测失败，请重新插拔设备或稍后重试")
	}
	cfg.ModemIMEI = strings.TrimSpace(probed)
	return cfg, nil
}

func (s *Server) handleDeviceMgmtDiscovered(c *gin.Context) {
	s.useDeviceMgmtDiscovered(c)
}

func (s *Server) useDeviceMgmtDiscovered(c businessContext) {
	pcscReaders, pcscErr := discoverPCSCReadersForMgmtFn(businessRequestContext(c))
	discoveredQMI, err := discoverQMIForMgmtFn()
	if err != nil {
		discoveredQMI = nil
	}

	list, err := discoverCompatibleModemsFromQMIFn(discoveredQMI)
	if err != nil {
		list = nil
	}

	withIMEI := strings.TrimSpace(c.Query("with_imei")) == "1"
	managedIndex := device.BuildWorkerDiscoveryIndex(s.pool.GetAllWorkers(), withIMEI)

	managed := config.ListDevices()

	// 第一阶段:并行补全每块硬件(AT/IMEI探测),不做任何身份判定。
	enriched := make([]device.CompatibleModem, len(list))
	var wg sync.WaitGroup

	for i, d := range list {
		wg.Add(1)
		go func(idx int, dev device.CompatibleModem) {
			defer wg.Done()

			resolvedATPort := strings.TrimSpace(dev.ATPort)
			imei := strings.TrimSpace(dev.IMEI)

			if withIMEI {
				live, handled := managedIndex.ResolveLiveATIdentity(dev, func(raw device.CompatibleModem) (device.CompatibleModem, string) {
					return enrichDiscoveredCompatibleModemFn(raw, device.CompatibleModemEnrichOptions{
						EnableATProbe: true, ATProbeTimeout: 900 * time.Millisecond,
					})
				})
				if handled {
					dev = live
					resolvedATPort, imei = dev.ATPort, dev.IMEI
				} else {
					managedMatch, hasManaged := managedIndex.Lookup(strings.TrimSpace(dev.ControlPath), strings.TrimSpace(dev.USBPath), strings.TrimSpace(dev.NetInterface))
					if hasManaged {
						if containsDiscoveredATPort(dev.ATPorts, managedMatch.ATPort) {
							resolvedATPort = managedMatch.ATPort
						}
						if imei == "" {
							imei = managedMatch.IMEI
						}
					} else {
						probed, discoveredIMEI := enrichDiscoveredCompatibleModemFn(dev, device.CompatibleModemEnrichOptions{
							EnableATProbe:      true,
							ATProbeTimeout:     900 * time.Millisecond,
							EnableQMIIMEIProbe: strings.TrimSpace(dev.ControlPath) != "" && dev.Mode != "mbim",
						})
						dev = probed
						if resolved := strings.TrimSpace(probed.ATPort); resolved != "" {
							resolvedATPort = resolved
						}
						if imei == "" {
							imei = discoveredIMEI
						}
						// MBIM 设备没有 AT 端口也不支持 QMI，使用 MBIM DeviceCaps 探测 IMEI
						if imei == "" && dev.Mode == "mbim" && strings.TrimSpace(dev.ControlPath) != "" {
							if mbimIMEI, err := probeIMEIViaMBIMForMgmtFn(dev.ControlPath); err == nil && mbimIMEI != "" {
								imei = mbimIMEI
							}
						}
					}
				}
			}

			mode := strings.ToLower(strings.TrimSpace(dev.Mode))
			networkCapable := dev.NetworkCapable

			if mode == "" {
				mode = "unknown"
			}

			dev.IMEI = imei
			dev.ATPort = resolvedATPort
			dev.Mode = mode
			dev.NetworkCapable = networkCapable
			enriched[idx] = dev
		}(i, d)
	}
	wg.Wait()

	// 第二阶段:统一身份解析(按 IMEI),路径不再是身份。
	resolved := device.ResolveDeviceIdentities(enriched, managed)

	out := make([]discoveredDevice, 0, len(enriched))
	for _, pair := range resolved.Matched {
		out = append(out, buildDiscoveredDevice(pair.Hardware, true, pair.Config.ID, false))
	}
	for _, hw := range resolved.Unmatched {
		out = append(out, buildDiscoveredDevice(hw, false, "", false))
	}
	for _, hw := range resolved.Degraded {
		out = append(out, buildDiscoveredDevice(hw, false, "", true))
	}
	if pcscErr == nil {
		out = append(out, buildDiscoveredPCSCDevices(pcscReaders, managed)...)
	}

	response := gin.H{"devices": out}
	if pcscErr != nil {
		response["pcsc_error"] = pcscErr.Error()
	}
	c.JSON(http.StatusOK, response)
}

func buildDiscoveredPCSCDevices(readers []pcsc.Reader, managed []config.DeviceConfig) []discoveredDevice {
	configuredByPath := make(map[string]string)
	configuredByName := make(map[string]string)
	for _, cfg := range managed {
		if strings.EqualFold(strings.TrimSpace(cfg.DeviceBackend), pcsc.HardwareKind) {
			configuredByPath[strings.TrimSpace(cfg.PCSCUSBPath)] = cfg.ID
			configuredByName[strings.TrimSpace(cfg.PCSCReaderName)] = cfg.ID
		}
	}
	result := make([]discoveredDevice, 0, len(readers))
	for _, reader := range readers {
		configuredID := configuredByPath[strings.TrimSpace(reader.USBPath)]
		if configuredID == "" {
			configuredID = configuredByName[strings.TrimSpace(reader.Name)]
		}
		result = append(result, discoveredDevice{
			DiscoveryKey: pcsc.DeviceID(reader), ControlPath: reader.Name, USBPath: reader.USBPath,
			DriverName: "pcsc", Mode: pcsc.HardwareKind, HardwareKind: pcsc.HardwareKind,
			ReaderName: reader.Name, CardPresent: reader.CardPresent, ATR: reader.ATR,
			NetworkCapable: false, Configured: configuredID != "", ConfiguredID: configuredID,
			Degraded: !reader.CardPresent,
		})
	}
	return result
}

func buildDiscoveredDevice(hw device.CompatibleModem, configured bool, configuredID string, degraded bool) discoveredDevice {
	return discoveredDevice{
		DiscoveryKey:   hw.DiscoveryKey(),
		ControlPath:    hw.ControlPath,
		NetInterface:   hw.NetInterface,
		USBPath:        hw.USBPath,
		IMEI:           strings.TrimSpace(hw.IMEI),
		VendorID:       hw.VendorID,
		ProductID:      hw.ProductID,
		DriverName:     hw.DriverName,
		ATPorts:        hw.ATPorts,
		ATPort:         hw.ATPort,
		AudioDevice:    hw.AudioDevice,
		Mode:           hw.Mode,
		NetworkCapable: hw.NetworkCapable,
		Configured:     configured,
		ConfiguredID:   configuredID,
		Degraded:       degraded,
	}
}

func containsDiscoveredATPort(ports []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, port := range ports {
		if strings.TrimSpace(port) == target {
			return true
		}
	}
	return false
}

type setFlightModeRequest struct {
	Enabled bool `json:"enabled"`
}

func isFlightModeEnabled(mode int) bool {
	return mode == int(backend.ModeLowPower) || mode == int(backend.ModeRFOff)
}

func flightModeSuccessMessage(enabled bool) string {
	if enabled {
		return "飞行模式已开启"
	}
	return "飞行模式已关闭"
}

func setWorkerFlightMode(ctx context.Context, worker *device.Worker, flightModeEnabled bool) (operatingMode int, flightMode bool, err error) {
	targetMode := backend.ModeOnline
	expectedMode := int(backend.ModeOnline)
	if flightModeEnabled {
		targetMode = backend.ModeRFOff
		expectedMode = int(backend.ModeRFOff)
	}

	if worker.Backend != nil {
		if err := worker.Backend.SetOperatingMode(ctx, targetMode); err != nil {
			return 0, false, err
		}
		opMode, err := worker.Backend.GetOperatingMode(ctx)
		if err != nil {
			return expectedMode, isFlightModeEnabled(expectedMode), nil
		}
		operatingMode = int(opMode)
		return operatingMode, isFlightModeEnabled(operatingMode), nil
	}

	return 0, false, fmt.Errorf("设备后端未初始化，无法切换飞行模式")
}

type updateDeviceRequest struct {
	Config deviceConfigDTO `json:"config"`
}

func hasManagedNetworkCapability(cfg config.DeviceConfig) bool {
	return strings.TrimSpace(cfg.ControlDevice) != "" && strings.TrimSpace(cfg.Interface) != ""
}

func validateManagedNetworkConfig(cfg config.DeviceConfig) error {
	if err := validateDeviceBackendConfig(cfg); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(cfg.DeviceBackend), pcsc.HardwareKind) {
		if err := pcscSelectorFromConfig(cfg).Validate(); err != nil {
			return err
		}
		return validateSIMPINEnvName(cfg.SIMPINEnv)
	}
	if _, _, err := config.ResolveIPFamily(cfg.IPVersion); err != nil {
		return err
	}
	// 零路径持久化后 control_device/interface 由运行时从 IMEI 发现，不再作为保存前置条件。
	return nil
}

func pcscSelectorFromConfig(cfg config.DeviceConfig) pcsc.Selector {
	return pcsc.Selector{USBPath: cfg.PCSCUSBPath, ReaderName: cfg.PCSCReaderName}
}

func validateSIMPINEnvName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	for index, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return fmt.Errorf("SIM PIN 环境变量名无效: %q", name)
	}
	return nil
}

func normalizeManagedDeviceConfig(cfg config.DeviceConfig) (config.DeviceConfig, string) {
	if cfg.VoWiFiEnabled && cfg.NetworkEnabled {
		cfg.NetworkEnabled = false
		return cfg, "VoWiFi 已启用"
	}
	return cfg, ""
}

func deviceConfigForAdd(cfg config.DeviceConfig) config.DeviceConfig {
	cfg.APN = ""
	cfg.IPVersion = ""
	cfg.NetworkEnabled = false
	cfg.VoWiFiEnabled = false
	cfg.AirplaneEnabled = true
	cfg.SMSEnabled = true
	return cfg
}

func joinWarningMessages(parts ...string) string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, "；")
}

type deviceBindingConflict struct {
	Field   string
	Value   string
	OtherID string
}

func (e *deviceBindingConflict) Error() string {
	return fmt.Sprintf("设备资源冲突：%s=%s 已被设备 %s 使用", e.Field, e.Value, e.OtherID)
}

type deviceConfigConflictError struct{ message string }

type deviceBindingKey struct {
	field string
	value string
}

func (e *deviceConfigConflictError) Error() string { return e.message }

func detectDeviceBindingConflictInList(cfg config.DeviceConfig, excludeID string, devices []config.DeviceConfig) *deviceBindingConflict {
	claimed := make(map[string]string)
	for _, binding := range deviceBindingKeys(cfg) {
		claimed[binding.field] = binding.value
	}
	if len(claimed) == 0 {
		return nil
	}
	for _, existing := range devices {
		existingID := strings.TrimSpace(existing.ID)
		if existingID == "" || existingID == strings.TrimSpace(excludeID) {
			continue
		}
		for _, binding := range deviceBindingKeys(existing) {
			if claimed[binding.field] == binding.value {
				return &deviceBindingConflict{
					Field: binding.field, Value: binding.value, OtherID: existingID,
				}
			}
		}
	}
	return nil
}

func deviceBindingKeys(cfg config.DeviceConfig) []deviceBindingKey {
	bindings := make([]deviceBindingKey, 0, 7)
	appendBinding := func(field, value string) {
		if value = strings.TrimSpace(value); value != "" {
			bindings = append(bindings, deviceBindingKey{field: field, value: value})
		}
	}
	appendBinding("modem_imei", config.NormalizeIMEI(cfg.ModemIMEI))
	appendBinding("control_device", cfg.ControlDevice)
	appendBinding("usb_path", cfg.USBPath)
	appendBinding("interface", cfg.Interface)
	appendBinding("at_port", cfg.ATPort)
	appendBinding("pcsc_reader_name", cfg.PCSCReaderName)
	appendBinding("pcsc_usb_path", cfg.PCSCUSBPath)
	return bindings
}

func managedDeviceListValidator(
	cfg config.DeviceConfig,
	excludeID string,
	enforceLimit bool,
) config.DeviceListValidator {
	return func(devices []config.DeviceConfig) error {
		if conflict := detectDeviceBindingConflictInList(cfg, excludeID, devices); conflict != nil {
			return conflict
		}
		if enforceLimit {
			if err := validateFreeDeviceConfigLimit(devices); err != nil {
				return &deviceConfigConflictError{message: err.Error()}
			}
		}
		return nil
	}
}

func writeManagedDevicePersistError(c businessContext, message string, err error) {
	status := http.StatusInternalServerError
	var bindingConflict *deviceBindingConflict
	var configConflict *deviceConfigConflictError
	if errors.As(err, &bindingConflict) || errors.As(err, &configConflict) {
		status = http.StatusConflict
	}
	logger.Error(message, "err", err)
	c.JSON(status, gin.H{"status": "error", "message": message + ": " + err.Error()})
}

func (s *Server) handleDeviceMgmtUpdateDevice(c *gin.Context) {
	s.useDeviceMgmtUpdateDevice(c)
}

func (s *Server) useDeviceMgmtUpdateDevice(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "必须填写 id"})
		return
	}
	var req updateDeviceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}
	if strings.TrimSpace(req.Config.ID) != "" && strings.TrimSpace(req.Config.ID) != id {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "不支持修改设备 ID"})
		return
	}
	mutationLeaseHeld := false
	if s.pool != nil {
		releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
		defer releaseMutation()
		mutationLeaseHeld = true
	}

	oldMD, err := config.GetDeviceByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "读取设备配置失败: " + err.Error()})
		return
	}
	if oldMD == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}

	req.Config.ID = id
	newCfg := deviceConfigFromDTOWithBase(req.Config, oldMD)
	newCfg, forcedWarning := normalizeManagedDeviceConfig(newCfg)
	if err := validateManagedNetworkConfig(newCfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
		return
	}
	oldCfg := *oldMD
	// 策略跟卡走：设备保存只负责硬件/身份字段，不再触碰策略（策略经 PUT /cards/:iccid/policy 独立编辑）。
	// DTO 仍会回传 network/vowifi/ip/apn，但 GET config 不投影这些字段（恒零），直接采信会把卡策略清空。
	// 故把当前有效策略同时写回 oldCfg 与 newCfg，使其在开关转换判断中互相抵消（中性化），
	// 不写 card_policies、不误触发 VoWiFi 关闭重建/恢复射频/热拉起。
	_, effNetwork, effVoWiFi, effIP, effAPN := s.currentEffectiveDevicePolicy(id)
	oldCfg.NetworkEnabled = effNetwork
	oldCfg.VoWiFiEnabled = effVoWiFi
	oldCfg.IPVersion = effIP
	oldCfg.APN = effAPN
	newCfg.NetworkEnabled = effNetwork
	newCfg.VoWiFiEnabled = effVoWiFi
	newCfg.IPVersion = effIP
	newCfg.APN = effAPN
	oldBackend := strings.ToLower(strings.TrimSpace(oldCfg.DeviceBackend))
	newBackend := strings.ToLower(strings.TrimSpace(newCfg.DeviceBackend))
	if oldBackend != newBackend {
		switchCtx, cancel := context.WithTimeout(businessRequestContext(c), backendSwitchRequestTimeout)
		defer cancel()
		result, switchErr := s.deviceBackendSwitcher().Switch(switchCtx, backendSwitchRequest{
			Current:           oldCfg,
			Desired:           newCfg,
			Target:            newBackend,
			MutationLeaseHeld: mutationLeaseHeld,
		})
		if switchErr != nil {
			respondBackendSwitchFailure(c, switchErr)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status":           "ok",
			"requires_restart": false,
			"backend_switch":   result,
		})
		return
	}

	requiresRestart := deviceConfigRequiresRestart(oldCfg, newCfg)
	if err := config.UpdateDeviceInFileValidated(s.configPath, config.DeviceUpdateRequest{
		DeviceID: newCfg.ID,
		Device:   newCfg,
		Validate: managedDeviceListValidator(newCfg, id, false),
	}); err != nil {
		writeManagedDevicePersistError(c, "写入设备配置失败", err)
		return
	}

	worker := s.pool.GetWorker(id)
	s.pool.UpdateWorkerConfig(id, newCfg, !requiresRestart)

	// 检测 DeviceBackend 状态变化，或 VoWiFi 从开启变为关闭都需要彻底重建 Worker 释放残余句柄
	needsRebuild := oldCfg.DeviceBackend != newCfg.DeviceBackend ||
		pcscRuntimeConfigChanged(oldCfg, newCfg) ||
		qmiProxyConfigChanged(oldCfg, newCfg) ||
		(!newCfg.VoWiFiEnabled && oldCfg.VoWiFiEnabled) ||
		(worker != nil && managedNetworkConfigChanged(oldCfg, newCfg))
	shouldApplyNetworkNow := worker != nil || needsRebuild

	warningMessage := forcedWarning
	if needsRebuild {
		logger.Info("配置保存触发底盘或 VoWiFi 停止变更，将彻底重建 Worker", "device", id)
		if err := s.pool.RebuildWorker(id); err != nil {
			logger.Error("重建 Worker 失败", "device", id, "err", err)
			warningMessage = joinWarningMessages(warningMessage, "配置已保存，但运行时重建失败: "+err.Error())
		}
	}

	shouldRestoreRadio := oldCfg.VoWiFiEnabled && !newCfg.VoWiFiEnabled
	if shouldRestoreRadio {
		if s.pool.IsVoWiFiActive(id) {
			warningMessage = joinWarningMessages(warningMessage, "VoWiFi 尚未完全退出，暂未恢复射频")
		} else if err := s.pool.RestoreRadioAfterVoWiFi(id); err != nil {
			logger.Warn("配置保存后恢复射频失败", "device", id, "err", err)
			warningMessage = joinWarningMessages(warningMessage, "配置已保存，但恢复射频失败: "+err.Error())
		}
	}

	// 检测 VoWiFi 状态变化，仅对于由关到开执行热拉起
	if newCfg.VoWiFiEnabled && !oldCfg.VoWiFiEnabled {
		logger.Info("配置保存触发 VoWiFi 启动", "device", id)
		if err := s.pool.EnableVoWiFi(id); err != nil {
			logger.Error("VoWiFi 启动失败", "device", id, "err", err)
			c.JSON(http.StatusOK, gin.H{
				"status":           "ok",
				"requires_restart": requiresRestart,
				"warning":          joinWarningMessages(warningMessage, "VoWiFi 启动失败: "+err.Error()),
				"vowifi_error":     "VoWiFi 启动失败: " + err.Error(),
			})
			return
		}
	}

	if shouldApplyNetworkNow && !newCfg.VoWiFiEnabled && !s.pool.IsVoWiFiActive(id) {
		if err := s.pool.ApplyConfiguredNetwork(id); err != nil {
			logger.Warn("配置保存后自动应用网络偏好失败", "device", id, "err", err)
			warningMessage = joinWarningMessages(warningMessage, "配置已保存，但自动应用网络失败: "+err.Error())
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"requires_restart": requiresRestart,
		"warning":          warningMessage,
	})
}

func (s *Server) handleDeviceMgmtDeleteDevice(c *gin.Context) {
	s.useDeviceMgmtDeleteDevice(c)
}

func (s *Server) useDeviceMgmtDeleteDevice(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "必须填写 id"})
		return
	}
	if s.pool != nil {
		releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
		defer releaseMutation()
	}
	deletedIMEI := managedDeviceIMEI(id, s.pool)
	var abandon func(string) error
	if s.pool != nil {
		abandon = s.pool.AbandonDevice
	}
	ops := databaseManagedDeviceDeleteOps(abandon)
	ops.detach = exactManagedDeviceDetach(deletedIMEI)
	ops.deleteConfig = s.deleteManagedDeviceConfig
	deleteManagedDevice(c, s.configPath, ops)
}

type addDeviceRequest struct {
	Config deviceConfigDTO `json:"config"`
}

// validateDeviceBackendConfig 校验 device_backend 配置合法值。
// 零路径持久化后 control_device 由运行时从 IMEI 发现，不再作为保存前置条件。
func validateDeviceBackendConfig(cfg config.DeviceConfig) error {
	backend := strings.ToLower(strings.TrimSpace(cfg.DeviceBackend))
	switch backend {
	case "", "at", "qmi", "mbim", "pcsc":
		// 合法值
	default:
		return fmt.Errorf("不支持的 device_backend: %q，可选值: at, qmi, mbim, pcsc", backend)
	}
	return nil
}

func validateFreeDeviceConfigLimit(devices []config.DeviceConfig) error {
	if device.FreeDeviceLimitReached(len(devices)) {
		return fmt.Errorf("%s", device.FreeDeviceAddLimitMessage())
	}
	return nil
}

func (s *Server) handleDeviceMgmtAddDevice(c *gin.Context) {
	s.useDeviceMgmtAddDevice(c)
}

func (s *Server) useDeviceMgmtAddDevice(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	var req addDeviceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}
	newCfg := deviceConfigFromDTO(req.Config)
	newCfg = deviceConfigForAdd(newCfg)
	newCfg, forcedWarning := normalizeManagedDeviceConfig(newCfg)
	if strings.TrimSpace(newCfg.ID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "必须填写 id"})
		return
	}
	if s.pool != nil {
		releaseMutation := s.pool.AcquireManagedDeviceMutation(newCfg.ID)
		defer releaseMutation()
	}
	if err := validateManagedNetworkConfig(newCfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
		return
	}

	if existing, err := config.GetDeviceByID(newCfg.ID); err == nil && existing != nil {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "设备 ID 已存在"})
		return
	}
	if s.pool != nil {
		if !stopManagedDevice(c, newCfg.ID, s.pool.AbandonDevice) {
			return
		}
	}
	// MBIM 设备使用 MBIM DeviceCaps 探测 IMEI，非 MBIM 设备使用 QMI 探测
	if strings.EqualFold(strings.TrimSpace(newCfg.DeviceBackend), pcsc.HardwareKind) {
		newCfg.ModemIMEI = ""
		newCfg.ESIMTransport = config.ESIMTransportPCSC
	} else if strings.ToLower(strings.TrimSpace(newCfg.DeviceBackend)) == "mbim" {
		if config.NormalizeIMEI(newCfg.ModemIMEI) == "" && strings.TrimSpace(newCfg.ControlDevice) != "" {
			if mbimIMEI, err := device.ProbeIMEIViaMBIM(newCfg.ControlDevice); err == nil && mbimIMEI != "" {
				newCfg.ModemIMEI = mbimIMEI
			}
		}
	} else {
		enrichedCfg, imeiErr := ensureAddDeviceIMEI(newCfg, probeIMEIForAddFn)
		if imeiErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": imeiErr.Error()})
			return
		}
		newCfg = enrichedCfg
	}

	if err := config.AddDeviceInFileValidated(
		s.configPath,
		newCfg,
		managedDeviceListValidator(newCfg, "", true),
	); err != nil {
		writeManagedDevicePersistError(c, "写入新设备配置失败", err)
		return
	}

	if _, err := s.pool.AddWorkerFromConfig(newCfg); err != nil {
		logger.Warn("设备配置已添加，但启动运行时设备失败", "device_id", newCfg.ID, "err", err)
		c.JSON(http.StatusOK, gin.H{
			"status":           "ok",
			"started":          false,
			"requires_restart": true,
			"warning":          "设备配置已添加，但运行时启动失败（可尝试重启服务或检查端口/权限）: " + err.Error(),
		})
		return
	}

	warningMessage := forcedWarning

	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"started":          true,
		"requires_restart": false,
		"warning":          warningMessage,
	})
}

type executeATRequest struct {
	Cmd       string `json:"cmd"`
	TimeoutMs int    `json:"timeout_ms"`
}

type manualATSession interface {
	Execute(cmd string, timeout time.Duration) (string, error)
	Close() error
}

var openManualATSession = func(port string) (manualATSession, error) {
	return modem.NewSerialAT(port, 115200, 8, 1, "N")
}

func (s *Server) handleDeviceMgmtExecuteAT(c *gin.Context) {
	s.useDeviceMgmtExecuteAT(c)
}

func (s *Server) useDeviceMgmtExecuteAT(c businessContext) {
	id := deviceIDParam(c)
	var req executeATRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}
	cmd := strings.TrimSpace(req.Cmd)
	if cmd == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "cmd 不能为空"})
		return
	}
	if len(cmd) > 512 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "cmd 过长"})
		return
	}

	worker := s.pool.GetWorker(id)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到或未运行"})
		return
	}

	timeout := 10 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}

	resp, err := s.pool.ExecuteATContext(businessRequestContext(c), device.ATRequest{DeviceID: id, Command: cmd, Timeout: timeout})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, device.ErrATUnavailable) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"status": "error", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "response": resp})
}

type setUSBNetModeRequest struct {
	Mode int `json:"mode"`
}

func (s *Server) handleDeviceMgmtSetUSBNetMode(c *gin.Context) {
	s.useDeviceMgmtSetUSBNetMode(c)
}

func (s *Server) useDeviceMgmtSetUSBNetMode(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "必须填写 id"})
		return
	}
	var req setUSBNetModeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}

	target := ""
	switch req.Mode {
	case 0:
		target = "qmi"
	case 2:
		target = "mbim"
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"message": "仅支持 USBNET 0(QMI) 与 2(MBIM)，其他模式不会自动猜测",
		})
		return
	}
	mutationLeaseHeld := false
	if s.pool != nil {
		releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
		defer releaseMutation()
		mutationLeaseHeld = true
	}
	current, err := config.GetDeviceByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "读取设备配置失败: " + err.Error()})
		return
	}
	if current == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}
	desired := *current
	desired.DeviceBackend = target
	switchCtx, cancel := context.WithTimeout(businessRequestContext(c), backendSwitchRequestTimeout)
	defer cancel()
	result, switchErr := s.deviceBackendSwitcher().Switch(switchCtx, backendSwitchRequest{
		Current:           *current,
		Desired:           desired,
		Target:            target,
		MutationLeaseHeld: mutationLeaseHeld,
	})
	if switchErr != nil {
		respondBackendSwitchFailure(c, switchErr)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "backend_switch": result})
}

// handleEsimListProfiles 获取 eSIM Profile 列表
func (s *Server) handleEsimListProfiles(c *gin.Context) {
	s.useEsimListProfiles(c)
}

func (s *Server) useEsimListProfiles(c businessContext) {
	id := deviceIDParam(c)
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	refresh := c.Query("refresh") == "true"
	if refresh {
		if err := worker.EsimMgr.RefreshProfiles(); err != nil {
			if isEsimBusyError(err) {
				respondEsimBusy(c, "refresh_profiles", err)
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	profiles, err := worker.EsimMgr.GetProfiles()
	if err != nil {
		if isEsimBusyError(err) {
			respondEsimBusy(c, "list_profiles", err)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, profiles)
}

// esimSwitchRequest 包含切换的目标 ICCID
type esimSwitchRequest struct {
	ICCID  string `json:"iccid" binding:"required"`
	AIDHex string `json:"aid_hex"` // 可选，前端已知时直接传，跳过遍历
}

type esimDisableOperation struct {
	Context context.Context
	ICCID   string
	AIDHex  string
}

type esimRetryableConflict struct {
	Reason string
	Code   string
	Err    error
}

type esimSwitchResponse struct {
	Message            string `json:"message"`
	TargetICCID        string `json:"target_iccid"`
	SwitchToken        uint64 `json:"switch_token"`
	SwitchPhase        string `json:"switch_phase"`
	SwitchAccepted     bool   `json:"switch_accepted"`
	RecoveryPending    bool   `json:"recovery_pending"`
	DegradedReason     string `json:"degraded_reason,omitempty"`
	PostSwitchAsync    bool   `json:"post_switch_async"`
	CachePatched       bool   `json:"cache_patched"`
	SIMReloadAttempted bool   `json:"sim_reload_attempted"`
	SIMReloadOK        bool   `json:"sim_reload_ok"`
	SIMReloadWarning   string `json:"sim_reload_warning,omitempty"`
}

const (
	esimBusyRetryAfterMs     = 1200
	esimProfileActionTimeout = 30 * time.Second
)

func isEsimBusyError(err error) bool {
	return errors.Is(err, esim.ErrOperationInProgress) ||
		errors.Is(err, apduarbiter.ErrAPDUBusy) ||
		esim.IsDisableProfileBusy(err)
}

func esimDisableHTTPStatus(err error) int {
	switch esim.ClassifyDisableProfileError(err) {
	case esim.DisableProfileErrorInvalidICCID, esim.DisableProfileErrorInvalidAIDHex:
		return http.StatusBadRequest
	case esim.DisableProfileErrorProfileNotFound:
		return http.StatusNotFound
	case esim.DisableProfileErrorProfileNotEnabled,
		esim.DisableProfileErrorDisallowedByPolicy,
		esim.DisableProfileErrorCATBusy:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func esimSwitchHTTPStatus(err error) int {
	switch esim.ClassifySwitchProfileError(err) {
	case esim.SwitchProfileErrorInvalidICCID, esim.SwitchProfileErrorInvalidAIDHex:
		return http.StatusBadRequest
	case esim.SwitchProfileErrorProfileNotFound:
		return http.StatusNotFound
	case esim.SwitchProfileErrorProfileNotDisabled,
		esim.SwitchProfileErrorPolicyRejected,
		esim.SwitchProfileErrorWrongReenable,
		esim.SwitchProfileErrorCATBusy:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func esimDeleteHTTPStatus(err error) int {
	switch {
	case isEsimBusyError(err) || esim.IsDeleteProfileConflict(err):
		return http.StatusConflict
	case esim.IsDeleteProfileInvalidInput(err):
		return http.StatusBadRequest
	case esim.IsDeleteProfileNotFound(err):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func respondEsimBusy(c businessContext, reason string, err error) {
	respondEsimRetryableConflict(c, esimRetryableConflict{
		Reason: reason,
		Code:   "ESIM_BUSY",
		Err:    err,
	})
}

func respondEsimRetryableConflict(c businessContext, conflict esimRetryableConflict) {
	retryAfterSec := (esimBusyRetryAfterMs + 999) / 1000
	c.Header("Retry-After", strconv.Itoa(retryAfterSec))
	c.JSON(http.StatusConflict, gin.H{
		"error":        conflict.Err.Error(),
		"busy":         true,
		"code":         conflict.Code,
		"reason":       conflict.Reason,
		"retryAfterMs": esimBusyRetryAfterMs,
	})
}

func esimDeleteSuccessBody(result esim.DeleteProfileResult) gin.H {
	body := gin.H{
		"status":  "ok",
		"message": "Profile 删除成功",
	}
	if warning := strings.TrimSpace(result.Warning); warning != "" {
		body["warning"] = warning
	}
	if warningCode := strings.TrimSpace(result.WarningCode); warningCode != "" {
		body["warning_code"] = warningCode
	}
	if result.SpaceDelta != nil {
		body["space_delta"] = result.SpaceDelta
	}
	return body
}

func formatEsimDownloadDoneEvent(result esim.DownloadProfileResult) string {
	base := `{"step":"done","msg":"Profile 下载完成","pct":100`
	if result.SpaceDelta != nil {
		spaceDeltaJSON, err := json.Marshal(result.SpaceDelta)
		if err == nil {
			base += fmt.Sprintf(`,"space_delta":%s`, spaceDeltaJSON)
		}
	}
	if warning := strings.TrimSpace(result.Warning); warning != "" {
		base += fmt.Sprintf(`,"warning":%q`, warning)
	}
	if warningCode := strings.TrimSpace(result.WarningCode); warningCode != "" {
		base += fmt.Sprintf(`,"warning_code":%q`, warningCode)
	}
	return base + `}`
}

func formatEsimDownloadErrorEvent(err error) string {
	msg := "下载失败"
	var downloadErr *esim.DownloadProfileError
	if errors.As(err, &downloadErr) && downloadErr != nil {
		if downloadErr.Message != "" {
			msg += ": " + downloadErr.Message
		} else if err != nil {
			msg += ": " + err.Error()
		}
		base := fmt.Sprintf(`{"step":"error","msg":%q,"pct":-1`, msg)
		if code := strings.TrimSpace(downloadErr.Code); code != "" {
			base += fmt.Sprintf(`,"code":%q`, code)
		}
		if details := strings.TrimSpace(downloadErr.Details); details != "" {
			base += fmt.Sprintf(`,"details":%q`, details)
		}
		return base + `}`
	}
	if err != nil {
		msg += ": " + err.Error()
	}
	return fmt.Sprintf(`{"step":"error","msg":%q,"pct":-1}`, msg)
}

func writeEsimDownloadDoneEvent(c businessContext, result esim.DownloadProfileResult) {
	businessProgress(c, formatEsimDownloadDoneEvent(result))
}
func writeEsimDownloadErrorEvent(c businessContext, err error) {
	businessProgress(c, formatEsimDownloadErrorEvent(err))
}

func writeEsimDeleteSuccessJSON(c businessContext, result esim.DeleteProfileResult) {
	c.JSON(http.StatusOK, esimDeleteSuccessBody(result))
}

func esimDownloadExec(run func(context.Context, string, string, string, string, string, esim.DownloadProgressFn) (esim.DownloadProfileResult, error), ctx context.Context, aidHex, smdp, matchingID, confirmationCode, imei string, progressFn esim.DownloadProgressFn) (esim.DownloadProfileResult, error) {
	return run(ctx, aidHex, smdp, matchingID, confirmationCode, imei, progressFn)
}

var esimNotificationListExec = func(run func(string) ([]esim.NotificationItem, error), aidHex string) ([]esim.NotificationItem, error) {
	return run(aidHex)
}

var esimNotificationRetryExec = func(run func(int64, string) error, sequence int64, aidHex string) error {
	return run(sequence, aidHex)
}

var esimDisableExec = func(run func(context.Context, string, string) error, operation esimDisableOperation) error {
	return run(operation.Context, operation.ICCID, operation.AIDHex)
}

func esimDeleteExec(run func(string, string) (esim.DeleteProfileResult, error), iccid, aidHex string) (esim.DeleteProfileResult, error) {
	return run(iccid, aidHex)
}

func esimNotificationHTTPStatus(err error) int {
	switch esim.ClassifyNotificationError(err) {
	case esim.NotificationErrorBusy:
		return http.StatusConflict
	case esim.NotificationErrorInvalidSequence, esim.NotificationErrorInvalidAIDHex:
		return http.StatusBadRequest
	case esim.NotificationErrorNotFound:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) handleEsimListNotifications(c *gin.Context) {
	s.useEsimListNotifications(c)
}

func (s *Server) useEsimListNotifications(c businessContext) {
	id := deviceIDParam(c)
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}
	items, err := esimNotificationListExec(worker.EsimMgr.ListNotifications, strings.TrimSpace(c.Query("aid_hex")))
	if err != nil {
		if esimNotificationHTTPStatus(err) == http.StatusConflict {
			respondEsimBusy(c, "list_notifications", err)
			return
		}
		c.JSON(esimNotificationHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (s *Server) handleEsimRetryNotification(c *gin.Context) {
	s.useEsimRetryNotification(c)
}

func (s *Server) useEsimRetryNotification(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	sequence, err := strconv.ParseInt(strings.TrimSpace(c.Param("sequence")), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的通知序号"})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()
	if s.pool.IsESIMSwitching(id) {
		c.JSON(http.StatusConflict, gin.H{"error": "设备正在切换 SIM，暂不能重试通知"})
		return
	}
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}
	err = esimNotificationRetryExec(worker.EsimMgr.RetryNotification, sequence, strings.TrimSpace(c.Query("aid_hex")))
	if err != nil {
		if esimNotificationHTTPStatus(err) == http.StatusConflict {
			respondEsimBusy(c, "retry_notification", err)
			return
		}
		c.JSON(esimNotificationHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "通知重试发送成功"})
}

// handleEsimSwitchProfile 切换 eSIM Profile
func (s *Server) handleEsimSwitchProfile(c *gin.Context) {
	s.useEsimSwitchProfile(c)
}

func (s *Server) useEsimSwitchProfile(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	var req esimSwitchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()

	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	// Profile 切换：EnableProfile 后等待目标 profile 生效；切卡后按 Ready+Delay 门控执行后处理（不等待搜网）
	ctx, cancel := context.WithTimeout(context.Background(), esimProfileActionTimeout)
	defer cancel()

	result, err := worker.EsimMgr.SwitchProfileWithResult(ctx, req.ICCID, req.AIDHex)
	if err != nil {
		if isEsimBusyError(err) {
			respondEsimBusy(c, "switch_profile", err)
			return
		}
		if esim.ClassifySwitchProfileError(err) == esim.SwitchProfileErrorCATBusy {
			respondEsimRetryableConflict(c, esimRetryableConflict{
				Reason: "switch_profile",
				Code:   string(esim.SwitchProfileErrorCATBusy),
				Err:    err,
			})
			return
		}
		c.JSON(esimSwitchHTTPStatus(err), gin.H{
			"error": "esim配置切换失败: " + err.Error(),
			"code":  esim.ClassifySwitchProfileError(err),
		})
		return
	}

	c.JSON(http.StatusOK, esimSwitchResponse{
		Message:            "eSIM Profile 切换指令已提交，设备信息将异步刷新",
		TargetICCID:        result.TargetICCID,
		SwitchToken:        result.SwitchToken,
		SwitchPhase:        string(result.Phase),
		SwitchAccepted:     result.SwitchAccepted,
		RecoveryPending:    result.RecoveryPending,
		DegradedReason:     result.DegradedReason,
		PostSwitchAsync:    result.PostSwitchAsync,
		CachePatched:       result.CachePatched,
		SIMReloadAttempted: result.PowerCycleAttempt,
		SIMReloadOK:        result.PowerCycleAttempt && result.SIMReloadWarning == "",
		SIMReloadWarning:   result.SIMReloadWarning,
	})

}

// handleEsimDisableProfile 停用当前 eSIM Profile，但不启用其他 Profile。
func (s *Server) handleEsimDisableProfile(c *gin.Context) {
	s.useEsimDisableProfile(c)
}

func (s *Server) useEsimDisableProfile(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	var req esimSwitchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.ICCID = strings.TrimSpace(req.ICCID)
	req.AIDHex = strings.TrimSpace(req.AIDHex)
	if req.ICCID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "iccid 不能为空"})
		return
	}
	id := deviceIDParam(c)
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()

	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	ctx, cancel := context.WithTimeout(businessRequestContext(c), esimProfileActionTimeout)
	defer cancel()
	err := esimDisableExec(worker.EsimMgr.DisableProfile, esimDisableOperation{
		Context: ctx,
		ICCID:   req.ICCID,
		AIDHex:  req.AIDHex,
	})
	if err != nil {
		if isEsimBusyError(err) {
			respondEsimBusy(c, "disable_profile", err)
			return
		}
		if esim.ClassifyDisableProfileError(err) == esim.DisableProfileErrorCATBusy {
			respondEsimRetryableConflict(c, esimRetryableConflict{
				Reason: "disable_profile",
				Code:   string(esim.DisableProfileErrorCATBusy),
				Err:    err,
			})
			return
		}
		c.JSON(esimDisableHTTPStatus(err), gin.H{
			"error": "esim profile 停用失败: " + err.Error(),
			"code":  esim.ClassifyDisableProfileError(err),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": "eSIM Profile 停用指令已提交，设备信息将异步刷新",
	})
}

// handleEsimGetEID 获取所有 eUICC 的 EID 列表
func (s *Server) handleEsimGetEID(c *gin.Context) {
	s.useEsimGetEID(c)
}

func (s *Server) useEsimGetEID(c businessContext) {
	id := deviceIDParam(c)
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	eids, err := worker.EsimMgr.GetEIDs()
	if err != nil {
		if isEsimBusyError(err) {
			respondEsimBusy(c, "get_eids", err)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"eids": eids})
}

// handleEsimGetChipInfo 获取 eUICC 芯片硬件信息（名称、序列号、固件版本、可用空间）
func (s *Server) handleEsimGetChipInfo(c *gin.Context) {
	s.useEsimGetChipInfo(c)
}

func (s *Server) useEsimGetChipInfo(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	forceRefresh := c.Query("refresh") == "true"
	chipInfo, err := worker.EsimMgr.GetEUICCChipInfo(forceRefresh)
	if err != nil {
		if isEsimBusyError(err) {
			respondEsimBusy(c, "get_chip_info", err)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, chipInfo)
}

// handleEsimGetOverview 获取 eSIM 总览（合并芯片信息和 profiles）
func (s *Server) handleEsimGetOverview(c *gin.Context) {
	s.useEsimGetOverview(c)
}

func (s *Server) useEsimGetOverview(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	refresh := c.Query("refresh") == "true"
	if refresh {
		if err := worker.EsimMgr.RefreshOverview(); err != nil {
			if isEsimBusyError(err) {
				respondEsimBusy(c, "refresh_overview", err)
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	overview, err := worker.EsimMgr.GetEsimOverview()
	if err != nil {
		if isEsimBusyError(err) {
			respondEsimBusy(c, "get_overview", err)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, overview)
}

// handleEsimDownloadProfile 下载 eSIM profile（SSE 流式进度推送）
//
// 请求方式：GET，通过 Query 参数传递下载参数：
//
//	smdp=<SM-DP+ 地址或 LPA:1$ 激活码>
//	activation_code=<二维码/LPA 激活码>（可选；能解析时优先，失败则回退 smdp）
//	matching_id=<Matching ID>（可选）
//	confirmation_code=<确认码>（可选）
//	aid_hex=<目标 AID hex>（可选）
//	imei=<下载使用的 IMEI>（可选）
//
// 响应为 text/event-stream，每条事件 data 为 JSON：
//
//	{"step":"preflight","msg":"正在检查 eUICC 剩余空间...","pct":10}
//	{"step":"auth_client","msg":"...","pct":30}
//	{"step":"auth_server","msg":"...","pct":60}
//	{"step":"install","msg":"...","pct":80}
//	{"step":"notify","msg":"...","pct":90}
//	{"step":"done","msg":"Profile 下载完成","pct":100}
//	{"step":"error","msg":"<错误信息>","pct":-1}
func (s *Server) handleEsimDownloadProfile(c *gin.Context) {
	s.useEsimDownloadProfile(c)
}

func (s *Server) useEsimDownloadProfile(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	smdp, matchingID, err := esim.ResolveDownloadQuery(c.Query("activation_code"), c.Query("smdp"), c.Query("matching_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	confirmationCode := c.Query("confirmation_code")
	aidHex := c.Query("aid_hex")
	imei := strings.TrimSpace(c.Query("imei"))

	businessStreamHeaders(c)
	// 下载可能耗时较长，给 5 分钟超时
	ctx, cancel := context.WithTimeout(businessRequestContext(c), 5*time.Minute)
	defer cancel()

	progressFn := func(event esim.DownloadProgressEvent) {
		payload, marshalErr := json.Marshal(map[string]any{"step": event.Step, "msg": event.Msg, "pct": event.Pct})
		if marshalErr != nil {
			return
		}
		businessProgress(c, string(payload))
	}

	result, err := esimDownloadExec(worker.EsimMgr.DownloadProfile, ctx, aidHex, smdp, matchingID, confirmationCode, imei, progressFn)
	if err != nil {
		writeEsimDownloadErrorEvent(c, err)
		return
	}
	writeEsimDownloadDoneEvent(c, result)
}

// handleEsimRenameProfile 修改 eSIM profile 名称
func (s *Server) handleEsimRenameProfile(c *gin.Context) {
	s.useEsimRenameProfile(c)
}

func (s *Server) useEsimRenameProfile(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	iccid := c.Param("iccid")
	var req struct {
		Name   string `json:"name"`
		AIDHex string `json:"aid_hex"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 为必填项"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.AIDHex = strings.TrimSpace(req.AIDHex)
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 为必填项"})
		return
	}
	if len([]byte(req.Name)) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "profile 名称不能超过 64 字节"})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()
	if s.pool.IsESIMSwitching(id) {
		c.JSON(http.StatusConflict, gin.H{"error": "设备正在切换 SIM，暂不能修改 Profile 名称"})
		return
	}
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	if err := worker.EsimMgr.RenameProfile(iccid, req.Name, req.AIDHex); err != nil {
		if isEsimBusyError(err) {
			respondEsimBusy(c, "rename_profile", err)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "修改名称失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Profile 名称修改成功"})
}

// handleEsimDeleteProfile 删除 eSIM profile
func (s *Server) handleEsimDeleteProfile(c *gin.Context) {
	s.useEsimDeleteProfile(c)
}

func (s *Server) useEsimDeleteProfile(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	iccid := c.Param("iccid")
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()
	worker := s.pool.GetWorker(id)
	if worker == nil || worker.EsimMgr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备或esim管理器未找到"})
		return
	}

	if iccid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "iccid 为必填项"})
		return
	}

	aidHex := strings.TrimSpace(c.Query("aid_hex"))

	result, err := esimDeleteExec(worker.EsimMgr.DeleteProfile, iccid, aidHex)
	if err != nil {
		if isEsimBusyError(err) || esim.IsDeleteProfileBusy(err) {
			respondEsimBusy(c, "delete_profile", err)
			return
		}
		c.JSON(esimDeleteHTTPStatus(err), gin.H{
			"error": "删除 profile 失败: " + err.Error(),
			"code":  esim.ClassifyDeleteProfileError(err),
		})
		return
	}

	writeEsimDeleteSuccessJSON(c, result)
}

type executeUSSDRequest struct {
	Command   string `json:"command" binding:"required"`
	TimeoutMs int    `json:"timeout_ms"`
}

// handleDeviceMgmtExecuteUSSD 执行 USSD 指令
// 路由策略：VoWiFi 在线时优先使用 VoWiFi 通道，否则回退到 CS 域
func (s *Server) handleDeviceMgmtExecuteUSSD(c *gin.Context) {
	s.useDeviceMgmtExecuteUSSD(c)
}

func (s *Server) useDeviceMgmtExecuteUSSD(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	var req executeUSSDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误: " + err.Error()})
		return
	}

	cmd := strings.TrimSpace(req.Command)
	if cmd == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "command 不能为空"})
		return
	}

	worker := s.pool.GetWorker(id)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}

	timeout := 45 * time.Second // 默认 45 秒（与 CS 域 USSD 一致）
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}

	// VoWiFi 在线时优先走 VoWiFi
	if s.pool.IsVoWiFiActive(id) {
		ctx, cancel := context.WithTimeout(businessRequestContext(c), timeout)
		defer cancel()
		resp, err := s.pool.SendVoWiFiUSSD(ctx, id, cmd)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error(), "channel": "vowifi"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "result": resp, "channel": "vowifi"})
		return
	}

	// 回退到 CS 域 USSD
	provider, ok := worker.Backend.(backend.USSDProvider)
	if !ok || provider == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "当前设备后端不支持 USSD"})
		return
	}
	resp, err := provider.ExecuteUSSD(businessRequestContext(c), cmd, timeout)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error(), "channel": "cs"})
		return
	}
	markCSUSSDSession(resp)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "result": resp, "channel": "cs"})
}

type continueUSSDRequest struct {
	SessionID string `json:"session_id" binding:"required"`
	Input     string `json:"input" binding:"required"`
	TimeoutMs int    `json:"timeout_ms"`
}

// handleDeviceMgmtContinueUSSD 发送 USSD 后续输入（多轮菜单选择）
func (s *Server) handleDeviceMgmtContinueUSSD(c *gin.Context) {
	s.useDeviceMgmtContinueUSSD(c)
}

func (s *Server) useDeviceMgmtContinueUSSD(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	var req continueUSSDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误: " + err.Error()})
		return
	}

	input := strings.TrimSpace(req.Input)
	if input == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "input 不能为空"})
		return
	}

	timeout := 45 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}

	if !s.pool.IsVoWiFiActive(id) {
		worker := s.pool.GetWorker(id)
		if worker == nil {
			c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
			return
		}
		provider, ok := worker.Backend.(backend.USSDContinueProvider)
		if !ok || provider == nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "当前设备后端不支持多轮 USSD"})
			return
		}
		resp, err := provider.ContinueUSSD(businessRequestContext(c), input, timeout)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error(), "channel": "cs"})
			return
		}
		markCSUSSDSession(resp)
		c.JSON(http.StatusOK, gin.H{"status": "ok", "result": resp, "channel": "cs"})
		return
	}

	ctx, cancel := context.WithTimeout(businessRequestContext(c), timeout)
	defer cancel()
	resp, err := s.pool.ContinueVoWiFiUSSD(ctx, id, req.SessionID, input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "result": resp, "channel": "vowifi"})
}

type cancelUSSDRequest struct {
	SessionID string `json:"session_id"`
}

// handleDeviceMgmtCancelUSSD 取消活跃的 USSD 会话
func (s *Server) handleDeviceMgmtCancelUSSD(c *gin.Context) {
	s.useDeviceMgmtCancelUSSD(c)
}

func (s *Server) useDeviceMgmtCancelUSSD(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	var req cancelUSSDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// session_id 是可选的
		req.SessionID = ""
	}

	if !s.pool.IsVoWiFiActive(id) {
		worker := s.pool.GetWorker(id)
		if worker == nil {
			c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
			return
		}
		provider, ok := worker.Backend.(backend.USSDProvider)
		if !ok || provider == nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "当前设备后端不支持 USSD 取消"})
			return
		}
		if err := provider.CancelUSSD(businessRequestContext(c)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "USSD 会话已取消", "channel": "cs"})
		return
	}

	if err := s.pool.CancelVoWiFiUSSD(businessRequestContext(c), id, req.SessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "USSD 会话已取消"})
}

func markCSUSSDSession(resp *backend.USSDResult) {
	if resp != nil && resp.Status == 1 {
		resp.SessionID = "cs"
	}
}

// handleDeviceMgmtReboot 执行模组重启 (发送 AT+CFUN=1,1)
func (s *Server) handleDeviceMgmtSetFlightMode(c *gin.Context) {
	s.useDeviceMgmtSetFlightMode(c)
}

func (s *Server) useDeviceMgmtSetFlightMode(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}

	if s.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "服务未就绪"})
		return
	}

	var req setFlightModeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()

	worker := s.pool.GetWorker(id)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到或未运行"})
		return
	}
	if s.pool.IsESIMSwitching(id) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "设备正在切卡，请稍后再切换飞行模式"})
		return
	}
	if !req.Enabled {
		class, classifyErr := s.classifyLebaraUKForDevice(businessRequestContext(c), id)
		if rejectLebaraUKRFUnlock(c, class, classifyErr) {
			return
		}
	}

	if s.pool.IsVoWiFiActive(id) && !device.PhoneModeCampsOnCell(worker.Config.PhoneMode) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "WiFi calling 正在接管射频，请先关掉软件电话或改成蜂窝/VoLTE"})
		return
	}

	flightModeEnabled := req.Enabled

	// 卡策略持久化是硬件动作的前置条件，避免数据库与真实射频状态悄悄分叉。
	// 蜂窝软件电话与飞行可以并存：只关射频和流量，电话策略保持。
	if _, _, err := s.patchCardPolicyForDevice(id, func(p *db.CardPolicy) {
		applyAirplaneToCardPolicy(p, flightModeEnabled)
	}); err != nil {
		writeCardPolicyMutationError(c, err)
		return
	}
	// 同步 w.Config，使概览即时反映飞行/在线模式（setWorkerFlightMode 只切硬件不碰 Config）。
	s.pool.SetWorkerAirplanePolicy(id, flightModeEnabled)
	if flightModeEnabled && worker.Config.PhoneMode == "cellular" && s.pool.IsVoWiFiActive(id) {
		// 先停 IMS 再关射频；RestoreRadioAfterVoWiFi 看到 airplane 后不会把射频拉回 Online。
		_ = s.pool.StopVoWiFiRuntimeForCellularIdle(id)
	}

	operatingMode, flightMode, err := setWorkerFlightMode(businessRequestContext(c), worker, flightModeEnabled)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "切换飞行模式失败: " + err.Error()})
		return
	}
	if err := s.pool.ApplyCurrentCardPolicy(id, "flight_mode_change"); err != nil {
		logger.Debug("飞行切换后重投影卡策略失败", "device", id, "err", err)
	}
	s.scheduleFlightModeRuntimeRefresh(worker, !flightModeEnabled)

	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"message":        flightModeSuccessMessage(flightModeEnabled),
		"operating_mode": operatingMode,
		"flight_mode":    flightMode,
	})
}

// shouldUseATFirstReboot 判断重启时是否应优先尝试 AT+CFUN=1,1。
// QMI 模式设备直接走 QMI ModeReset（backend.Reboot）；AT 优先路径仅保留给 AT 模式设备，
// 原先"QMI 模式也优先走 AT"是为了规避部分模组 QMI ModeReset 假死的历史问题，
// 现已实测确认本机型号 QMI ModeReset 正常工作，因此 QMI 模式不再绕道 AT。
func shouldUseATFirstReboot(backendMode string) bool {
	return backendMode != backend.BackendQMI
}

// handleDeviceMgmtReboot 执行模组重启 (QMI 模式走 QMI ModeReset，AT 模式走 AT+CFUN=1,1)
func (s *Server) handleDeviceMgmtReboot(c *gin.Context) {
	s.useDeviceMgmtReboot(c)
}

func (s *Server) useDeviceMgmtReboot(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "参数错误"})
		return
	}
	if s == nil || s.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "服务未就绪"})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()

	worker := s.pool.GetWorker(id)
	if worker == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备未找到"})
		return
	}

	if err := validateRebootWorkerIdentity(businessRequestContext(c), worker); err != nil {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": err.Error()})
		return
	}

	rebootSent := false

	useATFirst := worker.Backend == nil || shouldUseATFirstReboot(worker.Backend.Mode())

	// AT 模式设备优先尝试使用 AT 端口软重启；QMI 模式设备直接走 QMI ModeReset（见下方 fallback）
	if useATFirst && worker.Modem != nil && worker.Modem.HasATPort() && worker.Modem.CanExecuteAT() {
		_, err := worker.Modem.ExecuteAT("AT+CFUN=1,1", 20*time.Second)
		if err == nil {
			rebootSent = true
		} else {
			// 如果发送后立刻断开，可能会报错，视同成功发送
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "timeout") || strings.Contains(msg, "eof") || strings.Contains(msg, "closed") || strings.Contains(msg, "no such file") {
				rebootSent = true
			}
		}
	}

	// QMI 模式设备的主路径；AT 模式设备在 AT 端口不可用/发送失败时的降级路径
	if !rebootSent && worker.Backend != nil {
		if err := worker.Backend.Reboot(businessRequestContext(c)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "重启指令失败: " + err.Error()})
			return
		}
		rebootSent = true
	}

	if !rebootSent {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "无法发送重启指令，无可用通道"})
		return
	}

	if !s.pool.ScheduleModemRebootRecoveryForWorker(worker, "manual_reboot") {
		c.JSON(http.StatusConflict, gin.H{
			"status": "error", "message": "重启指令已发送，但设备实例已更换，未启动旧设备恢复任务",
		})
		return
	}
	s.pool.MarkLifecycleRecovery(id, device.LifecyclePhaseRebooting, "manual_reboot", 3*time.Minute)

	// 因为重启后设备会脱网并暂时下线，前端仅需知道命令已送达
	c.JSON(http.StatusOK, gin.H{"status": "ok", "response": "重启指令已发送"})
}

func validateRebootWorkerIdentity(ctx context.Context, worker *device.Worker) error {
	if worker == nil || worker.Backend == nil {
		return nil
	}
	expectedIMEI := strings.TrimSpace(worker.Config.ModemIMEI)
	if expectedIMEI == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	currentIMEI, err := worker.Backend.GetIMEI(probeCtx)
	if err != nil || strings.TrimSpace(currentIMEI) == "" {
		return nil
	}
	currentIMEI = strings.TrimSpace(currentIMEI)
	if !config.IMEIMatches(currentIMEI, expectedIMEI) {
		return fmt.Errorf("设备路径已漂移：当前控制面 IMEI=%s，不匹配配置 IMEI=%s，请先重新扫描/重新绑定后再重启", currentIMEI, expectedIMEI)
	}
	return nil
}

// handleDeviceMgmtReconnectVoWiFi 执行重连 VoWiFi 的操作
func (s *Server) handleDeviceMgmtReconnectVoWiFi(c *gin.Context) {
	s.useDeviceMgmtReconnectVoWiFi(c)
}

func (s *Server) useDeviceMgmtReconnectVoWiFi(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	id := deviceIDParam(c)
	if id == "" || s == nil || s.pool == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "设备参数无效"})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(id)
	defer releaseMutation()
	if s.pool.IsESIMSwitching(id) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "设备正在切换 SIM，暂不能重连 VoWiFi"})
		return
	}

	// 验证设备存在（硬件/传输配置仍在 config.yaml）
	md, err := config.GetDeviceByID(id)
	if err != nil || md == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "设备配置不存在"})
		return
	}

	// VoWiFi 开关已跟卡走、只存在于运行时投影。门禁读 worker 的有效策略；
	// 无 worker 时跳过友好门禁，交由 RestartVoWiFi 报告底层错误。
	if worker := s.pool.GetWorker(id); worker != nil && !worker.Config.VoWiFiEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "设备未开启 VoWiFi，无法重连"})
		return
	}

	if err := s.pool.RestartVoWiFi(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "VoWiFi 重连失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "已触发 VoWiFi 重连"})
}

// handleDeviceMgmtOverviewStreamSingle 给前端管理的概览信息提供带有动态刷新的 SSE 推流（仅针对选中的单个设备）
func (s *Server) handleDeviceMgmtOverviewStreamSingle(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	deviceID := deviceIDParam(c)
	if deviceID == "" {
		return
	}

	worker := s.pool.GetWorker(deviceID)
	if worker != nil {
		worker.IncStreamSub()
		defer worker.DecStreamSub()
	}

	notify := c.Writer.CloseNotify()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// 订阅 VoWiFi 运行态变更——状态一变（如 IMS 注册成功）立即推送，无需等待 Ticker。
	// 若 VoWiFi 未启动则 stateCh 为 nil，nil channel 在 select 中永远阻塞，行为安全。
	stateCh, unsubState := s.pool.SubscribeVoWiFiState(deviceID)
	defer unsubState()
	trafficStream := overviewTrafficStreamState{
		subscriber: s.trafficRT,
		deviceID:   deviceID,
		ctx:        c.Request.Context(),
	}
	defer trafficStream.stop()
	var trafficCh <-chan proxytraffic.RealtimeSnapshot

	var (
		cachedCfg *config.DeviceConfig
		lastSent  *overviewStreamEmitVersion
	)
	getConfig := func(refresh bool) *config.DeviceConfig {
		if !refresh && cachedCfg != nil {
			return cachedCfg
		}
		md, _ := config.GetDeviceByID(deviceID)
		cachedCfg = md
		return md
	}

	sendData := func(refreshConfig bool, fromStateEvent bool, stateSnapshot *runtimehost.State) {
		md := getConfig(refreshConfig)
		if md == nil {
			return
		}
		var item deviceMgmtOverviewLiteItem
		var status modem.DeviceStatus

		w := s.pool.GetWorker(deviceID)
		if w != nil {
			status = w.GetCachedDeviceStatus()
			trueVal := true
			// 用运行时投影(w.Config)合并展示，使策略字段反映跟卡走的有效值
			item = s.buildOverviewLiteDetailItemFromWorker(w, overviewDisplayConfig(w.Config, *md, true), status, &trueVal)
			if overviewRealtimeTrafficEnabled(item) {
				tag := w.ID + "@" + md.Interface
				ps, rx, tx, _ := db.GetLatestMinuteDeltas("iface", tag)
				item.Traffic, item.TrafficRaw, item.TrafficMeta = buildTrafficOverviewFields(md.Interface, db.LatestMinuteDeltas{
					PeriodStart: ps,
					RxBytes:     rx,
					TxBytes:     tx,
				}, time.Now())
			}
		} else {
			trueVal := true
			pol := resolveOfflineDevicePolicy(deviceID)
			item = deviceMgmtOverviewLiteItem{
				ID:                     md.ID,
				Name:                   md.Name,
				Running:                false,
				Healthy:                false,
				ControlOnline:          false,
				PublicIP:               "",
				Interface:              md.Interface,
				ControlDevice:          md.ControlDevice,
				ESIMTransport:          config.NormalizeESIMTransport(md.ESIMTransport),
				ATPort:                 md.ATPort,
				AudioDevice:            md.AudioDevice,
				SMSEnabled:             pol.SMSEnabled,
				NetworkEnabled:         pol.NetworkEnabled,
				VoWiFiEnabled:          pol.VoWiFiEnabled,
				VoWiFiActive:           false,
				VoWiFiHealth:           s.getWiFiCallingHealth(deviceID),
				NetworkConnected:       false,
				RegistrationStateLabel: registrationStateLabel(0),
				RadioLiveOK:            &trueVal,
				Modem:                  offlineModemStatus(deviceID),
				Traffic:                nil,
				BackendMode:            resolveOfflineBackendMode(*md),
			}
			s.applyLifecycleToOverviewLiteItem(&item, nil, *md)
		}
		if stateSnapshot != nil {
			item.VoWiFiRuntime = s.attachVoWiFiMWI(runtimeStateSnapshotToDTO(*stateSnapshot, status), deviceID)
		}

		trafficCh = trafficStream.sync(item)
		curr := newOverviewStreamEmitVersion(item)
		if fromStateEvent && shouldSkipOverviewStatePush(lastSent, curr) {
			return
		}
		lastSent = &curr
		if fromStateEvent {
			phase := ""
			readiness := voWiFiRuntimeDTO{}
			if item.VoWiFiRuntime != nil {
				phase = item.VoWiFiRuntime.Phase
				readiness = *item.VoWiFiRuntime
			}
			fields := []any{
				"device", deviceID,
				"phase", phase,
				"sim_ready", readiness.SIMReady,
				"access_ready", readiness.AccessReady,
				"tunnel_ready", readiness.TunnelReady,
				"ims_ready", readiness.IMSReady,
				"sms_ready", readiness.SMSReady,
				"sms_mo_ready", readiness.SMSMOReady,
				"last_reason", readiness.LastReason,
				"last_error", readiness.LastError,
				"last_error_class", readiness.LastErrorClass,
			}
			switch phase {
			case "interrupted", "error", "retrying":
				logger.Info("overview SSE 推送 VoWiFi 状态变更", fields...)
			default:
				logger.Debug("overview SSE 推送 VoWiFi 状态变更", fields...)
			}
		}

		// 仍然使用 devices 结构体包裹返回单项从而无缝对接前台旧结构
		c.SSEvent("overview", gin.H{"devices": []deviceMgmtOverviewLiteItem{item}})
		c.Writer.Flush()
	}

	sendData(true, false, nil)

	for {
		select {
		case <-notify:
			return
		case <-c.Request.Context().Done():
			return
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			sendData(true, false, nil)
		case state := <-stateCh: // 按事件快照推送，避免连续状态被合并成最终态。
			sendData(false, true, &state)
		case snap, ok := <-trafficCh:
			if !ok {
				trafficStream.stop()
				trafficCh = nil
				continue
			}
			c.SSEvent("traffic", snap)
			c.Writer.Flush()
		}
	}
}

func offlineModemStatus(deviceID string) modem.DeviceStatus {
	return modem.DeviceStatus{ICCID: db.CurrentICCIDForDevice(deviceID)}
}

type overviewTrafficStreamState struct {
	subscriber  realtimeTrafficSubscriber
	deviceID    string
	ctx         context.Context
	ch          <-chan proxytraffic.RealtimeSnapshot
	unsubscribe func()
}

func (s *overviewTrafficStreamState) sync(item deviceMgmtOverviewLiteItem) <-chan proxytraffic.RealtimeSnapshot {
	if s == nil || s.subscriber == nil || s.deviceID == "" {
		return nil
	}
	if !overviewRealtimeTrafficEnabled(item) {
		s.stop()
		return nil
	}
	if s.ch != nil {
		return s.ch
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	s.ch, s.unsubscribe = s.subscriber.Subscribe(ctx, s.deviceID)
	return s.ch
}

func (s *overviewTrafficStreamState) stop() {
	if s == nil {
		return
	}
	if s.unsubscribe != nil {
		s.unsubscribe()
	}
	s.ch = nil
	s.unsubscribe = nil
}

func overviewRealtimeTrafficEnabled(item deviceMgmtOverviewLiteItem) bool {
	return item.NetworkEnabled && item.NetworkConnected
}

func resolveOfflineBackendMode(cfg config.DeviceConfig) string {
	m := strings.ToLower(strings.TrimSpace(cfg.DeviceBackend))
	if m == "" && strings.TrimSpace(cfg.ControlDevice) != "" {
		return "qmi"
	}
	if m == "qmi" {
		return "qmi"
	}
	return "at"
}
