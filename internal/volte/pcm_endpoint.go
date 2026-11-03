package volte

import (
	"errors"
	"net"
)

// SetRemote preserves the socket, PCM device and outgoing RTP timeline. mediaMu
// also covers UDP writes and PCM playback: on return no old operation can start.
func (b *PCMBridge) SetRemote(remote net.Addr) error {
	_, err := b.swapRemote(remote)
	return err
}

// Return the previous endpoint under the same lock so concurrent updates log
// the endpoint they actually replaced.
func (b *PCMBridge) swapRemote(remote net.Addr) (net.Addr, error) {
	if b == nil || remote == nil {
		return nil, errors.New("volte: media endpoint is unavailable")
	}
	b.mediaMu.Lock()
	defer b.mediaMu.Unlock()
	if b.isClosed() {
		return nil, net.ErrClosed
	}
	previous := b.remote
	if samePCMEndpoint(b.remote, remote) {
		return previous, nil
	}
	b.remote = remote
	b.generation++
	diagnostics := b.buffer.diagnostics
	b.buffer = newAdaptivePCMBuffer()
	b.buffer.diagnostics = diagnostics
	return previous, nil
}

func samePCMEndpoint(left, right net.Addr) bool {
	return left != nil && right != nil && left.Network() == right.Network() && left.String() == right.String()
}

func (b *PCMBridge) isClosed() bool {
	select {
	case <-b.closed:
		return true
	default:
		return false
	}
}

func (b *PCMBridge) mediaEndpoint() (net.Addr, uint64) {
	b.mediaMu.Lock()
	defer b.mediaMu.Unlock()
	return b.remote, b.generation
}

func (b *PCMBridge) acceptArrival(arrival pcmRTPArrival) (pcmRTPArrival, bool) {
	b.mediaMu.Lock()
	defer b.mediaMu.Unlock()
	if b.isClosed() {
		return arrival, false
	}
	// Only the first valid RTP packet may discover an incoming call's endpoint.
	if b.remote == nil {
		b.remote = arrival.remote
	}
	arrival.generation = b.generation
	return arrival, samePCMEndpoint(b.remote, arrival.remote)
}

func (b *PCMBridge) queueDownlink(arrival pcmRTPArrival) {
	b.mediaMu.Lock()
	defer b.mediaMu.Unlock()
	if b.isClosed() || arrival.generation != b.generation || !samePCMEndpoint(b.remote, arrival.remote) {
		return
	}
	b.buffer.push(arrival.packet, arrival.received)
}

func (b *PCMBridge) playBufferedDownlink(state *pcmWriteLogState) {
	b.mediaMu.Lock()
	defer b.mediaMu.Unlock()
	if b.isClosed() {
		return
	}
	packet, kind := b.buffer.pop()
	b.playDownlink(packet, kind, state)
}

func (b *PCMBridge) writeUplink(packet []byte, generation uint64) error {
	b.mediaMu.Lock()
	defer b.mediaMu.Unlock()
	if b.isClosed() {
		return net.ErrClosed
	}
	// A PCM read may have blocked across a takeover. Discard that old frame.
	if generation != b.generation {
		return nil
	}
	_, err := b.conn.WriteTo(packet, b.remote)
	return err
}

func (b *PCMBridge) saveJitterStats() {
	b.mediaMu.Lock()
	defer b.mediaMu.Unlock()
	b.setJitterStats(b.buffer.stats())
}
