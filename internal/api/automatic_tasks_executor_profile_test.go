package api

import (
	"strings"
	"testing"

	"github.com/yibaiba/hideck/internal/automation"
	"github.com/yibaiba/hideck/internal/db"
)

func TestResolveAutomaticTaskProfileUsesCurrentPhysicalSIM(t *testing.T) {
	task := automation.Task{ProfileICCID: "8944101234567890123"}
	action, err := resolveAutomaticTaskProfile(task, "\"8944101234567890123F\"")
	if err != nil {
		t.Fatal(err)
	}
	if action != automaticTaskUseCurrentSIM {
		t.Fatalf("action=%d want current SIM", action)
	}
}

func TestAutomaticTaskRestoreKeepsManualPhoneEditedDuringTask(t *testing.T) {
	openTestDB(t)
	const iccid = "8944101234567890123"
	snapshot := db.DefaultCardPolicy(iccid)
	snapshot.NetworkEnabled = false
	snapshot.LocalPhoneOverride = "+447700900123"
	if err := db.UpsertCardPolicy(snapshot); err != nil {
		t.Fatal(err)
	}
	current := snapshot
	current.NetworkEnabled = true
	current.LocalPhoneOverride = "+447911123456"
	current.Source = "user"
	if err := db.UpsertCardPolicy(current); err != nil {
		t.Fatal(err)
	}

	if err := restoreAutomaticTaskCardPolicy(snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetCardPolicy(iccid)
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkEnabled || got.LocalPhoneOverride != "+447911123456" || got.Source != "user" {
		t.Fatalf("restored policy=%+v", got)
	}
}

func TestAutomaticTaskRestoreKeepsMWIModeEditedDuringTask(t *testing.T) {
	openTestDB(t)
	const iccid = "8944101234567890123"
	snapshot := db.DefaultCardPolicy(iccid)
	snapshot.NetworkEnabled = false
	if err := db.UpsertCardPolicy(snapshot); err != nil {
		t.Fatal(err)
	}
	current := snapshot
	current.NetworkEnabled = true
	current.MWIMode = db.MWIModeOff
	current.Source = "user"
	if err := db.UpsertCardPolicy(current); err != nil {
		t.Fatal(err)
	}

	if err := restoreAutomaticTaskCardPolicy(snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetCardPolicy(iccid)
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkEnabled || got.MWIMode != db.MWIModeOff || got.Source != "user" {
		t.Fatalf("restored policy=%+v", got)
	}
}

func TestResolveAutomaticTaskProfileSwitchesOnlyWithAID(t *testing.T) {
	task := automation.Task{
		ProfileICCID: "8944101234567890123",
		ProfileAID:   "A0000005591010FFFFFFFF8900000100",
	}
	action, err := resolveAutomaticTaskProfile(task, "8944100000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if action != automaticTaskSwitchESIM {
		t.Fatalf("action=%d want eSIM switch", action)
	}
}

func TestResolveAutomaticTaskProfileRejectsUnverifiablePhysicalSIM(t *testing.T) {
	tests := []struct {
		name    string
		current string
		wantErr string
	}{
		{name: "identity unavailable", wantErr: "current SIM ICCID is unavailable"},
		{name: "different card", current: "8944100000000000000", wantErr: "eSIM AID is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := automation.Task{ProfileICCID: "8944101234567890123"}
			_, err := resolveAutomaticTaskProfile(task, test.current)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error=%v want containing %q", err, test.wantErr)
			}
		})
	}
}
