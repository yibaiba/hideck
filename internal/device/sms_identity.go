package device

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
)

var (
	ErrSMSIdentityUnknown       = errors.New("短信身份未知")
	ErrSMSIdentityConflict      = errors.New("短信身份冲突")
	ErrSMSIdentityTransitioning = errors.New("SIM 身份正在切换")
)

type SMSIdentity struct {
	ICCID string
	IMSI  string
}

type inboundSMSRecord struct {
	Sender    string
	Content   string
	Timestamp time.Time
}

type smsIdentityCandidate struct {
	identity SMSIdentity
	present  bool
	imei     string
}

type smsIdentityStore interface {
	LookupDeviceIdentity(selector db.SMSDeviceSelector) (SMSIdentity, bool, error)
	SaveReceived(identity SMSIdentity, message inboundSMSRecord) error
}

type databaseSMSIdentityStore struct{}

func (databaseSMSIdentityStore) LookupDeviceIdentity(selector db.SMSDeviceSelector) (SMSIdentity, bool, error) {
	identity, found, err := db.LookupSMSIdentityForDevice(selector)
	return SMSIdentity{ICCID: identity.ICCID, IMSI: identity.IMSI}, found, err
}

func (databaseSMSIdentityStore) SaveReceived(identity SMSIdentity, message inboundSMSRecord) error {
	return db.SaveSMSForIdentity(db.SMSRecord{
		Identity: db.SMSIdentity{ICCID: identity.ICCID, IMSI: identity.IMSI},
		Sender:   message.Sender, Content: message.Content,
		Type: 1, Status: 0, Timestamp: message.Timestamp,
	})
}

func (p *Pool) ResolveSMSIdentity(deviceID string) (SMSIdentity, error) {
	return p.resolveSMSIdentity(deviceID, true)
}

func (p *Pool) ResolveSMSICCID(deviceID string) (string, error) {
	identity, err := p.resolveSMSIdentity(deviceID, false)
	return identity.ICCID, err
}

func (p *Pool) resolveSMSIdentity(deviceID string, requireIMSI bool) (SMSIdentity, error) {
	deviceID = strings.TrimSpace(deviceID)
	if p == nil || deviceID == "" {
		return SMSIdentity{}, ErrSMSIdentityUnknown
	}
	runtime, err := p.runtimeSMSIdentity(deviceID)
	if err != nil {
		return SMSIdentity{}, err
	}
	storedIdentity, storedPresent, err := p.smsIdentityRepository().LookupDeviceIdentity(db.SMSDeviceSelector{
		DeviceID: deviceID, IMEI: runtime.imei,
	})
	if err != nil {
		return SMSIdentity{}, fmt.Errorf("读取设备短信身份失败: %w", err)
	}
	identity, err := mergeSMSIdentities(
		runtime,
		smsIdentityCandidate{identity: storedIdentity, present: storedPresent},
	)
	if err != nil {
		return SMSIdentity{}, err
	}
	if identity.ICCID == "" || (requireIMSI && identity.IMSI == "") {
		return SMSIdentity{}, ErrSMSIdentityUnknown
	}
	return identity, nil
}

func (p *Pool) runtimeSMSIdentity(deviceID string) (smsIdentityCandidate, error) {
	worker := p.GetWorker(deviceID)
	if worker == nil {
		cfg, err := config.GetDeviceByID(deviceID)
		if err != nil {
			return smsIdentityCandidate{}, err
		}
		if cfg != nil && resolvedBackendMode(*cfg) != backend.BackendPCSC {
			return smsIdentityCandidate{imei: strings.TrimSpace(cfg.ModemIMEI)}, nil
		}
		return smsIdentityCandidate{}, nil
	}
	worker.cacheMu.RLock()
	defer worker.cacheMu.RUnlock()
	phase := worker.state.Identity.Phase
	if phase == simIdentityPhaseTransitioning || phase == simIdentityPhaseDegraded {
		return smsIdentityCandidate{}, fmt.Errorf("%w: device=%s phase=%s", ErrSMSIdentityTransitioning, deviceID, phase)
	}
	identity := normalizeSMSIdentity(SMSIdentity{
		ICCID: worker.state.Identity.ICCID,
		IMSI:  worker.state.Identity.IMSI,
	})
	imei := strings.TrimSpace(worker.state.Identity.IMEI)
	if resolvedBackendMode(worker.Config) == backend.BackendPCSC || (worker.Backend != nil && worker.Backend.Mode() == backend.BackendPCSC) {
		imei = ""
	}
	return smsIdentityCandidate{
		identity: identity,
		present:  identity.ICCID != "" || identity.IMSI != "",
		imei:     imei,
	}, nil
}

func mergeSMSIdentities(runtime, stored smsIdentityCandidate) (SMSIdentity, error) {
	runtime.identity = normalizeSMSIdentity(runtime.identity)
	stored.identity = normalizeSMSIdentity(stored.identity)
	if runtime.present && stored.present && runtime.identity.ICCID != "" && stored.identity.ICCID != "" && runtime.identity.ICCID != stored.identity.ICCID {
		return SMSIdentity{}, fmt.Errorf("%w: runtime ICCID differs from stored device binding", ErrSMSIdentityConflict)
	}
	if runtime.present && stored.present && runtime.identity.IMSI != "" && stored.identity.IMSI != "" && runtime.identity.IMSI != stored.identity.IMSI {
		return SMSIdentity{}, fmt.Errorf("%w: runtime IMSI differs from stored SIM binding", ErrSMSIdentityConflict)
	}
	identity := stored.identity
	if runtime.identity.ICCID != "" {
		identity.ICCID = runtime.identity.ICCID
	}
	if runtime.identity.IMSI != "" {
		identity.IMSI = runtime.identity.IMSI
	}
	return identity, nil
}

func normalizeSMSIdentity(identity SMSIdentity) SMSIdentity {
	identity.ICCID = strings.ToUpper(strings.TrimSpace(identity.ICCID))
	identity.ICCID = strings.ReplaceAll(identity.ICCID, " ", "")
	identity.ICCID = strings.TrimRight(identity.ICCID, "F")
	identity.IMSI = strings.TrimSpace(identity.IMSI)
	return identity
}

func (p *Pool) smsIdentityRepository() smsIdentityStore {
	if p != nil && p.smsIdentities != nil {
		return p.smsIdentities
	}
	return databaseSMSIdentityStore{}
}

func (w *Worker) resolveSMSIdentity() (SMSIdentity, error) {
	if w == nil || w.Pool == nil {
		return SMSIdentity{}, ErrSMSIdentityUnknown
	}
	return w.Pool.ResolveSMSIdentity(w.ID)
}

func (w *Worker) persistReceivedSMS(identity SMSIdentity, message inboundSMSRecord) error {
	return w.Pool.smsIdentityRepository().SaveReceived(identity, message)
}
