package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/iniwex5/vowifi-go/runtimehost/voicehost"
	"github.com/yibaiba/hideck/internal/modemvoice/media"
	"github.com/yibaiba/hideck/pkg/logger"
)

func offer(conn net.PacketConn) string {
	return fmt.Sprintf("v=0\r\no=hideck 0 0 IN IP4 127.0.0.1\r\ns=HiDeck Modem Voice\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=ptime:20\r\na=sendrecv\r\n", conn.LocalAddr().(*net.UDPAddr).Port)
}

// The SDP comes from the phone service's local relay, never from a learned RTP
// source. Validate it before enabling audio or issuing ATD/ATA.
func relay(sdp string) (netip.AddrPort, error) {
	var host string
	port := 0
	pcmu := false
	for _, line := range strings.Split(strings.ReplaceAll(sdp, "\r", ""), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "c=IN" {
			host = fields[2]
		}
		if len(fields) >= 4 && fields[0] == "m=audio" && fields[2] == "RTP/AVP" {
			port, _ = strconv.Atoi(fields[1])
			for _, pt := range fields[3:] {
				if pt == "0" {
					pcmu = true
				}
			}
		}
	}
	address, err := netip.ParseAddr(host)
	if err != nil || !address.IsLoopback() || port <= 0 || port > 65535 || !pcmu {
		return netip.AddrPort{}, errors.New("模组直拨需要本机 PCMU 媒体通道")
	}
	return netip.AddrPortFrom(address, uint16(port)), nil
}

// A failed ATA keeps the incoming call owned. A later explicit answer may use
// a new browser relay without allocating a second hardware audio route.
func (c *Controller) prepareAnswerMedia(d *device, current *call, sdp string) error {
	remote, err := relay(sdp)
	if err != nil {
		return err
	}
	if current.bridge != nil {
		return current.bridge.SetRemote(remote)
	}
	if current.conn == nil {
		conn, err := c.options.Listen()
		if err != nil {
			return err
		}
		d.mu.Lock()
		current.conn, current.snapshot.ClientSDP = conn, offer(conn)
		d.mu.Unlock()
	}
	if err := c.openMedia(d.ctx, d, current, sdp); err != nil {
		closeErr := current.conn.Close()
		current.conn = nil
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		return errors.Join(err, closeErr)
	}
	return nil
}

func (c *Controller) openMedia(ctx context.Context, d *device, current *call, sdp string) error {
	remote, err := relay(sdp)
	if err != nil {
		return err
	}
	route, err := d.resources.Route()
	if err != nil {
		return err
	}
	mediaCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	bridge, err := media.Open(mediaCtx, media.OpenRequest{Conn: current.conn, Remote: remote, Route: route,
		ListenOnly: strings.Contains(sdp, "a=recvonly")})
	stop()
	if err != nil {
		cancel()
		return err
	}
	if err := ctx.Err(); err != nil {
		closeErr := bridge.Close()
		cancel()
		return errors.Join(err, closeErr)
	}
	d.mu.Lock()
	current.bridge = bridge
	d.mu.Unlock()
	current.cancelMedia = cancel
	go func() {
		<-bridge.Done()
		d.op.Lock()
		defer d.op.Unlock()
		d.mu.Lock()
		foreground := d.call
		active := foreground != nil && foreground.bridge == bridge
		d.mu.Unlock()
		if active && bridge.Err() != nil && d.ctx.Err() == nil {
			d.setStatus("failed", bridge.Err())
			c.publish(notification{event: voicehost.CallEvent{Type: "CallMediaUpdated", DeviceID: d.id,
				CallID: foreground.snapshot.CallID, RecordingError: bridge.Err().Error(), Time: time.Now()}})
		}
	}()
	return nil
}

// Called with op held. Detach before closing so media completion cannot end a
// later call. Close errors remain visible and are never replaced by null audio.
func (c *Controller) endMedia(d *device, reason string) error {
	d.mu.Lock()
	current := d.call
	d.call = nil
	d.mu.Unlock()
	if current == nil {
		return nil
	}
	var err error
	if current.bridge != nil {
		stats := current.bridge.Stats()
		logger.Info("模组音频转发统计", "device_id", d.id, "from_modem", stats.FromModem, "to_modem", stats.ToModem, "playback_peak", stats.PlaybackPeak, "muted", stats.Muted, "rejected_peer", stats.RejectedPeer, "other_payload", stats.OtherPayload, "end_reason", reason)
		err = current.bridge.Close()
	} else if current.conn != nil {
		err = current.conn.Close()
	}
	if current.cancelMedia != nil {
		current.cancelMedia()
	}
	event := voicehost.CallEvent{Type: "CallEnded", DeviceID: d.id, CallID: current.snapshot.CallID,
		Direction: current.snapshot.Direction, Reason: reason, Time: time.Now()}
	if err != nil {
		event.RecordingError = err.Error()
	}
	c.publish(notification{event: event})
	event.Type = "CallFinalized"
	event.AudioCodec = "PCMU"
	c.publish(notification{event: event})
	return err
}
