package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/pkg/logger"
)

type enabledPatchRequest struct {
	Enabled      *bool   `json:"enabled"`
	Mode         *string `json:"mode"`          // "wifi" | "cellular" | "volte"
	DataStrategy *string `json:"data_strategy"` // "always" | "on_demand"
}

type networkPatchRequest struct {
	Enabled   *bool   `json:"enabled"`
	IPVersion *string `json:"ip_version"`
	APN       *string `json:"apn"`
}

func (s *Server) handleDeviceNetworkPatch(c *gin.Context) {
	s.useDeviceNetworkPatch(c)
}

func (s *Server) useDeviceNetworkPatch(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	var req networkPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "enabled 为必填项"})
		return
	}

	deviceID := deviceIDParam(c)
	if deviceID == "" || s == nil || s.pool == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "设备参数无效"})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(deviceID)
	defer releaseMutation()
	if s.pool.IsESIMSwitching(deviceID) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "设备正在切换 SIM，暂不能修改网络"})
		return
	}

	if *req.Enabled {
		class, classifyErr := s.classifyLebaraUKForDevice(businessRequestContext(c), deviceID)
		if rejectLebaraUKRFUnlock(c, class, classifyErr) {
			return
		}
		ipVersion, err := normalizedOptionalIPVersion(req.IPVersion)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
			return
		}
		apn := trimmedOptionalString(req.APN)
		effectiveIPVersion := ""
		effectiveAPN := ""
		_, _, err = s.patchCardPolicyForDevice(deviceID, func(p *db.CardPolicy) {
			applyNetworkEnableToCardPolicy(p)
			if ipVersion != "" {
				p.IPVersion = ipVersion
			}
			if req.APN != nil {
				p.APN = apn
			}
			effectiveIPVersion = p.IPVersion
			effectiveAPN = p.APN
		})
		if err != nil {
			writeCardPolicyMutationError(c, err)
			return
		}
		s.pool.SetWorkerNetworkPolicy(deviceID, true, effectiveIPVersion, effectiveAPN)
		s.useDeviceMgmtStartNetworkLocked(c, deviceID)
		return
	}

	if _, _, err := s.patchCardPolicyForDevice(deviceID, func(p *db.CardPolicy) {
		p.NetworkEnabled = false
	}); err != nil {
		writeCardPolicyMutationError(c, err)
		return
	}
	s.pool.SetWorkerNetworkPolicy(deviceID, false, "", "")
	if err := s.pool.ApplyCurrentCardPolicy(deviceID, "network_disabled"); err != nil {
		logger.Debug("关闭网络后重投影卡策略失败", "device", deviceID, "err", err)
	}
	s.useDeviceMgmtStopNetworkLocked(c, deviceID)
}

func (s *Server) handleDeviceVoWiFiPatch(c *gin.Context) {
	s.useDeviceVoWiFiPatch(c)
}

func (s *Server) useDeviceVoWiFiPatch(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	var req enabledPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "enabled 为必填项"})
		return
	}

	deviceID := deviceIDParam(c)
	if deviceID == "" || s == nil || s.pool == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "设备参数无效"})
		return
	}
	releaseMutation := s.pool.AcquireManagedDeviceMutation(deviceID)
	defer releaseMutation()
	if s.pool.IsESIMSwitching(deviceID) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "设备正在切换 SIM，暂不能修改通话模式"})
		return
	}

	if *req.Enabled {
		w := s.pool.GetWorker(deviceID)
		var mode, strategy string
		if _, _, err := s.patchCardPolicyForDeviceChecked(deviceID, func(p *db.CardPolicy) error {
			db.NormalizeCardPolicy(p)
			mode = p.PhoneMode
			if req.Mode != nil {
				mode = normalizePhoneMode(req.Mode)
			}
			softwareIMSBlocked := device.WorkerSoftwareIMSBlocked(w)
			if softwareIMSBlocked && !device.IsModemVoiceMode(mode) {
				mode = device.PhoneModeVoLTE
			}
			var rfPolicyErr error
			keepSavedMode := false
			if device.PhoneModeCampsOnCell(mode) {
				class, err := s.classifyLebaraUKForDevice(businessRequestContext(c), deviceID)
				switch {
				case err != nil:
					rfPolicyErr = fmt.Errorf("识别 Lebara UK 射频策略失败: %w", err)
				case class.IsLebara && req.Mode == nil && !softwareIMSBlocked:
					// Lebara projects the saved mode to WiFi while its RF lock is active.
					mode = device.PhoneModeWiFi
					keepSavedMode = true
				case class.IsLebara:
					rfPolicyErr = device.ErrLebaraUKRFLocked
				}
			}
			if err := s.checkModemVoiceModeChange(deviceID, mode); err != nil {
				return err
			}
			if err := w.CheckPhoneModeSupported(mode); err != nil {
				return err
			}
			if device.IsNativeVoLTEMode(mode) && !s.pool.HasQMIVoice(deviceID) {
				return &device.PhoneModeUnsupportedError{Reason: "该设备没有 QMI 语音接口，无法启用 VoLTE，请改用模组直拨"}
			}
			if rfPolicyErr != nil {
				return rfPolicyErr
			}
			strategy = p.DataStrategy
			if req.DataStrategy != nil {
				strategy = normalizeDataStrategy(req.DataStrategy)
			}
			if !keepSavedMode {
				p.PhoneMode = mode
			}
			p.DataStrategy = strategy
			applyVoWiFiEnableToCardPolicy(p, mode)
			return nil
		}); err != nil {
			writeCardPolicyMutationError(c, err)
			return
		}
		// 同步 w.Config，使概览即时切到 VoWiFi 模式面板（EnableVoWiFi 不碰 Config）。
		w = s.pool.GetWorker(deviceID)
		prevMode, prevStrategy := "", ""
		if w != nil {
			prevMode = w.Config.PhoneMode
			if prevMode == "" {
				prevMode = "wifi"
			}
			prevStrategy = w.Config.DataStrategy
			if prevStrategy == "" {
				prevStrategy = "on_demand"
			}
			w.Config.PhoneMode = mode
			w.Config.DataStrategy = strategy
		}
		s.pool.SetWorkerVoWiFiPolicy(deviceID, true)
		if w := s.pool.GetWorker(deviceID); w != nil && device.IsModemVoiceMode(w.Config.PhoneMode) {
			if err := s.pool.ApplyCurrentCardPolicy(deviceID, "api_enable_modem_voice"); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "正在准备模组直拨音频", "device": deviceID})
			return
		}
		if w := s.pool.GetWorker(deviceID); w != nil && device.IsNativeVoLTEMode(w.Config.PhoneMode) {
			if err := s.pool.ApplyCurrentCardPolicy(deviceID, "api_enable_native_volte"); err != nil {
				logger.Warn("VoLTE 投影射频失败，仍尝试启用原生 IMS", "device", deviceID, "err", err)
				s.pool.ScheduleNativeVoLTE(deviceID, "api_enable_native_volte_fallback")
			}
			c.JSON(http.StatusOK, gin.H{
				"status":  "ok",
				"message": "VoLTE 已设置，会驻网并由模组原生 IMS 打电话。打开「网络」才会走上网流量",
				"device":  deviceID,
			})
			return
		}
		if w := s.pool.GetWorker(deviceID); w != nil && w.Config.PhoneMode == "cellular" && w.Config.DataStrategy != "always" {
			if err := s.pool.StopVoWiFiRuntimeForCellularIdle(deviceID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"status":  "error",
					"message": "切换蜂窝模式失败: " + err.Error(),
					"device":  deviceID,
				})
				return
			}
			message := "蜂窝模式已设置，会正常驻网。打开「网络」后才会走流量"
			if w.Config.NetworkEnabled {
				message = "蜂窝模式已设置，仅打电话时开：拨号才连数据"
			}
			c.JSON(http.StatusOK, gin.H{
				"status":  "ok",
				"message": message,
				"device":  deviceID,
			})
			return
		}
		modeChanged := mode != prevMode
		strategyChanged := strategy != prevStrategy
		if s.pool.IsVoWiFiActive(deviceID) && (modeChanged || strategyChanged) {
			if err := s.pool.RestartVoWiFi(deviceID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"status":  "error",
					"message": "切换通话模式失败: " + err.Error(),
					"device":  deviceID,
				})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"status":  "ok",
				"message": "通话模式已切换",
				"device":  deviceID,
			})
			return
		}
		s.useVoWiFiEnableLocked(c, deviceID)
		return
	}

	// 落库：仅清 vowifi_enabled=false，保留 airplane_enabled（用户飞行意图）。
	if err := s.checkModemVoiceModeChange(deviceID, ""); err != nil {
		writeCardPolicyMutationError(c, err)
		return
	}
	// 关闭 VoWiFi 后 DisableVoWiFi 会按当前卡策略重投影：之前是飞行则回飞行，否则回在线。
	if _, _, err := s.patchCardPolicyForDevice(deviceID, vowifiDisablePolicyMutation); err != nil {
		writeCardPolicyMutationError(c, err)
		return
	}
	s.pool.SetWorkerVoWiFiPolicy(deviceID, false)
	s.useVoWiFiDisableLocked(c, deviceID)
}

// vowifiEnablePolicyMutation 开 VoWiFi 的落库副作用：只置 vowifi，飞行意图保持不变。
func vowifiEnablePolicyMutation(p *db.CardPolicy) { p.VoWiFiEnabled = true }

// vowifiDisablePolicyMutation 关 VoWiFi 的落库副作用：只清 vowifi，保留用户飞行意图以便回退。
func vowifiDisablePolicyMutation(p *db.CardPolicy) { p.VoWiFiEnabled = false }

// applyAirplaneToCardPolicy 把飞行/驻网写成一组互斥结果：开飞行必关流量；
// WiFi calling 占用射频，开飞行就关掉它；蜂窝软件电话可以保持开启。
func applyAirplaneToCardPolicy(p *db.CardPolicy, enabled bool) {
	if p == nil {
		return
	}
	p.AirplaneEnabled = enabled
	if !enabled {
		// 长时间开启 = 驻网时保持流量；退出飞行后把网络开关写回，避免恢复路径偷开 PDP。
		if p.PhoneMode == "cellular" && p.DataStrategy == "always" {
			p.NetworkEnabled = true
		}
		return
	}
	p.NetworkEnabled = false
	if !device.PhoneModeCampsOnCell(p.PhoneMode) {
		p.VoWiFiEnabled = false
	}
}

// applyNetworkEnableToCardPolicy 开流量必须先驻网；WiFi calling 与数据互斥。
func applyNetworkEnableToCardPolicy(p *db.CardPolicy) {
	if p == nil {
		return
	}
	p.NetworkEnabled = true
	p.AirplaneEnabled = false
	if !device.PhoneModeCampsOnCell(p.PhoneMode) {
		p.VoWiFiEnabled = false
	}
}

// applyVoWiFiEnableToCardPolicy 按通话方式联动射频和流量。
func applyVoWiFiEnableToCardPolicy(p *db.CardPolicy, mode string) {
	if p == nil {
		return
	}
	p.VoWiFiEnabled = true
	if mode == device.PhoneModeCellular {
		p.AirplaneEnabled = false
		if p.DataStrategy == "always" {
			p.NetworkEnabled = true
		}
		return
	}
	if device.UsesModemPhoneControl(mode) {
		p.AirplaneEnabled = false
		return
	}
	p.AirplaneEnabled = true
	p.NetworkEnabled = false
}

func normalizePhoneMode(v *string) string {
	if v == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(*v)) {
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

func normalizeDataStrategy(v *string) string {
	if v == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(*v)) {
	case "always":
		return "always"
	default:
		return "on_demand"
	}
}

func normalizedOptionalIPVersion(value *string) (string, error) {
	if value == nil {
		return "", nil
	}
	return normalizeCardPolicyIPVersion(*value)
}

func trimmedOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
