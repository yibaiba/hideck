package volte

import (
	"errors"
	"fmt"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

func (c *Controller) releaseHandoffSession(id string, s *session) error {
	// Drain registration refreshes and callbacks before invalidating their
	// generation. Neither external cleanup nor media I/O holds c.mu.
	s.events.Lock()
	defer s.events.Unlock()
	if err := c.host.ReleaseIMSClients(id); err != nil {
		return fmt.Errorf("volte: release IMS clients for handoff: %w", err)
	}
	c.mu.Lock()
	s.gen++
	s.imsHooked = false
	cancel := append([]func(){}, s.imsUnsubscribe...)
	s.imsUnsubscribe = nil
	if s.voice != nil {
		cancel = append(cancel, s.voice.unsubscribe)
		s.voice.unsubscribe = nil
		s.voice.attached = false
	}
	c.mu.Unlock()
	for _, unsubscribe := range cancel {
		if unsubscribe != nil {
			unsubscribe()
		}
	}
	if err := c.closeHandoffMedia(s); err != nil {
		return c.fail(id, fmt.Errorf("volte: release media for handoff: %w", err))
	}
	if s.voice != nil {
		// Physical idleness has already been confirmed. Publish final events
		// only after resources close successfully; failed cleanup can be retried.
		c.handleVoiceInfo(id, s.voice, &qmi.VoiceAllCallInfo{})
	}
	c.mu.Lock()
	delete(c.sess, id)
	c.mu.Unlock()
	return nil
}

func (c *Controller) closeHandoffMedia(s *session) error {
	if s.voice == nil {
		return nil
	}
	var errs []error
	for _, call := range s.voice.list() {
		if err := c.audio.Stop(call.ID); err != nil {
			errs = append(errs, err)
		}
		if media := c.media.get(call.ID); media != nil {
			if err := media.Close(); err != nil {
				errs = append(errs, err)
			} else {
				c.media.take(call.ID)
			}
		}
	}
	return errors.Join(errs...)
}
