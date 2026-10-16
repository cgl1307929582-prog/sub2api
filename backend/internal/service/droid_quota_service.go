package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"golang.org/x/sync/singleflight"
)

const (
	droidQuotaCacheTTL    = 3 * time.Minute
	droidQuotaErrorTTL    = time.Minute
	droidQuotaRefreshGap  = 5 * time.Second
	droidQuotaTimeout     = 20 * time.Second
	droidQuotaMaxAccounts = 1024
)

// These are independent rate-limit windows, not a dollar-denominated balance.
// Pointers distinguish a missing observation from a genuine zero.
type DroidQuotaWindow struct {
	UsedPercent *float64   `json:"used_percent"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type DroidQuotaPool struct {
	FiveHour *DroidQuotaWindow `json:"five_hour"`
	Weekly   *DroidQuotaWindow `json:"weekly"`
	Monthly  *DroidQuotaWindow `json:"monthly"`
}

type DroidQuotaInfo struct {
	Standard                   *DroidQuotaPool `json:"standard"`
	Core                       *DroidQuotaPool `json:"core"`
	ExtraUsageBalanceCents     *int64          `json:"extra_usage_balance_cents"`
	ExtraUsageAllowed          *bool           `json:"extra_usage_allowed"`
	UsesTokenRateLimitsBilling *bool           `json:"uses_token_rate_limits_billing"`
	FetchedAt                  time.Time       `json:"fetched_at"`
	Stale                      bool            `json:"stale"`
}

type droidLimitsWindow struct {
	UsedPercent *float64   `json:"usedPercent"`
	WindowEnd   *time.Time `json:"windowEnd"`
}

type droidLimitsPool struct {
	FiveHour *droidLimitsWindow `json:"fiveHour"`
	Weekly   *droidLimitsWindow `json:"weekly"`
	Monthly  *droidLimitsWindow `json:"monthly"`
}

type droidLimitsResponse struct {
	Limits *struct {
		Standard *droidLimitsPool `json:"standard"`
		Core     *droidLimitsPool `json:"core"`
	} `json:"limits"`
	ExtraUsageBalanceCents     *int64 `json:"extraUsageBalanceCents"`
	ExtraUsageAllowed          *bool  `json:"extraUsageAllowed"`
	UsesTokenRateLimitsBilling *bool  `json:"usesTokenRateLimitsBilling"`
}

type droidQuotaCacheEntry struct {
	snapshot  *DroidQuotaInfo
	attempted time.Time
	errorCode string
}

// DroidQuotaService only reads billing limits. It never changes overage settings,
// credits, account scheduling, or locally billed token usage.
type DroidQuotaService struct {
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	flight       singleflight.Group
	mu           sync.Mutex
	cache        map[string]droidQuotaCacheEntry
}

func NewDroidQuotaService(proxyRepo ProxyRepository, httpUpstream HTTPUpstream, cfg *config.Config) *DroidQuotaService {
	return &DroidQuotaService{proxyRepo: proxyRepo, httpUpstream: httpUpstream, cfg: cfg, cache: make(map[string]droidQuotaCacheEntry)}
}

func (s *DroidQuotaService) GetUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "DROID_QUOTA_NOT_CONFIGURED", "Factory quota service is unavailable")
	}
	if !account.IsDroid() {
		return nil, infraerrors.BadRequest("INVALID_FACTORY_ACCOUNT", "Factory quota requires a Droid account")
	}
	if err := validateDroidAccount(account); err != nil {
		return nil, err
	}
	key := droidQuotaCacheKey(account)
	if cached, ok := s.cached(key, force); ok {
		return droidQuotaUsage(cached, "cache"), nil
	}
	result := s.flight.DoChan(key, func() (any, error) {
		if cached, ok := s.cached(key, force); ok {
			return cached, nil
		}
		// One cancelled viewer must not cancel other callers sharing this query.
		queryCtx, cancel := context.WithTimeout(context.Background(), droidQuotaTimeout)
		defer cancel()
		snapshot, code := s.fetch(queryCtx, account)
		entry := droidQuotaCacheEntry{snapshot: snapshot, attempted: time.Now(), errorCode: code}
		s.mu.Lock()
		defer s.mu.Unlock()
		if snapshot == nil {
			entry.snapshot = s.cache[key].snapshot
		}
		if _, exists := s.cache[key]; !exists && len(s.cache) >= droidQuotaMaxAccounts {
			for old := range s.cache {
				delete(s.cache, old)
				break
			}
		}
		s.cache[key] = entry
		return entry, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case shared := <-result:
		if shared.Err != nil {
			return nil, shared.Err
		}
		entry, ok := shared.Val.(droidQuotaCacheEntry)
		if !ok {
			return nil, infraerrors.New(http.StatusInternalServerError, "DROID_QUOTA_RESULT_INVALID", "Factory quota result is unavailable")
		}
		return droidQuotaUsage(entry, "active"), nil
	}
}

func droidQuotaCacheKey(account *Account) string {
	proxy := ""
	if account.ProxyID != nil {
		proxy = fmt.Sprint(*account.ProxyID)
		if account.Proxy != nil {
			proxy += ":" + account.Proxy.URL()
		}
	}
	// Never reuse another credential's quota after an account is edited.
	sum := sha256.Sum256([]byte(strings.Join([]string{account.GetCredential("api_key"), account.GetOpenAIBaseURL(), account.GetCredential("user_agent"), proxy}, "\x00")))
	return fmt.Sprintf("%d:%x", account.ID, sum)
}

func (s *DroidQuotaService) cached(key string, force bool) (droidQuotaCacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok {
		return entry, false
	}
	ttl := droidQuotaCacheTTL
	if entry.errorCode != "" {
		ttl = droidQuotaErrorTTL
	}
	if force {
		ttl = droidQuotaRefreshGap
	}
	return entry, time.Since(entry.attempted) < ttl
}

func droidQuotaUsage(entry droidQuotaCacheEntry, source string) *UsageInfo {
	usage := &UsageInfo{Source: source, ErrorCode: entry.errorCode}
	if entry.snapshot != nil {
		// Callers own their response; cached data remains immutable.
		data, _ := json.Marshal(entry.snapshot)
		var snapshot DroidQuotaInfo
		_ = json.Unmarshal(data, &snapshot)
		snapshot.Stale = entry.errorCode != "" && entry.errorCode != "quota_unknown"
		usage.DroidQuota = &snapshot
		usage.UpdatedAt = &snapshot.FetchedAt
	}
	if entry.errorCode != "" {
		// Do not echo upstream bodies, headers, URLs, credentials or transport errors.
		usage.Error = "Factory quota query failed"
		if entry.errorCode == "quota_unknown" {
			usage.Error = "Factory returned no quota or balance data"
		}
	}
	return usage
}

func (s *DroidQuotaService) fetch(ctx context.Context, account *Account) (*DroidQuotaInfo, string) {
	base, err := url.Parse(account.GetOpenAIBaseURL())
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, "invalid_base_url"
	}
	// The model endpoints include /api/llm/{o,a}; billing is rooted at the
	// same configured origin, outside those model-protocol paths.
	base.Path = "/api/billing/limits"
	base.RawPath = ""
	// Reuse the existing quota probe policy, including operator host allowlists.
	target, err := cnValidateProbeURL(s.cfg, base.String())
	if err != nil {
		return nil, "url_rejected"
	}
	proxyURL := ""
	if account.ProxyID != nil {
		proxy := account.Proxy
		if proxy == nil && s.proxyRepo != nil {
			proxy, err = s.proxyRepo.GetByID(ctx, *account.ProxyID)
		}
		if err != nil || proxy == nil {
			return nil, "proxy_unavailable"
		}
		proxyURL = proxy.URL()
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), http.MethodGet, target, nil)
	if err != nil {
		return nil, "invalid_base_url"
	}
	req.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	req.Header.Set("Accept", "application/json")
	if err := applyDroidClientHeaders(req.Header, account); err != nil {
		return nil, "invalid_client_headers"
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil || resp == nil || resp.Body == nil {
		return nil, "network_error"
	}
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return nil, "unauthenticated"
		case http.StatusForbidden:
			return nil, "forbidden"
		case http.StatusTooManyRequests:
			return nil, "rate_limited"
		default:
			return nil, "upstream_error"
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, droidErrorJSONMaxBytes+1))
	if err != nil || len(body) > droidErrorJSONMaxBytes {
		return nil, "invalid_response"
	}
	var raw *droidLimitsResponse
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return nil, "invalid_response"
	}
	snapshot := &DroidQuotaInfo{
		ExtraUsageBalanceCents: raw.ExtraUsageBalanceCents, ExtraUsageAllowed: raw.ExtraUsageAllowed,
		UsesTokenRateLimitsBilling: raw.UsesTokenRateLimitsBilling, FetchedAt: time.Now().UTC(),
	}
	if raw.Limits != nil {
		snapshot.Standard = convertDroidQuotaPool(raw.Limits.Standard)
		snapshot.Core = convertDroidQuotaPool(raw.Limits.Core)
	}
	if snapshot.Standard == nil && snapshot.Core == nil && snapshot.ExtraUsageBalanceCents == nil {
		return snapshot, "quota_unknown"
	}
	return snapshot, ""
}

func convertDroidQuotaPool(raw *droidLimitsPool) *DroidQuotaPool {
	if raw == nil {
		return nil
	}
	convert := func(window *droidLimitsWindow) *DroidQuotaWindow {
		if window == nil {
			return nil
		}
		used := window.UsedPercent
		if used != nil && *used < 0 {
			used = nil
		}
		return &DroidQuotaWindow{UsedPercent: used, ResetsAt: window.WindowEnd}
	}
	return &DroidQuotaPool{FiveHour: convert(raw.FiveHour), Weekly: convert(raw.Weekly), Monthly: convert(raw.Monthly)}
}
