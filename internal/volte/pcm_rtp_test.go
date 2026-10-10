package volte

import (
	"bytes"
	"testing"

	"github.com/yibaiba/hideck/internal/phone"
)

func TestPCMUSilenceExcludesRTPExtensionAndPadding(t *testing.T) {
	// Raw wire bytes: RTP v2, one CSRC, one-word BEDE extension, PCMU,
	// 160 silence samples and four padding bytes. Extension bytes are non-silent.
	raw := []byte{0xb1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1,
		0, 0, 0, 2, 0xbe, 0xde, 0, 1, 0x10, 0x7f, 0, 0}
	silence := bytes.Repeat([]byte{0xff}, 160)
	raw = append(raw, silence...)
	raw = append(raw, 0, 0, 0, 4)
	payload, ok := rtpPCMUPayload(raw)
	if !ok || !bytes.Equal(payload, silence) {
		t.Fatalf("RTP metadata leaked into audio: valid=%v payload bytes=%d", ok, len(payload))
	}
	for i, sample := range phone.DecodePCMU(payload) {
		if sample != 0 {
			t.Fatalf("silent sample %d decoded as %d", i, sample)
		}
	}
}

func TestPCMUPayloadRejectsMalformedRTP(t *testing.T) {
	for _, raw := range [][]byte{
		{0x90, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},         // Missing extension header.
		{0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},         // Empty audio.
		{0x40, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff},   // Wrong RTP version.
		{0x80, 101, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff}, // DTMF, not PCMU.
	} {
		if _, ok := rtpPCMUPayload(raw); ok {
			t.Fatalf("accepted malformed/non-audio RTP: %x", raw)
		}
	}
}
