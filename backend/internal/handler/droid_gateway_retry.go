package handler

import (
	"context"
	"math/rand"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// selectAccount uses explicit scheduler admission for a replay, never sticky
// routing. A cooled, disabled or profit-vetoed target is not replaced silently.
func (b *droidReplayBarrier) selectAccount(h *OpenAIGatewayHandler, ctx context.Context, groupID *int64, previousID, sessionHash, model string, excluded map[int64]struct{}, transport service.OpenAIUpstreamTransport, capability service.OpenAIEndpointCapability, compact, canMove, tokenCost bool, platform ...string) (*service.AccountSelectionResult, service.OpenAIAccountScheduleDecision, error) {
	if b.pending != 0 {
		if !b.deadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, b.deadline)
			defer cancel()
		}
		selection, err := h.gatewayService.SelectDroidRetryAccount(ctx, groupID, b.pending, model, compact, capability)
		return selection, service.OpenAIAccountScheduleDecision{}, err
	}
	selection, decision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(ctx, groupID, previousID, sessionHash, model, excluded, transport, capability, compact, canMove, tokenCost, platform...)
	if err == nil && selection != nil && selection.Account != nil && selection.Account.IsDroid() {
		if b.single == nil {
			b.single = make(map[int64]bool)
		}
		id := selection.Account.ID
		if _, known := b.single[id]; !known {
			b.single[id], _ = h.gatewayService.IsSingleDroidRetryAccount(ctx, groupID, id, model, compact, capability)
		}
	}
	return selection, decision, err
}

// retry owns all Droid replay timing. The deadline is initialized once for the
// request, and counts survive account switches. Neither HTTP 503 nor 429 can
// start a second budget. Return values are replay and terminal respectively.
func (b *droidReplayBarrier) retry(h *OpenAIGatewayHandler, c *gin.Context, account *service.Account, failure *service.UpstreamFailoverError, anthropic, streaming bool) (bool, bool) {
	if !account.IsDroid() {
		return false, false
	}
	b.pending = 0
	if failoverClientGone(c) || b.stop(h, c, anthropic) {
		return false, true
	}
	isRate := failure.StatusCode == http.StatusTooManyRequests
	if isRate && !b.single[account.ID] {
		return false, false // preserve multi-account 429 failover
	}
	if b.deadline.IsZero() {
		b.deadline = time.Now().Add(service.DroidRetryBudget)
		b.retries = make(map[int64]int)
		b.rateRetries = make(map[int64]int)
	}
	count := b.retries[account.ID]
	allowed := failure.RetryableOnSameAccount && count < 2
	delay := 300 * time.Millisecond
	if count > 0 {
		delay = 800 * time.Millisecond
	}
	delay += time.Duration(rand.Int63n(int64(40 * time.Millisecond)))
	if isRate {
		allowed = allowed && b.rateRetries[account.ID] == 0
		delay = failure.SameAccountRetryDelay + 20*time.Millisecond
	}
	if allowed && time.Now().Add(delay).Before(b.deadline) {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-c.Request.Context().Done():
			return false, true
		case <-timer.C:
		}
		if failoverClientGone(c) || b.stop(h, c, anthropic) {
			return false, true
		}
		b.retries[account.ID]++
		if isRate {
			b.rateRetries[account.ID]++
		}
		b.pending = account.ID
		return true, false
	}
	if isRate {
		copyFailoverRetryAfter(c, failure.ResponseHeaders)
		if anthropic {
			h.anthropicStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded", streaming)
		} else {
			h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded", streaming)
		}
		return false, true
	}
	return false, false
}

// droidResponseCommitted is a request-level replay barrier, not a per-attempt
// byte delta. Header-only Flush and queue heartbeats also close the replay
// window. Stop the compact writer before inspecting Gin's writer so a heartbeat
// cannot race this check (or commit between the check and a subsequent replay).
// Call only for failed attempts/replays: a queue heartbeat must not prevent the
// first upstream attempt. It is safe to call again immediately before replay.
func droidResponseCommitted(c *gin.Context, account *service.Account) bool {
	if account == nil || !account.IsDroid() || c == nil || c.Writer == nil {
		return false
	}
	compactCommitted := service.StopOpenAICompactSSEKeepaliveCommitted(c)
	return compactCommitted || c.Writer.Written() || service.IsResponseCommitted(c)
}

// stopDroidCommittedReplay preserves the entry point's existing in-stream
// error contract without changing an already committed HTTP status. A service
// terminal response is authoritative; do not append another terminal event.
func (h *OpenAIGatewayHandler) stopDroidCommittedReplay(c *gin.Context, account *service.Account, failure *service.UpstreamFailoverError, anthropic bool) bool {
	if !droidResponseCommitted(c, account) {
		return false
	}
	if !failoverClientGone(c) && !service.IsResponseCommitted(c) {
		if anthropic {
			h.handleAnthropicFailoverExhausted(c, failure, true)
		} else {
			h.handleFailoverExhausted(c, failure, true)
		}
	}
	return true
}

// droidReplayBarrier remembers an actual failed upstream attempt, independently
// of scheduler exclusions. Recheck after backoff AND after concurrency admission
// since either can commit output. The zero value deliberately permits the first
// send even if admission already flushed a queue heartbeat.
type droidReplayBarrier struct {
	account     *service.Account
	failure     *service.UpstreamFailoverError
	pending     int64
	deadline    time.Time
	retries     map[int64]int
	rateRetries map[int64]int
	single      map[int64]bool
}

func (b *droidReplayBarrier) remember(account *service.Account, failure *service.UpstreamFailoverError) {
	if account != nil && account.IsDroid() {
		b.account, b.failure = account, failure
	}
}

func (b *droidReplayBarrier) stop(h *OpenAIGatewayHandler, c *gin.Context, anthropic bool) bool {
	if b.failure == nil {
		return false
	}
	if failoverClientGone(c) || h.stopDroidCommittedReplay(c, b.account, b.failure, anthropic) {
		return true
	}
	if b.pending != 0 && !b.deadline.IsZero() && !time.Now().Before(b.deadline) {
		if anthropic {
			h.handleAnthropicFailoverExhausted(c, b.failure, c.Writer.Written())
		} else {
			h.handleFailoverExhausted(c, b.failure, c.Writer.Written())
		}
		return true
	}
	return false
}

// A rejected pinned replay is terminal: it must not silently choose a new
// account or lose the original 429 contract in generic capacity mapping.
func (b *droidReplayBarrier) selectionFailed(h *OpenAIGatewayHandler, c *gin.Context, anthropic, streaming bool) bool {
	if b.pending == 0 || b.failure == nil {
		return false
	}
	if failoverClientGone(c) || b.stop(h, c, anthropic) {
		return true
	}
	if b.failure.StatusCode == http.StatusTooManyRequests {
		copyFailoverRetryAfter(c, b.failure.ResponseHeaders)
		if anthropic {
			h.anthropicStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded", streaming)
		} else {
			h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded", streaming)
		}
	} else if anthropic {
		h.handleAnthropicFailoverExhausted(c, b.failure, streaming)
	} else {
		h.handleFailoverExhausted(c, b.failure, streaming)
	}
	return true
}

// acquireSlot scopes the replay deadline to admission only. In particular it
// never replaces the request context used by Forward or the slot release lease.
// Once a Droid failure has started the request-level budget, every later
// admission wait is bounded by it, including an ordinary account switch after
// same-account retries are exhausted; only a pinned replay uses the pinned
// terminal contract. A failover that never started the budget (for example a
// multi-account 401/429 switch) keeps the scheduler's normal wait plan.
func (b *droidReplayBarrier) acquireSlot(h *OpenAIGatewayHandler, c *gin.Context, groupID *int64, sessionHash string, selection *service.AccountSelectionResult, streaming bool, started *bool, log *zap.Logger, anthropic bool) (func(), openAISlotAcquireResult) {
	if b.deadline.IsZero() || selection == nil || selection.Account == nil || !selection.Account.IsDroid() {
		return h.acquireResponsesAccountSlot(c, groupID, sessionHash, selection, streaming, started, log)
	}
	if b.pending == 0 {
		return h.acquireOpenAIAccountSlot(c, groupID, sessionHash, selection, streaming, started, log, nil, b)
	}
	writeError := func(status int, errType, code, message string) {
		b.selectionFailed(h, c, anthropic, *started)
	}
	return h.acquireOpenAIAccountSlot(c, groupID, sessionHash, selection, streaming, started, log, writeError, b)
}
