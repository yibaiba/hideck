package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/gin-gonic/gin"
	"github.com/iniwex5/vowifi-go/runtimehost"
	"github.com/yibaiba/hideck/internal/backend"
	"github.com/yibaiba/hideck/internal/config"
	"github.com/yibaiba/hideck/internal/device"
	"github.com/yibaiba/hideck/internal/vowifihost"
)

// Maintainer #119 point 2: a manual reboot cancels the old VoWiFi task and
// waits for its exit first; if it does not exit, the modem is not reset.
func TestHandleDeviceMgmtRebootDoesNotResetModemWhileOldVoWiFiRunnerRuns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := manualRebootVoWiFiStopTimeout
	manualRebootVoWiFiStopTimeout = 100 * time.Millisecond
	defer func() { manualRebootVoWiFiStopTimeout = previous }()

	p := device.NewPool(&config.Config{})
	be := &ussdDeviceBackendStub{mode: backend.BackendQMI}
	worker := &device.Worker{ID: "dev-qmi", Backend: be}
	setNestedPrivateField(t, p, []string{"workers"}, map[string]*device.Worker{"dev-qmi": worker})

	release := make(chan struct{})
	defer close(release)
	inst := runtimehost.StartWithRunnerForTest(context.Background(), "dev-qmi", func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	field := reflect.ValueOf(p).Elem().FieldByName("vowifiHost")
	host := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*vowifihost.Manager)
	host.RuntimeStore().SetInstance("dev-qmi", inst)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Params = gin.Params{{Key: "device_id", Value: "dev-qmi"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/devices/dev-qmi/actions/reboot", nil)
	server := &Server{pool: p}

	server.handleDeviceMgmtReboot(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if be.rebooted {
		t.Fatal("modem reset was sent while the old VoWiFi runner was still running")
	}
}

// rebootTestHost returns the Pool's VoWiFi host with a stoppable instance and a
// command recorder whose Disable removes that instance.
func rebootTestHost(t *testing.T, p *device.Pool, id string) (*vowifihost.Manager, func() []string) {
	t.Helper()
	field := reflect.ValueOf(p).Elem().FieldByName("vowifiHost")
	host := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*vowifihost.Manager)
	inst := &runtimehost.Instance{}
	host.RuntimeStore().SetInstance(id, inst)
	var mu sync.Mutex
	var submitted []string
	host.LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		mu.Lock()
		submitted = append(submitted, cmd.Kind.String())
		mu.Unlock()
		if cmd.Kind == vowifihost.LifecycleCommandDisable {
			host.RuntimeStore().DeleteInstance(id, inst)
		}
		return nil
	}
	return host, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), submitted...)
	}
}

func serveReboot(p *device.Pool, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Params = gin.Params{{Key: "device_id", Value: id}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/devices/"+id+"/actions/reboot", nil)
	(&Server{pool: p}).handleDeviceMgmtReboot(ctx)
	return rec
}

// A QMI reset that was written but whose reply failed may still reset the
// modem: VoWiFi is left stopped instead of racing the reset.
func TestHandleDeviceMgmtRebootLeavesVoWiFiStoppedAfterAttemptedReset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := device.NewPool(&config.Config{})
	worker := &device.Worker{ID: "dev-qmi", Backend: &ussdDeviceBackendStub{
		mode: backend.BackendQMI, rebootErr: errors.New("qmi: connection closed"),
	}}
	p.AttachWorkerForTest(worker)
	_, submitted := rebootTestHost(t, p, "dev-qmi")

	rec := serveReboot(p, "dev-qmi")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "VoWiFi 已停止") {
		t.Fatalf("status=%d body=%s, want 500 saying VoWiFi stays stopped", rec.Code, rec.Body.String())
	}
	time.Sleep(200 * time.Millisecond)
	if got := submitted(); len(got) != 1 || got[0] != "disable" {
		t.Fatalf("submitted=%v, want only [disable]", got)
	}
}

// The VoWiFi stop may take seconds; if the Worker was replaced meanwhile, the
// reset must not be sent to the new one.
func TestHandleDeviceMgmtRebootDoesNotResetReplacedWorker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := device.NewPool(&config.Config{})
	be := &ussdDeviceBackendStub{mode: backend.BackendQMI}
	worker := &device.Worker{ID: "dev-qmi", Backend: be}
	p.AttachWorkerForTest(worker)
	replacementBackend := &ussdDeviceBackendStub{mode: backend.BackendQMI}

	field := reflect.ValueOf(p).Elem().FieldByName("vowifiHost")
	host := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*vowifihost.Manager)
	inst := &runtimehost.Instance{}
	host.RuntimeStore().SetInstance("dev-qmi", inst)
	host.LifecycleControllerForTest().TestRun = func(ctx context.Context, cmd vowifihost.LifecycleCommand) error {
		if cmd.Kind == vowifihost.LifecycleCommandDisable {
			host.RuntimeStore().DeleteInstance("dev-qmi", inst)
			setNestedPrivateField(t, p, []string{"workers"}, map[string]*device.Worker{
				"dev-qmi": {ID: "dev-qmi", Backend: replacementBackend},
			})
		}
		return nil
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Params = gin.Params{{Key: "device_id", Value: "dev-qmi"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/devices/dev-qmi/actions/reboot", nil)
	(&Server{pool: p}).handleDeviceMgmtReboot(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if be.rebooted || replacementBackend.rebooted {
		t.Fatal("reset was sent after the Worker had been replaced")
	}
}
