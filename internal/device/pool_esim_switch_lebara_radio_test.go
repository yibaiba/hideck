package device

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/db"
	qmicore "github.com/yibaiba/hideck/internal/qmi"
)

var errLowPowerRejected = errors.New("set operating mode rejected")

// lowPowerFailingBackend fails every request for the low power mode; the mode
// read back is whatever the stub reports, or readErr when set.
type lowPowerFailingBackend struct {
	*esimSwitchRestoreBackendStub
	readErr error
}

func (b lowPowerFailingBackend) GetOperatingMode(ctx context.Context) (backend.OperatingMode, error) {
	if b.readErr != nil {
		return backend.ModeLowPower, b.readErr
	}
	return b.esimSwitchRestoreBackendStub.GetOperatingMode(ctx)
}

func (b lowPowerFailingBackend) SetOperatingMode(ctx context.Context, mode backend.OperatingMode) error {
	_ = b.esimSwitchRestoreBackendStub.SetOperatingMode(ctx, mode)
	if mode == backend.ModeLowPower {
		return errLowPowerRejected
	}
	return nil
}

const (
	lebaraTargetICCID = "8944000000000000087"
	plainTargetICCID  = "8986000000000000095"
)

func initSwitchTargetDB(t *testing.T) {
	t.Helper()
	if err := db.Init(filepath.Join(t.TempDir(), "lebara-radio.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if db.DB != nil {
			if sqlDB, err := db.DB.DB(); err == nil {
				_ = sqlDB.Close()
			}
			db.DB = nil
		}
	})
	if err := db.UpsertSIMCard(lebaraTargetICCID, "234870000000001", "", "Lebara", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSIMCard(plainTargetICCID, "460001234567891", "", "CMCC", nil); err != nil {
		t.Fatal(err)
	}
}

func setCurrentSIMIdentity(w *Worker, iccid, imsi string) {
	w.cacheMu.Lock()
	w.state.Identity.ICCID = iccid
	w.state.Identity.IMSI = imsi
	w.state.Identity.Ready = true
	w.state.Identity.Phase = simIdentityPhaseReady
	w.cacheMu.Unlock()
}

func hasOperatingMode(calls []backend.OperatingMode, mode backend.OperatingMode) bool {
	for _, call := range calls {
		if call == mode {
			return true
		}
	}
	return false
}

// A Lebara UK target, or one that could not be classified, must not see the
// radio on while its profile is enabled: when the radio cannot be turned off
// first, the switch is aborted before any profile APDU, and the failure path
// never turns the radio on for a current card that is or may be Lebara UK.
func TestESIMSwitchToProtectedTargetAbortsWhenRadioStaysOn(t *testing.T) {
	for _, tc := range []struct {
		name               string
		currentIMSI        string
		classifyFails      bool
		currentKnownLebara bool
		withQMICore        bool
		wantOnlineAfter    bool
		wantIMSIRestored   string
	}{
		{name: "plain current card", currentIMSI: "460001234567890", wantOnlineAfter: true, wantIMSIRestored: "460001234567890"},
		{name: "current card is Lebara UK", currentIMSI: "234870000000009"},
		{name: "current Lebara UK flipped to 20404", currentIMSI: "204040000000009", currentKnownLebara: true},
		{name: "current IMSI 20404 without Lebara history", currentIMSI: "204040000000009"},
		{name: "current identity unknown"},
		{name: "target not classified", currentIMSI: "460001234567890", classifyFails: true, wantOnlineAfter: true},
		{name: "QMI core branch", currentIMSI: "234870000000009", withQMICore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initSwitchTargetDB(t)
			p := NewPool(&config.Config{})
			defer p.cancel()
			stub := &esimSwitchRestoreBackendStub{mode: backend.BackendQMI, getMode: backend.ModeOnline}
			w := &Worker{ID: "dev-1", Backend: lowPowerFailingBackend{esimSwitchRestoreBackendStub: stub}, Config: config.DeviceConfig{ID: "dev-1"}}
			const currentICCID = "8986000000000000011"
			if tc.currentKnownLebara {
				// The card was seen as Lebara UK (23487) before it flipped to 20404.
				if err := db.UpsertSIMCard(currentICCID, "234870000000009", "", "Lebara", nil); err != nil {
					t.Fatal(err)
				}
				if class, err := ClassifyLebaraUKForICCID(currentICCID, ""); err != nil || !class.IsLebara {
					t.Fatalf("current card classification = (%+v, %v), want Lebara UK from its history", class, err)
				}
			}
			if tc.currentIMSI != "" {
				setCurrentSIMIdentity(w, currentICCID, tc.currentIMSI)
			}
			var qmiMgr *qmicore.Manager
			if tc.withQMICore {
				qmiMgr = &qmicore.Manager{}
				setPrivateMapFieldSwitchRestore(t, qmiMgr, "apduSessions", byte(1), func(elemType reflect.Type) reflect.Value {
					elem := reflect.New(elemType).Elem()
					elem.FieldByName("Channel").SetUint(1)
					elem.FieldByName("Owner").SetString("test")
					elem.FieldByName("OpenedAt").Set(reflect.ValueOf(time.Now()))
					return elem
				})
				w.QMICore = qmiMgr
			}
			p.workers[w.ID] = w
			if tc.classifyFails {
				sqlDB, err := db.DB.DB()
				if err != nil {
					t.Fatal(err)
				}
				_ = sqlDB.Close()
			}

			snapshot := p.beginESIMSwitchForWorker(w.ID, lebaraTargetICCID, w)
			if snapshot.TargetLebaraCandidate == tc.classifyFails || snapshot.TargetClassifyFailed != tc.classifyFails {
				t.Fatalf("target classification = (lebara %v, failed %v), test setup is wrong",
					snapshot.TargetLebaraCandidate, snapshot.TargetClassifyFailed)
			}
			token, err := p.prepareESIMSwitch(w.ID, snapshot)
			if !errors.Is(err, ErrSwitchRadioNotReleased) || !errors.Is(err, errLowPowerRejected) {
				t.Fatalf("prepareESIMSwitch err = %v, want ErrSwitchRadioNotReleased wrapping the backend error", err)
			}
			if token == 0 {
				t.Fatal("aborted switch must return its token so the failure callback can clear it")
			}
			if len(stub.setCalls) != 1 || stub.setCalls[0] != backend.ModeLowPower {
				t.Fatalf("setCalls before the failure callback = %v, want one low power request", stub.setCalls)
			}
			if qmiMgr != nil {
				if got := privateMapLenSwitchRestore(t, qmiMgr, "apduSessions"); got != 1 {
					t.Fatalf("apduSessions len = %d, want the current card's session kept", got)
				}
			}

			p.handleESIMSwitchFailedWithError(w.ID, token, err)
			if p.IsESIMSwitching(w.ID) {
				t.Fatal("aborted switch left the device in the switching state")
			}
			if got := hasOperatingMode(stub.setCalls, backend.ModeOnline); got != tc.wantOnlineAfter {
				t.Fatalf("setCalls after the failure callback = %v, radio online = %v, want %v",
					stub.setCalls, got, tc.wantOnlineAfter)
			}
			if tc.wantIMSIRestored != "" {
				status := w.ProjectDeviceStatus()
				if status.IMSI != tc.wantIMSIRestored || status.ICCID != currentICCID || w.CurrentICCID() != currentICCID {
					t.Fatalf("restored identity = (%q, %q, current %q), want (%q, %q)",
						status.ICCID, status.IMSI, w.CurrentICCID(), currentICCID, tc.wantIMSIRestored)
				}
			}
		})
	}
}

// A failed release aborts the switch even when the mode reads back as off (the
// QMI backend passes a reset in progress through as 4, the persistent flight
// value) or cannot be read back; the read back is only reported in the error.
func TestESIMSwitchToLebaraAbortsWhateverTheModeReadsBack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		getMode  backend.OperatingMode
		readErr  error
		wantText string
	}{
		{name: "reads back as flight", getMode: backend.ModeRFOff, wantText: "读回射频模式 4"},
		{name: "reads back as low power", getMode: backend.ModeLowPower, wantText: "读回射频模式 0"},
		{name: "read back fails", readErr: errors.New("dms timeout"), wantText: "读回射频模式失败: dms timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initSwitchTargetDB(t)
			p := NewPool(&config.Config{})
			defer p.cancel()
			stub := &esimSwitchRestoreBackendStub{mode: backend.BackendQMI, getMode: tc.getMode}
			w := &Worker{ID: "dev-1", Backend: lowPowerFailingBackend{stub, tc.readErr}, Config: config.DeviceConfig{ID: "dev-1"}}
			p.workers[w.ID] = w

			token, err := p.prepareESIMSwitch(w.ID, p.beginESIMSwitchForWorker(w.ID, lebaraTargetICCID, w))
			if !errors.Is(err, ErrSwitchRadioNotReleased) || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("prepareESIMSwitch err = %v, want ErrSwitchRadioNotReleased reporting %q", err, tc.wantText)
			}
			p.handleESIMSwitchFailedWithError(w.ID, token, err)
		})
	}
}

// Other cards keep the existing RadioCycle behaviour: a failed release is only
// logged and the switch goes on.
func TestESIMSwitchRadioCycleReleaseFailureStillSwitchesOtherCards(t *testing.T) {
	initSwitchTargetDB(t)
	p := NewPool(&config.Config{})
	defer p.cancel()
	stub := &esimSwitchRestoreBackendStub{mode: backend.BackendQMI, getMode: backend.ModeOnline}
	w := &Worker{ID: "dev-1", Backend: lowPowerFailingBackend{esimSwitchRestoreBackendStub: stub}, Config: config.DeviceConfig{
		ID: "dev-1", ESIMSwitch: config.ESIMSwitchConfig{RadioCycle: true},
	}}
	p.workers[w.ID] = w

	snapshot := p.beginESIMSwitchForWorker(w.ID, plainTargetICCID, w)
	if snapshot.TargetLebaraCandidate || snapshot.TargetClassifyFailed {
		t.Fatalf("plain target classified as protected: %+v", snapshot)
	}
	token, err := p.prepareESIMSwitch(w.ID, snapshot)
	if err != nil || token == 0 {
		t.Fatalf("prepareESIMSwitch = (%d, %v), want the switch to go on", token, err)
	}
	if len(stub.setCalls) != 1 || stub.setCalls[0] != backend.ModeLowPower {
		t.Fatalf("setCalls = %v, want one low power request", stub.setCalls)
	}
	p.handleESIMSwitchFailed(w.ID, token)
}
