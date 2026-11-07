package imscore

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

type subscriptionNotifyState struct {
	state, reason     string
	expires           time.Duration
	expiresPresent    bool
	retryAfter        time.Duration
	retryAfterPresent bool
}

type subscriptionNotification struct {
	raw       string
	context   subscriptionContext
	version   uint64
	mwi       bool
	queue     *subscriptionNotificationQueue
	replyDone bool // Protected by Service.mu, like the queue.
}

func parseSubscriptionNotifyState(raw string) (subscriptionNotifyState, error) {
	parts := strings.Split(rawSIPHeaderValue(raw, "Subscription-State"), ";")
	state := subscriptionNotifyState{state: strings.ToLower(strings.TrimSpace(parts[0]))}
	if state.state != "active" && state.state != "pending" && state.state != "terminated" {
		return state, fmt.Errorf("invalid Subscription-State %q", state.state)
	}
	for _, part := range parts[1:] {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		name, value = strings.ToLower(name), strings.TrimSpace(value)
		switch name {
		case "reason":
			state.reason = strings.ToLower(value)
		case "expires", "retry-after":
			if state.state == "terminated" && name == "expires" {
				continue // RFC 6665: expires has no meaning on termination.
			}
			seconds, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return state, fmt.Errorf("invalid Subscription-State %s", name)
			}
			if name == "expires" {
				state.expires, state.expiresPresent = time.Duration(seconds)*time.Second, true
			} else {
				state.retryAfter, state.retryAfterPresent = time.Duration(seconds)*time.Second, true
			}
		}
	}
	return state, nil
}

func (s *Service) prepareInboundNotification(raw string) (inboundSIPResult, error) {
	event := sipEventPackage(raw)
	if event != registrationEventPackage && event != mwiEventPackage {
		// Other event packages retain their existing dispatch ownership.
		response, err := buildSIPRequestResponse(raw, 200)
		return inboundSIPResult{response: response, afterReply: func() { s.handleInboundNotification(raw) }}, err
	}
	notification, status := s.acceptSubscriptionNotification(raw)
	response, err := buildSIPRequestResponse(raw, status)
	result := inboundSIPResult{response: response}
	if status == 200 && notification.version != 0 {
		result.afterReply = func() { s.applySubscriptionNotificationBody(notification) }
		// Failure to write the response does not invalidate an accepted update.
		// Finish its queue entry as well; the transport still returns the error.
		result.onReplyError = result.afterReply
	}
	return result, err
}

func (s *Service) acceptSubscriptionNotification(raw string) (*subscriptionNotification, int) {
	n := &subscriptionNotification{raw: raw, mwi: sipEventPackage(raw) == mwiEventPackage}
	message, parseErr := parseSIPMessage(raw)
	request, isRequest := message.(*sip.Request)
	if parseErr != nil || !isRequest || request.Method != sip.NOTIFY || request.CSeq() == nil {
		return n, 400
	}
	state, err := parseSubscriptionNotifyState(raw)
	if err != nil {
		logging.WarnRate("ims-notify-state-invalid", "IMS subscription notification rejected", "err", err)
		return n, 400
	}
	seq := request.CSeq().SeqNo
	if request.CSeq().MethodName != sip.NOTIFY {
		return n, 400
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireSubscriptionTimersLocked(time.Now())
	f := s.subscriptionFieldsLocked(n.mwi)
	if !s.subscriptionNotificationMatchesLocked(f, raw) {
		return n, 481
	}
	if f.dialog.notifySeen && seq < f.dialog.remoteCSeq {
		return n, 500
	}
	if f.dialog.notifySeen && seq == f.dialog.remoteCSeq {
		return n, 200
	}
	if !f.dialog.notifySeen {
		f.dialog.routeSet = notificationRouteSet(request)
	}
	f.dialog.remoteCSeq = seq
	f.dialog.notifySeen = true
	f.dialog.remoteTag = sipAddressTag(rawSIPHeaderValue(raw, "From"))
	if target := firstSIPHeaderURI(rawSIPHeaderValue(raw, "Contact")); target != "" {
		f.dialog.remoteTarget = target
	}
	f.lifecycle.notifyDeadline = time.Time{}
	f.lifecycle.notifyVersion++
	if state.state == "terminated" {
		s.terminateNotifiedSubscriptionLocked(f, state, time.Now())
	} else {
		// Only a matching active or pending NOTIFY confirms the subscription;
		// a 200 to SUBSCRIBE or a REGISTER success does not (hideck#102).
		// While a failed attempt still waits for its retry, a NOTIFY on the
		// older, still-valid subscription confirms nothing new.
		eventPackage := subscriptionEventPackage(n.mwi)
		if f.lifecycle.retryAt.IsZero() {
			if failures := s.subscriptionRegistrations.subscriptionConfirmed(f.lifecycle.context.binding.identity, eventPackage, time.Now()); failures != 0 {
				logging.Info("IMS subscription confirmed; failure backoff cleared",
					"device", s.DeviceID(), "event_package", eventPackage, "failures", failures, "state", state.state)
			}
		}
		if state.expiresPresent {
			f.lifecycle.notifyExpires = true
			f.setExpiry(time.Now(), state.expires)
		}
	}
	n.context, n.version = s.subscriptionContextLocked(), f.lifecycle.notifyVersion
	if f.lifecycle.notifications == nil {
		f.lifecycle.notifications = &subscriptionNotificationQueue{}
	}
	n.queue = f.lifecycle.notifications
	n.queue.pending = append(n.queue.pending, n)
	s.signalIMSMaintenance()
	return n, 200
}

func (s *Service) subscriptionNotificationMatchesLocked(f subscriptionFields, raw string) bool {
	if s.stopped() || f.lifecycle.context != s.subscriptionContextLocked() {
		return false
	}
	if *f.closed && !(f.lifecycle.unsubscribing && !f.lifecycle.notifyDeadline.IsZero()) {
		return false
	}
	d := f.dialog
	if d.callID == "" || d.callID != strings.TrimSpace(rawSIPHeaderValue(raw, "Call-ID")) ||
		d.localTag == "" || d.localTag != sipAddressTag(rawSIPHeaderValue(raw, "To")) {
		return false
	}
	remoteTag := sipAddressTag(rawSIPHeaderValue(raw, "From"))
	firstNotify := f.lifecycle.initial && !d.notifySeen && !f.lifecycle.notifyDeadline.IsZero()
	if remoteTag == "" || (!firstNotify && d.remoteTag != "" && d.remoteTag != remoteTag) {
		return false
	}
	if !d.ready() && f.lifecycle.sentAt.IsZero() {
		return false
	}
	// Our SUBSCRIBEs do not send Event id. An id-bearing NOTIFY belongs to
	// another usage even when it happens to share the dialog identifiers.
	for _, parameter := range strings.Split(rawSIPHeaderValue(raw, "Event"), ";")[1:] {
		name, _, _ := strings.Cut(strings.TrimSpace(parameter), "=")
		if strings.EqualFold(name, "id") {
			return false
		}
	}
	return true
}

func notificationRouteSet(request *sip.Request) []string {
	var routes []string
	for _, header := range request.GetHeaders("Record-Route") {
		routes = append(routes, header.Value()) // UAS route order, unlike a response.
	}
	return routes
}

func (s *Service) notificationBodyCurrentLocked(n *subscriptionNotification) bool {
	f := s.subscriptionFieldsLocked(n.mwi)
	return !s.stopped() && n.context == s.subscriptionContextLocked() && n.queue == f.lifecycle.notifications
}
