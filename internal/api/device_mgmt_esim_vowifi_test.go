package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/yibaiba/hideck/internal/device"
)

// A switch aborted because the old VoWiFi task did not stop is a conflict, the
// same status a manual reboot returns for it.
func TestEsimHTTPStatusMapsVoWiFiNotStoppedToConflict(t *testing.T) {
	err := fmt.Errorf("切卡前准备失败，未切换 profile: %w", device.ErrSwitchVoWiFiNotStopped)
	if got := esimSwitchHTTPStatus(err); got != http.StatusConflict {
		t.Fatalf("esimSwitchHTTPStatus=%d want %d", got, http.StatusConflict)
	}
	if got := esimDisableHTTPStatus(err); got != http.StatusConflict {
		t.Fatalf("esimDisableHTTPStatus=%d want %d", got, http.StatusConflict)
	}
}
