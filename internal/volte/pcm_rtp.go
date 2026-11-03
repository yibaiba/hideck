package volte

import (
	"encoding/binary"
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/yibaiba/hideck/pkg/logger"
)

var firstPacketOnce atomic.Bool

// rtpPCMUPayload 取出 PCMU 载荷。必须走完整 RTP 解析：Chrome 的音频包带
// 头扩展（X 位，通常是 ssrc-audio-level / abs-send-time），早期手写版只跳过
// 12 字节固定头，把 8 字节扩展当成 µ-law 音频送进模组——每 20ms 一簇满幅
// 毛刺，对方听到的"电流声"就是它；而录音取的是已解析的 payload，所以录音
// 永远干净，把排查方向一直带偏。padding（P 位）同理必须剥掉。
func rtpPCMUPayload(raw []byte) ([]byte, bool) {
	packet, ok := parsePCMURTP(raw)
	if !ok {
		return nil, false
	}
	return packet.Payload, true
}

func parsePCMURTP(raw []byte) (*rtp.Packet, bool) {
	// Pion keeps Payload slices backed by its input. Buffered packets must own
	// immutable storage because the UDP reader reuses its receive buffer.
	owned := append([]byte(nil), raw...)
	var packet rtp.Packet
	if err := packet.Unmarshal(owned); err != nil {
		return nil, false
	}
	if packet.Version != 2 || packet.PayloadType != pcmuPayloadType || len(packet.Payload) == 0 {
		return nil, false
	}
	if firstPacketOnce.CompareAndSwap(false, true) {
		logger.Info("VoLTE 收到首个 RTP 包", "总长度", len(raw), "载荷长度", len(packet.Payload),
			"扩展", len(packet.Extensions) > 0, "填充", packet.Padding)
	}
	return &packet, true
}

func encodePCMURTP(sequence uint16, timestamp uint32, payload []byte) []byte {
	packet := make([]byte, 12+len(payload))
	packet[0] = 0x80
	packet[1] = pcmuPayloadType
	binary.BigEndian.PutUint16(packet[2:], sequence)
	binary.BigEndian.PutUint32(packet[4:], timestamp)
	binary.BigEndian.PutUint32(packet[8:], 0x48444543)
	copy(packet[12:], payload)
	return packet
}
