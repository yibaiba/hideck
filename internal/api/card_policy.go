package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/pkg/logger"
)

var errCardPolicyIdentityUnavailable = errors.New("SIM 身份未就绪，无法保存卡策略")

const cardPolicySavedRestartFailedCode = "card_policy_saved_restart_failed"
const cardPolicySavedMWIClearFailedCode = "card_policy_saved_mwi_clear_failed"

type cardPolicyStore interface {
	Get(string) (db.CardPolicy, error)
	Resolve(string) (db.CardPolicy, error)
	Upsert(db.CardPolicy) error
	ClearMWIRejections(string) (bool, error)
}

type cardPolicyVoWiFiRestarter interface {
	RestartVoWiFiForICCID(string) error
}

type databaseCardPolicyStore struct{}

func (databaseCardPolicyStore) Get(iccid string) (db.CardPolicy, error) {
	return db.GetCardPolicy(iccid)
}

func (databaseCardPolicyStore) Resolve(iccid string) (db.CardPolicy, error) {
	return db.ResolveCardPolicy(iccid)
}

func (databaseCardPolicyStore) Upsert(policy db.CardPolicy) error {
	return db.UpsertCardPolicy(policy)
}

func (databaseCardPolicyStore) ClearMWIRejections(iccid string) (bool, error) {
	return db.DeleteIMSMWIRejections(iccid)
}

func (s *Server) cardPolicyStore() cardPolicyStore {
	if s.cardPolicies != nil {
		return s.cardPolicies
	}
	return databaseCardPolicyStore{}
}

// patchCardPolicyForDevice 解析设备当前 ICCID，对 card_policies 行执行原地修改并落库。
// mutate 在 resolve 后的副本上改字段（source 会被强制为 "user"）。
func (s *Server) patchCardPolicyForDevice(deviceID string, mutate func(*db.CardPolicy)) (iccid string, applied bool, err error) {
	worker := s.pool.GetWorker(deviceID)
	if worker == nil {
		return "", false, fmt.Errorf("设备未找到")
	}
	iccid = worker.CurrentICCID()
	if iccid == "" {
		return "", false, errCardPolicyIdentityUnavailable
	}
	p, err := s.cardPolicyStore().Resolve(iccid)
	if err != nil {
		return iccid, false, fmt.Errorf("获取卡策略失败: %w", err)
	}
	mutate(&p)
	p.Source = "user"
	db.NormalizeCardPolicy(&p)
	if err := s.cardPolicyStore().Upsert(p); err != nil {
		return iccid, false, fmt.Errorf("保存卡策略失败: %w", err)
	}
	return iccid, true, nil
}

func (s *Server) handleGetCardPolicy(c *gin.Context) {
	s.useGetCardPolicy(c)
}

func (s *Server) useGetCardPolicy(c businessContext) {
	iccid := c.Param("iccid")
	pol, err := s.cardPolicyStore().Get(iccid)
	if errors.Is(err, db.ErrCardPolicyNotFound) {
		// 未建档则返回默认模板（不落库，读端点保持只读语义）
		c.JSON(http.StatusOK, db.DefaultCardPolicy(iccid))
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, pol)
}

func (s *Server) handleListCardPolicies(c *gin.Context) {
	s.useListCardPolicies(c)
}

func (s *Server) useListCardPolicies(c businessContext) {
	var out []db.CardPolicy
	if db.DB != nil {
		if err := db.DB.Order("updated_at desc").Find(&out).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"policies": out})
}

type cardPolicyPatchRequest struct {
	NetworkEnabled        *bool   `json:"network_enabled"`
	VoWiFiEnabled         *bool   `json:"vowifi_enabled"`
	AirplaneEnabled       *bool   `json:"airplane_enabled"`
	IPVersion             *string `json:"ip_version"`
	APN                   *string `json:"apn"`
	PhoneMode             *string `json:"phone_mode"`
	DataStrategy          *string `json:"data_strategy"`
	VowifiUpstreamProxyID *string `json:"vowifi_upstream_proxy_id"`
	LocalPhoneOverride    *string `json:"local_phone_override"`
	MWIMode               *string `json:"mwi_mode"`
	RedetectMWI           bool    `json:"redetect_mwi"`
}

func (s *Server) handlePutCardPolicy(c *gin.Context) {
	s.usePutCardPolicy(c)
}

func (s *Server) usePutCardPolicy(c businessContext) {
	releaseOperation, allowed := s.acquireBusinessMutation(c)
	if !allowed {
		return
	}
	defer releaseOperation()

	iccid := c.Param("iccid")
	var req cardPolicyPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	pol, err := s.cardPolicyStore().Get(iccid)
	if errors.Is(err, db.ErrCardPolicyNotFound) {
		pol = db.DefaultCardPolicy(iccid)
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取卡策略失败: " + err.Error()})
		return
	}
	previousProxyID := pol.VowifiUpstreamProxyID

	class, classifyErr := s.classifyLebaraUKForICCID(businessRequestContext(c), iccid)
	if classifyErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "识别 Lebara UK 射频策略失败: " + classifyErr.Error()})
		return
	}
	if class.IsLebara {
		unlocksRadio := req.NetworkEnabled != nil && *req.NetworkEnabled
		if req.AirplaneEnabled != nil && !*req.AirplaneEnabled {
			unlocksRadio = true
		}
		if device.PhoneModeCampsOnCell(normalizePhoneMode(req.PhoneMode)) {
			unlocksRadio = true
		}
		if unlocksRadio {
			c.JSON(http.StatusConflict, gin.H{
				"error":   device.ErrLebaraUKRFLocked.Error(),
				"rf_lock": class.RFLock(),
			})
			return
		}
	}

	if req.NetworkEnabled != nil {
		pol.NetworkEnabled = *req.NetworkEnabled
	}
	if req.VoWiFiEnabled != nil {
		pol.VoWiFiEnabled = *req.VoWiFiEnabled
	}
	if req.AirplaneEnabled != nil {
		pol.AirplaneEnabled = *req.AirplaneEnabled
	}
	if req.IPVersion != nil {
		normalized, err := normalizeCardPolicyIPVersion(*req.IPVersion)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		pol.IPVersion = normalized
	}
	if req.APN != nil {
		pol.APN = strings.TrimSpace(*req.APN)
	}
	if req.PhoneMode != nil {
		pol.PhoneMode = normalizePhoneMode(req.PhoneMode)
	}
	if req.DataStrategy != nil {
		pol.DataStrategy = normalizeDataStrategy(req.DataStrategy)
	}
	if req.VowifiUpstreamProxyID != nil {
		id, err := normalizeCardVoWiFiUpstreamProxyID(*req.VowifiUpstreamProxyID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		pol.VowifiUpstreamProxyID = id
	}
	if req.LocalPhoneOverride != nil {
		phone, err := db.NormalizeManualPhoneNumber(*req.LocalPhoneOverride)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		pol.LocalPhoneOverride = phone
	}
	if req.MWIMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*req.MWIMode))
		if db.NormalizeMWIMode(mode) != mode {
			c.JSON(http.StatusBadRequest, gin.H{"error": "mwi_mode 只能是 auto、on 或 off"})
			return
		}
		pol.MWIMode = mode
	}
	pol.Source = "user"

	if err := s.cardPolicyStore().Upsert(pol); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Choosing "on" re-detects MWI support by clearing the records left by an
	// explicit network rejection (405/489). The running registration still holds
	// that rejection in memory, so reconnect WiFi calling when a record was cleared.
	// Only identities stored in ims_mwi_rejections are known here; a message-summary
	// 489 left in the generic table before the upgrade expires with its registration.
	redetectMWI := false
	if req.MWIMode != nil && pol.MWIMode == db.MWIModeOn {
		cleared, err := s.cardPolicyStore().ClearMWIRejections(pol.ICCID)
		if err != nil {
			logger.Warn("卡策略已保存，但清除 MWI 拒绝记录失败", "iccid", pol.ICCID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":  "error",
				"code":    cardPolicySavedMWIClearFailedCode,
				"message": "卡策略已保存，但清除语音信箱不支持记录失败: " + err.Error(),
				"policy":  pol,
			})
			return
		}
		// The redetect button always reconnects: if the reconnect after an earlier
		// clear failed, the record is already gone and cleared alone would never retry.
		redetectMWI = (cleared || req.RedetectMWI) && pol.VoWiFiEnabled
		if cleared {
			logger.Info("已清除 MWI 拒绝记录，重新检测语音信箱提示", "iccid", pol.ICCID, "reconnect", redetectMWI)
		}
	}
	proxyChanged := req.VowifiUpstreamProxyID != nil && previousProxyID != pol.VowifiUpstreamProxyID && pol.VoWiFiEnabled

	if proxyChanged || redetectMWI {
		if err := s.restartVoWiFiForCardPolicy(pol.ICCID); err != nil {
			logger.Warn("卡策略已保存，但 WiFi calling 重连失败",
				"iccid", pol.ICCID,
				"upstream_proxy_id", pol.VowifiUpstreamProxyID,
				"proxy_changed", proxyChanged, "redetect_mwi", redetectMWI,
				"err", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":  "error",
				"code":    cardPolicySavedRestartFailedCode,
				"message": "卡策略已保存，但 WiFi calling 重连失败: " + err.Error(),
				"policy":  pol,
			})
			return
		}
	}

	c.JSON(http.StatusOK, pol)
}

func (s *Server) restartVoWiFiForCardPolicy(iccid string) error {
	if s.cardPolicyRestarter != nil {
		return s.cardPolicyRestarter.RestartVoWiFiForICCID(iccid)
	}
	if s.pool == nil {
		return errors.New("设备池未初始化")
	}
	return s.pool.RestartVoWiFiForICCID(iccid)
}

func normalizeCardVoWiFiUpstreamProxyID(value string) (string, error) {
	id := db.NormalizeVoWiFiUpstreamProxyID(value)
	if id == "" || id == db.VoWiFiUpstreamProxyDirect {
		return id, nil
	}
	proxy, err := db.GetUpstreamProxyByID(id)
	if err != nil {
		return "", err
	}
	if proxy == nil {
		return "", fmt.Errorf("前置代理 %s 不存在", id)
	}
	if !proxy.Enabled {
		return "", fmt.Errorf("前置代理 %s 已禁用", id)
	}
	return proxy.ID, nil
}

func normalizeCardPolicyIPVersion(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	v4, v6, err := config.ResolveIPFamily(value)
	if err != nil {
		return "", err
	}
	switch {
	case v4 && v6:
		return "v4v6", nil
	case v6:
		return "v6", nil
	default:
		return "v4", nil
	}
}

func writeCardPolicyMutationError(c businessContext, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, errCardPolicyIdentityUnavailable) {
		status = http.StatusConflict
	}
	c.JSON(status, gin.H{"status": "error", "message": err.Error()})
}
