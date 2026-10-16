//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const droidQuotaFixture = `{"usesTokenRateLimitsBilling":true,"limits":{"standard":{"fiveHour":{"usedPercent":12,"windowEnd":"2026-11-01T12:00:00Z"},"weekly":{"usedPercent":25,"windowEnd":"2026-11-07T12:00:00Z"},"monthly":{"usedPercent":30,"windowEnd":"2026-11-30T12:00:00Z"}},"core":{"fiveHour":{"usedPercent":0,"windowEnd":null},"weekly":null,"monthly":{"usedPercent":1,"windowEnd":"2026-11-30T12:00:00Z"}}},"extraUsageBalanceCents":0,"extraUsageAllowed":true}`

func droidQuotaResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func expireDroidQuotaCache(s *DroidQuotaService, account *Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := droidQuotaCacheKey(account)
	entry := s.cache[key]
	entry.attempted = time.Now().Add(-droidQuotaCacheTTL - time.Second)
	s.cache[key] = entry
}

func TestDroidQuotaUsageAndCache(t *testing.T) {
	account := droidTestAccount()
	upstream := &httpUpstreamRecorder{resp: droidQuotaResponse(200, droidQuotaFixture)}
	s := NewDroidQuotaService(nil, upstream, nil)
	usage, err := s.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
	require.Equal(t, "active", usage.Source)
	require.Equal(t, float64(12), *usage.DroidQuota.Standard.FiveHour.UsedPercent)
	require.Equal(t, float64(0), *usage.DroidQuota.Core.FiveHour.UsedPercent)
	require.Nil(t, usage.DroidQuota.Core.FiveHour.ResetsAt)
	require.Nil(t, usage.DroidQuota.Core.Weekly)
	require.Equal(t, int64(0), *usage.DroidQuota.ExtraUsageBalanceCents)
	require.Equal(t, "2026-11-30T12:00:00Z", usage.DroidQuota.Standard.Monthly.ResetsAt.Format(time.RFC3339))
	require.Equal(t, http.MethodGet, upstream.lastReq.Method)
	require.Equal(t, "https://api.factory.ai/api/billing/limits", upstream.lastReq.URL.String())
	require.Nil(t, upstream.lastReq.Body)
	require.Equal(t, "Bearer factory_test_secret", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "factory-cli/"+droidClientVersion, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, droidClientVersion, upstream.lastReq.Header.Get("X-Client-Version"))
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.lastReq.Context()))
	*usage.DroidQuota.Standard.FiveHour.UsedPercent = 999
	cached, err := s.GetUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, "cache", cached.Source)
	require.Equal(t, float64(12), *cached.DroidQuota.Standard.FiveHour.UsedPercent)
	require.Len(t, upstream.requests, 1, "rapid forced queries should share the short cooldown")

	expireDroidQuotaCache(s, account)
	upstream.resp = droidQuotaResponse(200, strings.Replace(droidQuotaFixture, `"extraUsageBalanceCents":0`, `"extraUsageBalanceCents":1234`, 1))
	fresh, err := s.GetUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, int64(1234), *fresh.DroidQuota.ExtraUsageBalanceCents)
	require.Len(t, upstream.requests, 2)
}

func TestDroidQuotaErrorsPreserveLastSnapshotWithoutCredentialEcho(t *testing.T) {
	for status, code := range map[int]string{401: "unauthenticated", 403: "forbidden", 429: "rate_limited", 500: "upstream_error", 302: "upstream_error"} {
		t.Run(code+http.StatusText(status), func(t *testing.T) {
			account := droidTestAccount()
			upstream := &httpUpstreamRecorder{resp: droidQuotaResponse(200, droidQuotaFixture)}
			s := NewDroidQuotaService(nil, upstream, nil)
			original, err := s.GetUsage(context.Background(), account, false)
			require.NoError(t, err)
			expireDroidQuotaCache(s, account)
			upstream.resp = droidQuotaResponse(status, `{"message":"factory_test_secret"}`)
			failed, err := s.GetUsage(context.Background(), account, true)
			require.NoError(t, err)
			require.Equal(t, code, failed.ErrorCode)
			require.True(t, failed.DroidQuota.Stale)
			require.Equal(t, original.UpdatedAt, failed.UpdatedAt)
			encoded, err := json.Marshal(failed)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "factory_test_secret")
			_, err = s.GetUsage(context.Background(), account, false)
			require.NoError(t, err)
			require.Len(t, upstream.requests, 2, "failed refreshes must be cached")
			require.Equal(t, StatusActive, account.Status, "quota access errors do not disable inference")

			expireDroidQuotaCache(s, account)
			upstream.resp = droidQuotaResponse(200, `{}`)
			unknown, err := s.GetUsage(context.Background(), account, true)
			require.NoError(t, err)
			require.Equal(t, "quota_unknown", unknown.ErrorCode)
			require.False(t, unknown.DroidQuota.Stale)
			require.Nil(t, unknown.DroidQuota.Standard, "a successful empty response clears old limits")
			require.Nil(t, unknown.DroidQuota.ExtraUsageBalanceCents, "unknown is not zero or an old balance")
		})
	}
}

func TestDroidQuotaKeyRotationAndInvalidResponses(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{"limits":`, strings.Repeat("x", droidErrorJSONMaxBytes+1)} {
		account := droidTestAccount()
		upstream := &httpUpstreamRecorder{resp: droidQuotaResponse(200, droidQuotaFixture)}
		s := NewDroidQuotaService(nil, upstream, nil)
		_, err := s.GetUsage(context.Background(), account, false)
		require.NoError(t, err)
		account.Credentials["api_key"] = "factory_rotated_secret"
		upstream.resp = droidQuotaResponse(200, body)
		usage, err := s.GetUsage(context.Background(), account, false)
		require.NoError(t, err)
		require.Equal(t, "invalid_response", usage.ErrorCode)
		require.Nil(t, usage.DroidQuota, "never show the old credential's quota")
		require.Len(t, upstream.requests, 2)
	}
}

func TestDroidQuotaPolicyAndProxyFailClosed(t *testing.T) {
	account := droidTestAccount()
	upstream := &httpUpstreamRecorder{}
	s := NewDroidQuotaService(nil, upstream, cnProbeAllowlistConfig("api.kimi.com"))
	usage, err := s.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Equal(t, "url_rejected", usage.ErrorCode)
	require.Empty(t, upstream.requests)

	proxyID := int64(9)
	account.ProxyID = &proxyID
	s = NewDroidQuotaService(nil, upstream, nil)
	usage, err = s.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Equal(t, "proxy_unavailable", usage.ErrorCode)
	require.Empty(t, upstream.requests, "missing proxy must not silently send directly")

	account.Proxy = &Proxy{Protocol: "socks5", Host: "127.0.0.1", Port: 12345}
	upstream.resp = droidQuotaResponse(200, droidQuotaFixture)
	usage, err = s.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
	require.Equal(t, "socks5://127.0.0.1:12345", upstream.lastProxyURL)
}

type droidQuotaBlockingUpstream struct {
	HTTPUpstream
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (u *droidQuotaBlockingUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	if u.calls.Add(1) == 1 {
		close(u.started)
	}
	<-u.release
	return droidQuotaResponse(200, droidQuotaFixture), nil
}

func TestDroidQuotaSharedQuerySurvivesViewerCancellation(t *testing.T) {
	u := &droidQuotaBlockingUpstream{started: make(chan struct{}), release: make(chan struct{})}
	s := NewDroidQuotaService(nil, u, nil)
	account := droidTestAccount()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := s.GetUsage(ctx, account, false); first <- err }()
	select {
	case <-u.started:
	case err := <-first:
		t.Fatalf("query completed before reaching upstream: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("query never reached upstream")
	}
	cancel()
	require.ErrorIs(t, <-first, context.Canceled)
	close(u.release)
	usage, err := s.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.NotNil(t, usage.DroidQuota)
	require.Equal(t, int32(1), u.calls.Load())
}

func TestDroidQuotaNetworkErrorsAreSanitized(t *testing.T) {
	u := &httpUpstreamRecorder{err: errors.New("failed to send factory_test_secret to private.proxy")}
	s := NewDroidQuotaService(nil, u, nil)
	usage, err := s.GetUsage(context.Background(), droidTestAccount(), false)
	require.NoError(t, err)
	require.Equal(t, "network_error", usage.ErrorCode)
	require.Equal(t, "Factory quota query failed", usage.Error)
	require.Nil(t, usage.DroidQuota)
}

func TestDroidQuotaSingleAndBatchAccountUsageRoutes(t *testing.T) {
	account := droidTestAccount()
	u := &httpUpstreamRecorder{resp: droidQuotaResponse(200, droidQuotaFixture)}
	repo := &stubOpenAIAccountRepo{accounts: []Account{*account}}
	s := &AccountUsageService{accountRepo: repo, droidQuotaService: NewDroidQuotaService(nil, u, nil)}
	single, err := s.GetUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, single.DroidQuota)
	batch, failures, err := s.GetUsageBatch(context.Background(), []int64{account.ID, account.ID}, false)
	require.NoError(t, err)
	require.Empty(t, failures)
	require.Len(t, batch, 1)
	require.Equal(t, single.DroidQuota, batch[account.ID].DroidQuota)
	require.Len(t, u.requests, 1)
}
