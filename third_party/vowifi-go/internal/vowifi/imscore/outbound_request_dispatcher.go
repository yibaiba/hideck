package imscore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/iniwex5/vowifi-go/internal/vowifi/logging"
)

const (
	outboundRequestShardCount    = 16
	outboundRequestQueueCapacity = 128
	outboundQueueWarningDelay    = time.Second
)

var errOutboundRequestQueueFull = errors.New("smsip_outbound_queue_full: outbound request queue full")

func (s *Service) initOutboundRequestDispatchersLocked() {
	if s == nil || len(s.outboundReqShards) != 0 {
		return
	}
	s.outboundReqShards = make([]chan outboundRequestTask, outboundRequestShardCount)
	for shard := range s.outboundReqShards {
		queue := make(chan outboundRequestTask, outboundRequestQueueCapacity)
		s.outboundReqShards[shard] = queue
		s.networkDone.Add(1)
		go s.runOutboundRequestShard(shard, queue)
	}
	if s.outboundMsgCh == nil {
		s.outboundMsgCh = make(chan outboundMessageTask, outboundRequestQueueCapacity)
		s.networkDone.Add(1)
		go s.runOutboundMessageDispatcher()
	}
}

func (s *Service) ensureOutboundRequestDispatchers() []chan outboundRequestTask {
	if s == nil {
		return nil
	}
	s.outboundMu.Lock()
	s.initOutboundRequestDispatchersLocked()
	shards := append([]chan outboundRequestTask(nil), s.outboundReqShards...)
	s.outboundMu.Unlock()
	return shards
}

func (s *Service) runOutboundRequestShard(shard int, queue <-chan outboundRequestTask) {
	defer s.networkDone.Done()
	for {
		select {
		case <-s.stop:
			return
		case task, ok := <-queue:
			if !ok {
				return
			}
			s.runOutboundRequestTask(shard, task)
		}
	}
}

func (s *Service) runOutboundRequestTask(shard int, task outboundRequestTask) {
	if delay := time.Since(task.enqueuedAt); delay > outboundQueueWarningDelay {
		logging.WarnRate("smsip_outbound_queue_wait_high", "IMS outbound request queue delay is high",
			"shard", shard, "delay", delay, "dispatch_seq", task.dispatchSeq)
	}
	response, err := s.executeOutboundRequestWithCallbacks(outboundSendOperation{
		Context: task.ctx, Mode: task.modeCtx, Request: task.req,
		Timeout: time.Duration(task.timeout), Callbacks: task.callbacks,
	})
	reply := outboundRequestReply{res: response, err: err, dispatchSeq: task.dispatchSeq}
	if task.done == nil {
		return
	}
	select {
	case task.done <- reply:
	default:
	}
}

func (s *Service) executeOutboundRequest(
	ctx context.Context,
	req *sip.Request,
	modeCtx outboundModeContext,
	timeout time.Duration,
) (*sip.Response, error) {
	return s.executeOutboundRequestWithCallbacks(outboundSendOperation{
		Context: ctx, Mode: modeCtx, Request: req, Timeout: timeout,
	})
}

func (s *Service) executeOutboundRequestWithCallbacks(
	operation outboundSendOperation,
) (*sip.Response, error) {
	if operation.Request == nil {
		return nil, errors.New("imscore: nil outbound request")
	}
	if operation.Context == nil {
		operation.Context = context.Background()
	}
	if operation.Timeout > 0 {
		var cancel context.CancelFunc
		operation.Context, cancel = context.WithTimeout(operation.Context, operation.Timeout)
		defer cancel()
	}
	response, err := s.sendByMode(operation)
	if err != nil {
		s.handleOutboundRequestError(operation.Mode, operation.Request, err)
		return nil, err
	}
	if response == nil {
		return nil, errors.New("imscore: outbound request returned no SIP response")
	}
	if response.parsed == nil {
		return sip.NewResponse(response.StatusCode, response.Reason), nil
	}
	return response.parsed.Clone(), nil
}

func (s *Service) handleOutboundRequestError(mode outboundModeContext, req *sip.Request, err error) {
	if err == nil || mode.InboundPeer || !isFatalSIPTransportError(err) {
		return
	}
	// A policy block never reached the wire; its reason may quote a store
	// error that only looks like a transport failure.
	if blocked := (*mwiPolicyBlockedError)(nil); errors.As(err, &blocked) {
		return
	}
	method := "request"
	if req != nil && strings.TrimSpace(string(req.Method)) != "" {
		method = strings.TrimSpace(string(req.Method))
	}
	s.markSignalingDead(fmt.Errorf("outbound %s: %w", method, err))
}

func outboundDispatchKey(req *sip.Request, flow string) string {
	if req != nil && req.CallID() != nil {
		if callID := strings.TrimSpace(req.CallID().Value()); callID != "" {
			return "callid:" + callID
		}
	}
	method := "request"
	if req != nil && strings.TrimSpace(string(req.Method)) != "" {
		method = strings.ToUpper(strings.TrimSpace(string(req.Method)))
	}
	return "fallback:" + strings.TrimSpace(flow) + ":" + method
}

func outboundDispatchShardIndex(key string, shardCount int) int {
	if shardCount < 2 {
		return 0
	}
	const (
		fnvOffset32 = uint32(2166136261)
		fnvPrime32  = uint32(16777619)
	)
	hash := fnvOffset32
	for index := 0; index < len(key); index++ {
		hash = (hash ^ uint32(key[index])) * fnvPrime32
	}
	return int(hash % uint32(shardCount))
}

func (s *Service) dispatchOutboundRequest(
	ctx context.Context,
	flow string,
	req *sip.Request,
	timeout time.Duration,
	wait bool,
) (*sip.Response, uint64, error) {
	return s.dispatchOutboundRequestWithCallbacks(
		outboundDispatchOptions{Context: ctx, Flow: flow, Request: req, Timeout: timeout}, wait,
	)
}

func (s *Service) dispatchOutboundRequestWithCallbacks(
	options outboundDispatchOptions,
	wait bool,
) (*sip.Response, uint64, error) {
	if options.Request == nil {
		return nil, 0, errors.New("imscore: nil outbound request")
	}
	if options.Context == nil {
		options.Context = context.Background()
	}
	modeCtx, err := s.dispatchModeContext(options)
	if err != nil {
		return nil, 0, err
	}
	shards := s.ensureOutboundRequestDispatchers()
	if len(shards) == 0 {
		return nil, 0, errors.New("imscore: outbound request dispatcher unavailable")
	}
	seq := s.outboundDispatchSeq.Add(1)
	done := make(chan outboundRequestReply, 1)
	if !wait {
		done = nil
	}
	task := outboundRequestTask{
		ctx: options.Context, flow: options.Flow, req: options.Request.Clone(), timeout: int64(options.Timeout),
		modeCtx: modeCtx, callbacks: options.Callbacks,
		dispatchSeq: seq, enqueuedAt: time.Now(), done: done,
	}
	queue := shards[outboundDispatchShardIndex(outboundDispatchKey(options.Request, options.Flow), len(shards))]
	select {
	case <-options.Context.Done():
		return nil, seq, options.Context.Err()
	case <-s.stop:
		return nil, seq, errors.New("imscore: service stopped")
	case queue <- task:
	default:
		s.outboundQueueReject.Add(1)
		return nil, seq, errOutboundRequestQueueFull
	}
	if !wait {
		return nil, seq, nil
	}
	select {
	case reply := <-done:
		return reply.res, reply.dispatchSeq, reply.err
	case <-options.Context.Done():
		return nil, seq, options.Context.Err()
	case <-s.stop:
		return nil, seq, errors.New("imscore: service stopped")
	}
}

func (s *Service) dispatchModeContext(options outboundDispatchOptions) (outboundModeContext, error) {
	if options.Mode != nil {
		return *options.Mode, nil
	}
	return s.resolveOutboundModeContextForPeer(options.Flow, options.Request, options.PeerConn)
}

func (s *Service) buildOutboundRequest(req *sip.Request) (*sip.Request, error) {
	if req == nil {
		return nil, errors.New("imscore: nil outbound request")
	}
	if req.CallID() == nil || req.CSeq() == nil || req.Via() == nil {
		return nil, errors.New("imscore: outbound request is missing transaction headers")
	}
	return req.Clone(), nil
}
