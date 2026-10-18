package service

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

// DroidRetryBudget is a request-level budget, created once by the handler.
// Service classification never starts or extends a retry deadline.
const DroidRetryBudget = 3 * time.Second

// markDroidHTTPRetry preserves account state updates performed by the caller.
// In particular a 429 cooldown is not cleared: the handler must wait AND obtain
// fresh scheduling admission before replaying. A short Retry-After is metadata,
// not permission to ignore a cooldown or to replay after a client write.
func markDroidHTTPRetry(account *Account, failure *UpstreamFailoverError, shouldDisable bool, message string) {
	if account == nil || !account.IsDroid() || failure == nil {
		return
	}
	failure.RetryableOnSameAccount = false
	failure.SameAccountRetryMax = 0
	failure.SameAccountRetryDelay = 0
	failure.SameAccountRetryDeadline = time.Time{}
	if failure.Stage == GatewayFailureStageAccountAuth || failure.Reason == openAIRequestBodyTooLargeReason {
		return
	}
	if hit, _, _ := detectOpenAICyberPolicy(failure.ResponseBody); hit {
		return
	}
	if isOpenAIContextWindowError(message, failure.ResponseBody) {
		return
	}
	switch failure.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if shouldDisable {
			return
		}
		failure.RetryableOnSameAccount = true
		failure.SameAccountRetryMax = 2
		failure.SameAccountRetryDelay = 300 * time.Millisecond
	case http.StatusTooManyRequests:
		now := time.Now()
		reset := parseRetryAfterResetTime(failure.ResponseHeaders, now)
		if reset == nil || !reset.After(now) || reset.Sub(now) > DroidRetryBudget {
			return
		}
		failure.RetryableOnSameAccount = true
		failure.SameAccountRetryMax = 1
		failure.SameAccountRetryDelay = reset.Sub(now)
	}
}

// SelectDroidRetryAccount admits exactly targetAccountID, never a sticky-cache
// approximation. The handler uses the returned wait plan just like a normal
// selection and rechecks client commitment before Forward. No cooldown is waived.
func (s *OpenAIGatewayService) SelectDroidRetryAccount(ctx context.Context, groupID *int64, targetAccountID int64, requestedModel string, requireCompact bool, capability OpenAIEndpointCapability) (*AccountSelectionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIGroupPrivacyRequirement(ctx, groupID)
	ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		return nil, ErrNoAvailableAccounts
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, PlatformDroid)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].ID != targetAccountID || !accounts[i].IsDroid() {
			continue
		}
		account, _, _ := s.selectBestAccount(ctx, groupID, PlatformDroid, accounts[i:i+1], requestedModel, nil, requireCompact, capability, false)
		if account == nil || !s.openAIAccountMatchesSchedulingGroup(account, groupID) {
			return nil, ErrNoAvailableAccounts
		}
		result, acquireErr := s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if acquireErr == nil && result != nil && result.Acquired {
			return s.newAcquiredSelectionResult(ctx, account, result.ReleaseFunc)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cfg := s.schedulingConfig()
		return s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{AccountID: account.ID, MaxConcurrency: account.Concurrency, Timeout: cfg.FallbackWaitTimeout, MaxWaiting: cfg.FallbackMaxWaiting})
	}
	return nil, ErrNoAvailableAccounts
}

// IsSingleDroidRetryAccount must be called before the first Forward (a 429 may
// subsequently cool the selected account). Count actual eligible candidates,
// not pool_mode, account configuration count, or an exclusion set from failover.
func (s *OpenAIGatewayService) IsSingleDroidRetryAccount(ctx context.Context, groupID *int64, targetAccountID int64, requestedModel string, requireCompact bool, capability OpenAIEndpointCapability) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIGroupPrivacyRequirement(ctx, groupID)
	ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		return false, nil
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, PlatformDroid)
	if err != nil {
		return false, err
	}
	count := 0
	found := false
	for i := range accounts {
		account, _, _ := s.selectBestAccount(ctx, groupID, PlatformDroid, accounts[i:i+1], requestedModel, nil, requireCompact, capability, false)
		if account == nil || !account.IsDroid() || !s.openAIAccountMatchesSchedulingGroup(account, groupID) {
			continue
		}
		count++
		found = found || account.ID == targetAccountID
		if count > 1 {
			return false, nil
		}
	}
	return count == 1 && found, nil
}

// isDroidRecoverableBodyReadError deliberately allows only transient read
// failures, not arbitrary application errors. Sanitization retains this bit,
// never the underlying error (which can contain credentials or request URLs).
func isDroidRecoverableBodyReadError(err error) bool {
	if err == nil || errors.Is(err, ErrUpstreamResponseBodyTooLarge) || errors.Is(err, bufio.ErrTooLong) || errors.Is(err, errOpenAIFirstOutputScannerLimit) {
		return false
	}
	var safe *droidTransportError
	if errors.As(err, &safe) {
		return safe.bodyReadRetryable
	}
	if classifyUpstreamTransportError(err).Persistent {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

// droidBodyReadFailover returns metadata only; the handler owns replay, account
// admission and the original retry budget. Use the inbound request context, not
// an upstream child timeout, to distinguish client cancellation. Written also
// covers header-only Flush and heartbeat writes, not just semantic output.
func droidBodyReadFailover(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, err error) *UpstreamFailoverError {
	if account == nil || !account.IsDroid() || c == nil || c.Writer == nil || c.Writer.Written() {
		return nil
	}
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	if ctx == nil || ctx.Err() != nil || !isDroidRecoverableBodyReadError(err) {
		return nil
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return &UpstreamFailoverError{
		StatusCode: http.StatusBadGateway, ResponseBody: openAITransportFailoverBody,
		RetryableOnSameAccount: true, SameAccountRetryMax: 2,
		SameAccountRetryDelay: 300 * time.Millisecond,
	}
}
