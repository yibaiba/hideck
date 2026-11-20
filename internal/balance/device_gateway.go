package balance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/carrierquery"
	"github.com/yibaiba/hideck/internal/db"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/pkg/smscodec"
)

const backendUSSDTimeout = 45 * time.Second

type PoolGateway struct {
	pool *device.Pool
}

func NewPoolGateway(pool *device.Pool) *PoolGateway {
	return &PoolGateway{pool: pool}
}

func (g *PoolGateway) Snapshot(deviceID string) (DeviceSnapshot, error) {
	if g == nil || g.pool == nil {
		return DeviceSnapshot{}, ErrDeviceNotFound
	}
	worker := g.pool.GetWorker(deviceID)
	if worker == nil {
		return DeviceSnapshot{}, ErrDeviceNotFound
	}
	status := worker.GetCachedDeviceStatus()
	if strings.TrimSpace(status.ICCID) == "" {
		return DeviceSnapshot{}, ErrIdentityMissing
	}
	return DeviceSnapshot{DeviceID: worker.ID, ICCID: status.ICCID, MCC: status.NativeMCC,
		MNC: status.NativeMNC, SPN: status.NativeSPN, VoWiFiActive: g.pool.IsVoWiFiActive(worker.ID),
		RouteSMSViaVoWiFi: g.pool.ShouldRouteSMSViaVoWiFi(worker.ID)}, nil
}

func (g *PoolGateway) SendVoWiFiSMS(ctx context.Context, snapshot DeviceSnapshot, destination, payload string) error {
	if g == nil || g.pool == nil {
		return ErrDeviceNotFound
	}
	worker := g.pool.GetWorker(snapshot.DeviceID)
	if worker == nil {
		return ErrDeviceNotFound
	}
	_, err := g.pool.SendRoutedSMSForIdentity(ctx, worker, destination, payload, smscodec.SubmitOptions{}, device.SMSIdentity{ICCID: snapshot.ICCID})
	return err
}

func (g *PoolGateway) SendBackendSMS(ctx context.Context, snapshot DeviceSnapshot, destination, payload string) error {
	worker := g.pool.GetWorker(snapshot.DeviceID)
	if worker == nil {
		return ErrDeviceNotFound
	}
	return worker.SendSMSForIdentity(ctx, destination, payload, smscodec.SubmitOptions{}, device.SMSIdentity{ICCID: snapshot.ICCID})
}

func (g *PoolGateway) SendVoWiFiUSSD(ctx context.Context, deviceID, code string) (USSDResponse, error) {
	result, err := g.pool.SendVoWiFiUSSD(ctx, deviceID, code)
	if err != nil {
		return USSDResponse{}, err
	}
	return USSDResponse{Text: result.Text, Raw: firstNonEmpty(result.RawText, result.RawXML)}, nil
}

func (g *PoolGateway) SendBackendUSSD(ctx context.Context, deviceID, code string) (USSDResponse, error) {
	worker := g.pool.GetWorker(deviceID)
	if worker == nil {
		return USSDResponse{}, ErrDeviceNotFound
	}
	provider, ok := worker.Backend.(backend.USSDProvider)
	if !ok || provider == nil {
		return USSDResponse{}, fmt.Errorf("设备 %s 的当前后端不支持 USSD", deviceID)
	}
	result, err := provider.ExecuteUSSD(ctx, code, backendUSSDTimeout)
	if err != nil {
		return USSDResponse{}, err
	}
	return USSDResponse{Text: result.Text, Raw: result.RawText}, nil
}

type DBCustomRuleSource struct{}

func (DBCustomRuleSource) ListCustomCarrierQueryRules() ([]carrierquery.Rule, error) {
	return db.ListCustomCarrierQueryRules()
}
