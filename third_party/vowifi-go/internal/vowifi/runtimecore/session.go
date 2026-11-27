package runtimecore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/iniwex5/vowifi-go/engine/swu"
	"github.com/iniwex5/vowifi-go/internal/vowifi/epdg"
	"github.com/iniwex5/vowifi-go/internal/vowifi/imscore"
	"github.com/iniwex5/vowifi-go/internal/vowifi/netstack"
)

func RunSession(ctx context.Context, cfg SessionConfig) (*SessionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.SIM == nil {
		return nil, errors.New("runtimecore: SIM adapter is required")
	}
	manager := epdg.New()
	swuConfig := BuildSWUConfig(cfg)
	if err := swu.ValidateProposalConfig(swuConfig); err != nil {
		return nil, fmt.Errorf("runtimecore: invalid SWu proposal config: %w", err)
	}
	session, snapshot, err := StartAndWaitEPDG(
		ctx, cfg.DeviceID, cfg.TraceID, swuConfig, manager,
	)
	if err != nil {
		return nil, err
	}
	result := &SessionResult{
		DeviceID: cfg.DeviceID, EPDGMgr: manager, Session: session, Snapshot: snapshot,
		LocalAddr: snapshotLocalAddress(snapshot), Proxy: cfg.Proxy,
	}
	if cfg.OnTunnelReady != nil {
		cfg.OnTunnelReady(result)
	}
	if err := startIMS(ctx, cfg, result); err != nil {
		defaultStopSession(context.Background(), result)
		return nil, errors.Join(err, result.cleanupErr)
	}
	attachAdditionalPDNs(ctx, cfg, result)
	return result, nil
}

func startIMS(ctx context.Context, cfg SessionConfig, result *SessionResult) error {
	imsNetwork, err := resolveIMSNetwork(ctx, cfg.DataplaneMode, result)
	if err != nil {
		return err
	}
	imsAKA := cfg.SIM.IMSAKAProvider(cfg.Prepared.EffectiveAuthPlan())
	if imsAKA == nil {
		return errors.New("runtimecore: IMS AKA provider is unavailable")
	}
	eventBus := buildEventBus(ctx, cfg.Dispatch)
	imsConfig, err := buildIMSConfig(imsConfigInput{
		session: cfg, result: result, aka: imsAKA, network: imsNetwork, eventBus: eventBus,
	})
	if err != nil {
		return err
	}
	service, err := imscore.New(imsConfig)
	if err != nil {
		return fmt.Errorf("runtimecore: create IMS service: %w", err)
	}
	service.SetOnRegistered(cfg.OnIMSRegistered)
	service.SetOnSMSReadinessChanged(func(readiness imscore.SMSReadiness) {
		if cfg.OnSMSReadinessChanged != nil {
			cfg.OnSMSReadinessChanged(readiness)
		}
		if readiness.Ready && cfg.OnSMSReady != nil {
			cfg.OnSMSReady()
		}
	})
	result.IMSService = service
	if err := service.Start(ctx); err != nil {
		_ = service.Stop(context.Background())
		result.IMSService = nil
		return fmt.Errorf("runtimecore: start IMS service: %w", err)
	}
	return nil
}

func resolveIMSNetwork(
	ctx context.Context,
	mode string,
	result *SessionResult,
) (imscore.IMSNetwork, error) {
	normalizedMode := strings.ToLower(strings.TrimSpace(mode))
	if normalizedMode == "" || normalizedMode == swu.DataplaneModeUserspace {
		network, err := NewUserspaceIMSNetwork(ctx, result.Session, result.Snapshot)
		if err != nil {
			return nil, err
		}
		result.IMSNetwork = network
		return newInstallerIMSNetwork(
			netstack.AdaptIMSNetwork(network),
			resolveIPSec3GPPInstaller(normalizedMode, network),
		), nil
	}
	localIP := net.ParseIP(result.LocalAddr)
	return newInstallerIMSNetwork(
		imscore.NewSystemIMSNetwork(localIP),
		resolveIPSec3GPPInstaller(normalizedMode, nil),
	), nil
}
