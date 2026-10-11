package backend

import (
	"context"
	"testing"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

func TestQMIRadioBandUsesBandNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *qmi.RFBandInfo
		want string
	}{
		{"nil", nil, ""},
		{"empty", &qmi.RFBandInfo{}, ""},
		{"B8", qmiTestBand(0x08, 127), "LTE BAND 8"},
		{"B39", qmiTestBand(0x08, 141), "LTE BAND 39"},
		{"B41", qmiTestBand(0x08, 149), "LTE BAND 41"},
		{"B66", qmiTestBand(0x08, 161), "LTE BAND 66"},
		{"unknown", qmiTestBand(0x08, 999), "LTE BAND CLASS 999（未识别）"},
		{"non LTE", qmiTestBand(0x04, 141), "RAT 4 BAND 141"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			band, channel := qmiRadioBandAndChannel(tc.info)
			if band != tc.want {
				t.Fatalf("band=%q want=%q", band, tc.want)
			}
			if tc.want != "" && channel != 38400 {
				t.Fatalf("channel=%d want=38400", channel)
			}
		})
	}
}

func qmiTestBand(rat uint8, raw uint16) *qmi.RFBandInfo {
	return &qmi.RFBandInfo{Bands: []qmi.RFBandInfoEntry{{
		RadioInterface: rat, ActiveBandClass: raw, ActiveChannel: 38400,
	}}}
}

func TestQMIBackendServingSystemReportsB39(t *testing.T) {
	src := &qmiBackendSendSourceStub{
		servingSeq: []*qmi.ServingSystem{{
			RegistrationState: qmi.RegStateRegistered, PSAttached: true,
			RadioInterface: 0x08, MCC: 460, MNC: 0,
		}},
		rfBandInfo: qmiTestBand(0x08, 141),
	}
	b, err := NewQMIBackend("/dev/null", src)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := b.GetServingSystem(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ss.RadioBand != "LTE BAND 39" || ss.RadioChannel != 38400 || ss.NetworkDuplex != "TDD" || ss.NetworkMode != "LTE" {
		t.Fatalf("serving system=%+v", ss)
	}
}
