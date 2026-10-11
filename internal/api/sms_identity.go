package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
)

type smsDeviceInfo struct {
	id   string
	name string
}

// resolveSMSICCID 将 device_id 或 imsi 查询参数解析为唯一 ICCID。
func (s *Server) resolveSMSICCID(deviceID, imsi string) (string, int, string) {
	deviceID = strings.TrimSpace(deviceID)
	imsi = strings.TrimSpace(imsi)
	if deviceID == "" || deviceID == "all" {
		if imsi == "" {
			return "", http.StatusBadRequest, "缺少 imsi 参数（device_id=all 时必须指定）"
		}
		iccid, err := db.ResolveICCIDForIMSI(imsi)
		if err != nil {
			status, message := smsIdentityHTTPError(err)
			return "", status, message
		}
		return iccid, 0, ""
	}
	if s.pool == nil {
		return "", http.StatusServiceUnavailable, "设备服务未就绪"
	}
	iccid, err := s.pool.ResolveSMSICCID(deviceID)
	if err != nil {
		status, message := smsIdentityHTTPError(err)
		return "", status, message
	}
	return iccid, 0, ""
}

type smsWorkerMatch struct {
	matches         func(*device.Worker) bool
	notFoundMessage string
	conflictMessage string
}

func (s *Server) resolveSMSSendWorker(deviceID, imsi, iccid string) (*device.Worker, int, string) {
	deviceID = strings.TrimSpace(deviceID)
	imsi = strings.TrimSpace(imsi)
	iccidProvided := strings.TrimSpace(iccid) != ""
	iccid = db.CanonicalICCID(iccid)
	if iccidProvided && iccid == "" {
		return nil, http.StatusBadRequest, "iccid 参数无效"
	}
	if countSMSSelectors(deviceID, imsi, iccid) > 1 {
		return nil, http.StatusBadRequest, "device_id、imsi 和 iccid 只能指定一个"
	}
	if s.pool == nil {
		return nil, http.StatusServiceUnavailable, "设备服务未就绪"
	}
	if deviceID != "" {
		worker := s.pool.GetWorker(deviceID)
		if worker == nil {
			return nil, http.StatusNotFound, "设备未找到: " + deviceID
		}
		return worker, 0, ""
	}
	if imsi != "" {
		return uniqueSMSWorker(s.pool.GetAllWorkers(), smsWorkerMatch{
			matches:         func(worker *device.Worker) bool { return worker.GetIMSI() == imsi },
			notFoundMessage: "未找到匹配 IMSI 的设备: " + imsi,
			conflictMessage: "多个在线设备使用同一 IMSI，请改用 iccid",
		})
	}
	if iccid != "" {
		return s.resolveSMSWorkerByICCID(iccid)
	}
	return uniqueSMSWorker(s.pool.GetAllWorkers(), smsWorkerMatch{
		matches:         func(*device.Worker) bool { return true },
		notFoundMessage: "暂无可用设备",
		conflictMessage: "存在多个设备时必须指定 device_id、imsi 或 iccid",
	})
}

func (s *Server) resolveSMSWorkerByICCID(iccid string) (*device.Worker, int, string) {
	var matched *device.Worker
	for _, worker := range s.pool.GetAllWorkers() {
		if worker == nil {
			continue
		}
		resolved, err := s.pool.ResolveSMSICCID(worker.ID)
		if err != nil {
			if smsWorkerMayOwnICCID(worker, iccid) {
				status, message := smsIdentityHTTPError(err)
				return nil, status, message
			}
			continue
		}
		if db.CanonicalICCID(resolved) != iccid {
			continue
		}
		if matched != nil {
			return nil, http.StatusConflict, "多个在线设备使用同一 ICCID"
		}
		matched = worker
	}
	if matched == nil {
		return nil, http.StatusNotFound, "未找到匹配 ICCID 的在线设备: " + iccid
	}
	return matched, 0, ""
}

func smsWorkerMayOwnICCID(worker *device.Worker, iccid string) bool {
	status := worker.ProjectDeviceStatus()
	if db.CanonicalICCID(status.ICCID) == iccid {
		return true
	}
	if backend.NormalizeBackendMode(worker.Config.DeviceBackend) == backend.BackendPCSC || (worker.Backend != nil && worker.Backend.Mode() == backend.BackendPCSC) {
		status.IMEI = ""
	}
	stored, found, err := db.LookupSMSIdentityForDevice(db.SMSDeviceSelector{
		DeviceID: worker.ID, IMEI: status.IMEI,
	})
	// An ambiguous historical alias cannot prove ownership of the requested card.
	return err == nil && found && db.CanonicalICCID(stored.ICCID) == iccid
}

func countSMSSelectors(deviceID, imsi, iccid string) int {
	count := 0
	for _, value := range []string{deviceID, imsi, iccid} {
		if value != "" {
			count++
		}
	}
	return count
}

func uniqueSMSWorker(workers []*device.Worker, match smsWorkerMatch) (*device.Worker, int, string) {
	var matched *device.Worker
	for _, worker := range workers {
		if worker == nil || !match.matches(worker) {
			continue
		}
		if matched != nil {
			return nil, http.StatusConflict, match.conflictMessage
		}
		matched = worker
	}
	if matched == nil {
		return nil, http.StatusNotFound, match.notFoundMessage
	}
	return matched, 0, ""
}

func smsIdentityHTTPError(err error) (int, string) {
	switch {
	case errors.Is(err, device.ErrSMSIdentityConflict), errors.Is(err, db.ErrSMSIdentityConflict):
		return http.StatusConflict, "设备短信身份冲突: " + err.Error()
	case errors.Is(err, device.ErrSMSIdentityTransitioning):
		return http.StatusConflict, "设备 SIM 身份正在切换，请稍后重试"
	case errors.Is(err, device.ErrSMSIdentityUnknown), errors.Is(err, db.ErrSMSIdentityUnknown):
		return http.StatusBadRequest, "该设备短信身份未就绪"
	default:
		return http.StatusInternalServerError, "解析设备短信身份失败: " + err.Error()
	}
}

func (s *Server) smsDeviceInfoByICCID() map[string]smsDeviceInfo {
	result := make(map[string]smsDeviceInfo)
	if s.pool == nil {
		return result
	}
	configured := config.ListDevices()
	seen := make(map[string]struct{}, len(configured))
	for _, cfg := range configured {
		seen[cfg.ID] = struct{}{}
		s.addSMSDeviceInfo(result, cfg.ID, cfg.Name)
	}
	for _, worker := range s.pool.GetAllWorkers() {
		if worker == nil {
			continue
		}
		if _, ok := seen[worker.ID]; ok {
			continue
		}
		s.addSMSDeviceInfo(result, worker.ID, worker.Config.Name)
	}
	return result
}

func (s *Server) addSMSDeviceInfo(result map[string]smsDeviceInfo, deviceID, name string) {
	iccid, err := s.pool.ResolveSMSICCID(deviceID)
	if err != nil || iccid == "" {
		return
	}
	if strings.TrimSpace(name) == "" {
		name = deviceID
	}
	result[iccid] = smsDeviceInfo{id: deviceID, name: name}
}
