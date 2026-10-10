package qmi

import "testing"

func TestVoiceCallInfoPreservesRawProtocolStates(t *testing.T) {
	for _, raw := range []byte{4, 1, 5, 3, 9} {
		calls, err := parseVoiceCallInfoArray([]byte{1, 1, raw, 0, 1, 0, 0, 0})
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || uint8(calls[0].State) != raw {
			t.Fatalf("raw %d remapped: %+v", raw, calls)
		}
	}
}
