package volte

import (
	"testing"
)

// buildRTP 组一个最小 RTP 包：固定头 + 可选扩展 + 载荷 + 可选填充。
func buildRTP(t *testing.T, extension, padding bool, payload []byte) []byte {
	t.Helper()
	first := byte(0x80)
	if extension {
		first |= 0x10
	}
	if padding {
		first |= 0x20
	}
	raw := []byte{first, pcmuPayloadType, 0x00, 0x07, 0x00, 0x00, 0x00, 0xa0, 0xde, 0xad, 0xbe, 0xef}
	if extension {
		// 0xBEDE + 长度 1 个字 + 1 个字的数据 = 8 字节，正是 Chrome 单扩展的情形
		raw = append(raw, 0xbe, 0xde, 0x00, 0x01, 0x10, 0xaa, 0x00, 0x02)
	}
	raw = append(raw, payload...)
	if padding {
		raw = append(raw, 0x00, 0x00, 0x00, 0x04)
	}
	return raw
}

// TestRTPPCMUPayloadStripsHeaderExtension 覆盖 Chrome 音频包带 RTP 头扩展的
// 情况：早期手写解析只跳过 12 字节固定头，把扩展字节当成 µ-law 音频写进模组，
// 每 20ms 产生一簇满幅毛刺（对方听到的"电流声"）。载荷必须严格是 160 样本。
func TestRTPPCMUPayloadStripsHeaderExtension(t *testing.T) {
	payload := make([]byte, pcmuFrameSamples)
	for index := range payload {
		payload[index] = 0xff
	}
	raw := buildRTP(t, true, false, payload)

	got, ok := rtpPCMUPayload(raw)
	if !ok {
		t.Fatal("packet rejected")
	}
	if len(got) != pcmuFrameSamples {
		t.Fatalf("payload = %d samples, want %d (extension leaked into audio)", len(got), pcmuFrameSamples)
	}
	for index, value := range got {
		if value != 0xff {
			t.Fatalf("sample %d = %#x, want 0xff (payload corrupted)", index, value)
		}
	}
}

// TestRTPPCMUPayloadStripsPadding 覆盖 P 位填充：填充字节同样不能进音频。
func TestRTPPCMUPayloadStripsPadding(t *testing.T) {
	payload := make([]byte, pcmuFrameSamples)
	for index := range payload {
		payload[index] = 0xff
	}
	raw := buildRTP(t, false, true, payload)

	got, ok := rtpPCMUPayload(raw)
	if !ok {
		t.Fatal("packet rejected")
	}
	if len(got) != pcmuFrameSamples {
		t.Fatalf("payload = %d samples, want %d (padding leaked into audio)", len(got), pcmuFrameSamples)
	}
}
