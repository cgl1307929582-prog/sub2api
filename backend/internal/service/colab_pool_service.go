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
	colabPoolMaxBytes    = 256 * 1024
	colabPoolCacheTTL    = 30 * time.Second
	colabPoolErrorTTL    = 15 * time.Second
	colabPoolRefreshGap  = 5 * time.Second
	colabPoolTimeout     = 20 * time.Second
	colabPoolMaxAccounts = 1024
)

// Compute Units are upstream GPU credits, independent of downstream token billing.
// Missing numeric observations remain null rather than becoming a false zero.
type ColabPoolInstance struct {
	ID              int64      `json:"id"`
	State           string     `json:"state"`
	GPU             string     `json:"gpu"`
	Running         int        `json:"running"`
	Waiting         int        `json:"waiting"`
	TokensPerSecond *float64   `json:"tokens_per_second"`
	CreatedAt       *time.Time `json:"created_at"`
	LastActiveAt    *time.Time `json:"last_active_at"`
}

type ColabPoolInfo struct {
	Version          int                 `json:"version"`
	ModelID          string              `json:"model_id"`
	BalanceCU        *float64            `json:"balance_cu"`
	BurnCUPerHour    *float64            `json:"burn_cu_per_hour"`
	UsedCU24h        *float64            `json:"used_cu_24h"`
	Accounts         int                 `json:"accounts"`
	EligibleAccounts int                 `json:"eligible_accounts"`
	Ready            int                 `json:"ready"`
	Starting         int                 `json:"starting"`
	AutoStart        bool                `json:"auto_start"`
	MaxInstances     int                 `json:"max_instances"`
	IdleStopMinutes  int                 `json:"idle_stop_minutes"`
	ObservedAt       time.Time           `json:"observed_at"`
	QuotaUpdatedAt   *time.Time          `json:"quota_updated_at"`
	QuotaStale       bool                `json:"quota_stale"`
	Instances        []ColabPoolInstance `json:"instances"`
	FetchedAt        time.Time           `json:"fetched_at"`
	Stale            bool                `json:"stale"`
}

type colabPoolCacheEntry struct {
	snapshot  *ColabPoolInfo
	attempted time.Time
	errorCode string
}

// ColabPoolService reads the bridge status endpoint only. It cannot create, stop,
// or wake GPU instances, and never changes local token billing or scheduling.
type ColabPoolService struct {
	proxyRepo    ProxyRepository
	httpUpstream HTTPUpstream
	cfg          *config.Config
	flight       singleflight.Group
	mu           sync.Mutex
	cache        map[string]colabPoolCacheEntry
}

func NewColabPoolService(proxyRepo ProxyRepository, httpUpstream HTTPUpstream, cfg *config.Config) *ColabPoolService {
	return &ColabPoolService{proxyRepo: proxyRepo, httpUpstream: httpUpstream, cfg: cfg, cache: make(map[string]colabPoolCacheEntry)}
}

func (s *ColabPoolService) GetUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "COLAB_POOL_NOT_CONFIGURED", "Colab Pool quota service is unavailable")
	}
	if !account.IsColab() {
		return nil, infraerrors.BadRequest("INVALID_COLAB_ACCOUNT", "Colab Pool quota requires a Colab account")
	}
	if err := validateColabAccount(account); err != nil {
		return nil, err
	}
	key := colabPoolCacheKey(account)
	if cached, ok := s.cached(key, force); ok {
		return colabPoolUsage(cached, "cache"), nil
	}
	result := s.flight.DoChan(key, func() (any, error) {
		if cached, ok := s.cached(key, force); ok {
			return cached, nil
		}
		// One cancelled viewer must not cancel other callers sharing this query.
		queryCtx, cancel := context.WithTimeout(context.Background(), colabPoolTimeout)
		defer cancel()
		snapshot, code := s.fetch(queryCtx, account)
		entry := colabPoolCacheEntry{snapshot: snapshot, attempted: time.Now(), errorCode: code}
		s.mu.Lock()
		defer s.mu.Unlock()
		if snapshot == nil {
			entry.snapshot = s.cache[key].snapshot
		}
		if _, exists := s.cache[key]; !exists && len(s.cache) >= colabPoolMaxAccounts {
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
		entry, ok := shared.Val.(colabPoolCacheEntry)
		if !ok {
			return nil, infraerrors.New(http.StatusInternalServerError, "COLAB_POOL_RESULT_INVALID", "Colab Pool quota result is unavailable")
		}
		return colabPoolUsage(entry, "active"), nil
	}
}

func colabPoolCacheKey(account *Account) string {
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

func (s *ColabPoolService) cached(key string, force bool) (colabPoolCacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok {
		return entry, false
	}
	ttl := colabPoolCacheTTL
	if entry.errorCode != "" {
		ttl = colabPoolErrorTTL
	}
	if force {
		ttl = colabPoolRefreshGap
	}
	return entry, time.Since(entry.attempted) < ttl
}

func colabPoolUsage(entry colabPoolCacheEntry, source string) *UsageInfo {
	usage := &UsageInfo{Source: source, ErrorCode: entry.errorCode}
	if entry.snapshot != nil {
		// Callers own their response; cached data remains immutable.
		data, _ := json.Marshal(entry.snapshot)
		var snapshot ColabPoolInfo
		_ = json.Unmarshal(data, &snapshot)
		snapshot.Stale = snapshot.Stale || entry.errorCode != "" || time.Since(snapshot.ObservedAt) > 2*time.Minute
		snapshot.QuotaStale = snapshot.QuotaStale || snapshot.QuotaUpdatedAt == nil || time.Since(*snapshot.QuotaUpdatedAt) > 10*time.Minute
		usage.ColabPool = &snapshot
		usage.UpdatedAt = &snapshot.FetchedAt
	}
	if entry.errorCode != "" {
		// Do not echo upstream bodies, headers, URLs, credentials or transport errors.
		usage.Error = "Colab Pool quota query failed"
	}
	return usage
}

func (s *ColabPoolService) fetch(ctx context.Context, account *Account) (*ColabPoolInfo, string) {
	base, err := url.Parse(account.GetOpenAIBaseURL())
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, "invalid_base_url"
	}
	// Keep a deployment's path prefix (e.g. /sub2api/v1) on the same origin.
	base.Path = strings.TrimRight(base.Path, "/")
	if !strings.HasSuffix(base.Path, "/v1") {
		base.Path += "/v1"
	}
	base.Path += "/pool/status"
	base.RawPath = ""
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, colabPoolMaxBytes+1))
	if err != nil || len(body) > colabPoolMaxBytes {
		return nil, "invalid_response"
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil, "invalid_response"
	}
	for _, name := range []string{"accounts", "eligible_accounts", "ready", "starting", "auto_start", "max_instances", "idle_stop_minutes", "instances"} {
		if value, ok := fields[name]; !ok || string(value) == "null" {
			return nil, "invalid_response"
		}
	}
	var snapshot *ColabPoolInfo
	if json.Unmarshal(body, &snapshot) != nil || snapshot == nil || snapshot.Version != 1 || snapshot.ObservedAt.IsZero() || strings.TrimSpace(snapshot.ModelID) == "" {
		return nil, "invalid_response"
	}
	if snapshot.Accounts < 0 || snapshot.Ready < 0 || snapshot.Starting < 0 || snapshot.EligibleAccounts < 0 || len(snapshot.Instances) > 100 {
		return nil, "invalid_response"
	}
	for _, number := range []*float64{snapshot.BalanceCU, snapshot.BurnCUPerHour, snapshot.UsedCU24h} {
		if number != nil && *number < 0 {
			return nil, "invalid_response"
		}
	}
	snapshot.FetchedAt = time.Now().UTC()
	// The bridge reports the Pool sample time separately from this HTTP fetch.
	snapshot.Stale = snapshot.Stale || time.Since(snapshot.ObservedAt) > 2*time.Minute
	return snapshot, ""
}
