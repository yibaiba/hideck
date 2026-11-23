package host

import "errors"

var (
	errAudioNotConnected = errors.New("模组直拨音频尚未连接")
	errAudioInUse        = errors.New("另一通电话正在使用模组音频")
)

// claimAudioRoute retains an activated call as the foreground audio owner.
// A call that is still held or waiting has to leave the hardware alone.
func claimAudioRoute(d *device, current *call) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if current.snapshot.Held || current.snapshot.State == "waiting" {
		return false
	}
	if d.call == nil && current.snapshot.State == "connected" {
		d.call = current
		delete(d.otherCalls, current.snapshot.CallID)
	}
	return d.call == current
}

// AttachCallMedia builds the audio path for a call the modem answered on its
// own. A waiting call accepted by AT+CHLD never went through Answer, so nothing
// opened a bridge for it; recovery has to be able to do that after the fact.
// The returned offer is what the browser media attaches to.
func (c *Controller) AttachCallMedia(deviceID, callID, sdp string) (string, error) {
	d := c.get(deviceID)
	if d == nil {
		return "", errors.New("模组直拨会话不存在")
	}
	d.op.Lock()
	defer d.op.Unlock()
	current, err := matchingCall(d, callID)
	if err != nil {
		return "", err
	}
	if !claimAudioRoute(d, current) {
		return "", errAudioInUse
	}
	if err := c.prepareAnswerMedia(d, current, sdp); err != nil {
		return "", err
	}
	d.mu.Lock()
	offer := current.snapshot.ClientSDP
	d.mu.Unlock()
	return offer, nil
}

// UpdateCallMedia binds a browser relay to an already negotiated endpoint.
// A call the modem activated on its own never opened a bridge, and a ringing
// call owns an endpoint long before Answer: the browser is attached to that
// endpoint, so open the missing bridge here instead of failing the recovery.
func (c *Controller) UpdateCallMedia(deviceID, callID, sdp string) error {
	d := c.get(deviceID)
	if d == nil {
		return errors.New("模组直拨会话不存在")
	}
	remote, err := relay(sdp)
	if err != nil {
		return err
	}
	d.op.Lock()
	defer d.op.Unlock()
	current, err := matchingCall(d, callID)
	if err != nil {
		return err
	}
	if !claimAudioRoute(d, current) {
		return errAudioInUse
	}
	if current.bridge != nil {
		return current.bridge.SetRemote(remote)
	}
	if current.conn == nil {
		return errAudioNotConnected
	}
	return c.openMedia(d.ctx, d, current, sdp)
}
