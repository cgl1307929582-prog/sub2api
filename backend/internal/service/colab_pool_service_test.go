//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func colabTestAccount() *Account {
	return &Account{ID: 812, Platform: PlatformColab, Type: AccountTypeAPIKey, Status: StatusActive, Concurrency: 1,
		Credentials: map[string]any{"api_key": "pool_test_secret", "base_url": "https://pool.example/sub2api/v1", "api_protocol": APIProtocolAdaptive}}
}

func colabFixture() string {
	return `{"version":1,"model_id":"local-model","balance_cu":560.77,"burn_cu_per_hour":0,"used_cu_24h":18.51,"accounts":5,"eligible_accounts":4,"ready":0,"starting":0,"auto_start":true,"max_instances":2,"idle_stop_minutes":60,"observed_at":"` + time.Now().UTC().Format(time.RFC3339) + `","quota_updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `","instances":[{"id":24,"state":"stopped","gpu":"G4","running":0,"waiting":0}]}`
}

func expireColabCache(s *ColabPoolService, account *Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := colabPoolCacheKey(account)
	entry := s.cache[key]
	entry.attempted = time.Now().Add(-time.Hour)
	s.cache[key] = entry
}

func TestColabPoolQueriesAreReadOnlyAndCached(t *testing.T) {
	account := colabTestAccount()
	upstream := &httpUpstreamRecorder{resp: droidQuotaResponse(200, colabFixture())}
	svc := NewColabPoolService(nil, upstream, nil)
	got, err := svc.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Empty(t, got.Error)
	require.Equal(t, 560.77, *got.ColabPool.BalanceCU)
	require.Zero(t, *got.ColabPool.BurnCUPerHour)
	require.False(t, got.ColabPool.Stale)
	require.False(t, got.ColabPool.QuotaStale)
	require.Equal(t, "stopped", got.ColabPool.Instances[0].State)
	require.Equal(t, "GET", upstream.lastReq.Method)
	require.Equal(t, "https://pool.example/sub2api/v1/pool/status", upstream.lastReq.URL.String())
	require.Nil(t, upstream.lastReq.Body)
	require.Equal(t, "Bearer pool_test_secret", upstream.lastReq.Header.Get("Authorization"))
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.lastReq.Context()))
	*got.ColabPool.BalanceCU = 999
	cached, err := svc.GetUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, 560.77, *cached.ColabPool.BalanceCU)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "cache", cached.Source)
}

func TestColabPoolFailurePreservesSnapshotButKeyRotationDoesNot(t *testing.T) {
	account := colabTestAccount()
	upstream := &httpUpstreamRecorder{resp: droidQuotaResponse(200, colabFixture())}
	svc := NewColabPoolService(nil, upstream, nil)
	first, err := svc.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	expireColabCache(svc, account)
	upstream.resp = droidQuotaResponse(403, `{"secret":"pool_test_secret"}`)
	failed, err := svc.GetUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, "forbidden", failed.ErrorCode)
	require.True(t, failed.ColabPool.Stale)
	require.Equal(t, first.UpdatedAt, failed.UpdatedAt)
	body, err := json.Marshal(failed)
	require.NoError(t, err)
	require.NotContains(t, string(body), "pool_test_secret")
	require.Equal(t, StatusActive, account.Status)
	account.Credentials["api_key"] = "rotated_pool_key"
	upstream.resp = droidQuotaResponse(401, `{}`)
	rotated, err := svc.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Nil(t, rotated.ColabPool)
	require.Equal(t, "unauthenticated", rotated.ErrorCode)
}

func TestColabPoolRejectsMalformedSnapshotsAndHonorsPolicy(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `[]`, strings.Repeat("x", colabPoolMaxBytes+1), strings.Replace(colabFixture(), `"version":1`, `"version":2`, 1), strings.Replace(colabFixture(), `"balance_cu":560.77`, `"balance_cu":-1`, 1), strings.Replace(colabFixture(), `"ready":0`, `"ready":null`, 1)} {
		svc := NewColabPoolService(nil, &httpUpstreamRecorder{resp: droidQuotaResponse(200, body)}, nil)
		usage, err := svc.GetUsage(context.Background(), colabTestAccount(), false)
		require.NoError(t, err)
		require.Equal(t, "invalid_response", usage.ErrorCode)
		require.Nil(t, usage.ColabPool)
	}
	account := colabTestAccount()
	upstream := &httpUpstreamRecorder{}
	svc := NewColabPoolService(nil, upstream, cnProbeAllowlistConfig("different.example"))
	usage, err := svc.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Equal(t, "url_rejected", usage.ErrorCode)
	require.Empty(t, upstream.requests)
	id := int64(9)
	account.ProxyID = &id
	svc = NewColabPoolService(nil, upstream, nil)
	usage, err = svc.GetUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Equal(t, "proxy_unavailable", usage.ErrorCode)
	require.Empty(t, upstream.requests)
}

func TestColabPoolMissingBalanceAndOldSamplesStayUnknownOrStale(t *testing.T) {
	body := strings.Replace(colabFixture(), `"balance_cu":560.77`, `"balance_cu":null`, 1)
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	raw["observed_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	raw["quota_updated_at"] = nil
	encoded, err := json.Marshal(raw)
	require.NoError(t, err)
	svc := NewColabPoolService(nil, &httpUpstreamRecorder{resp: droidQuotaResponse(200, string(encoded))}, nil)
	usage, err := svc.GetUsage(context.Background(), colabTestAccount(), false)
	require.NoError(t, err)
	require.Nil(t, usage.ColabPool.BalanceCU)
	require.True(t, usage.ColabPool.Stale)
	require.True(t, usage.ColabPool.QuotaStale)
}

func TestColabAccountRequiresExplicitURLAndUsesChatForEveryInbound(t *testing.T) {
	account := colabTestAccount()
	require.NoError(t, validateColabAccount(account))
	for _, inbound := range []string{APIProtocolChatCompletions, APIProtocolResponses, APIProtocolAnthropic} {
		require.Equal(t, APIProtocolChatCompletions, resolveUpstreamProtocol(account, inbound, "local-model", nil))
	}
	require.Empty(t, defaultModelsListCandidateIDs(PlatformColab))
	delete(account.Credentials, "base_url")
	require.Empty(t, account.GetOpenAIBaseURL())
	require.Error(t, validateColabAccount(account))
	account = colabTestAccount()
	account.Type = AccountTypeOAuth
	require.Error(t, validateColabAccount(account))
	account = colabTestAccount()
	account.Credentials["api_protocol"] = APIProtocolAnthropic
	require.Error(t, validateColabAccount(account))
}

func TestColabModelSyncUsesPoolConfigurationOnly(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: droidQuotaResponse(200, `{"data":[{"id":"new-configured-model","object":"model","context_window":262144,"input_modalities":["text"]}]}`)}
	svc := &AccountTestService{httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), colabTestAccount())
	require.NoError(t, err)
	require.Equal(t, []string{"new-configured-model"}, catalog.Models)
	require.Len(t, upstream.requests, 1, "metadata must never wake a GPU or query an unrelated provider")
	require.Equal(t, "https://pool.example/sub2api/v1/models", upstream.lastReq.URL.String())
	require.Equal(t, "GET", upstream.lastReq.Method)
}

func TestColabNativeAccountTestDiscoversConfiguredModel(t *testing.T) {
	account := colabTestAccount()
	svc, upstream := adaptiveCNAccountTestService(account,
		droidQuotaResponse(200, `{"data":[{"id":"configured-model"}]}`), adaptiveCNChatTestResponse())
	c, recorder := newTestContext()
	err := svc.TestAccountConnection(c, account.ID, "", "hi", AccountTestModeDefault)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, "GET", upstream.requests[0].Method)
	require.Equal(t, "/sub2api/v1/models", upstream.requests[0].URL.Path)
	require.Equal(t, "/sub2api/v1/chat/completions", upstream.requests[1].URL.Path)
	require.Contains(t, string(upstream.lastBody), `"model":"configured-model"`)
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

func TestColabCatalogRefreshUpdatesPublicModelsAndSurvivesFailure(t *testing.T) {
	account := colabTestAccount()
	repo := &upstreamModelMetadataRepoStub{}
	upstream := &httpUpstreamRecorder{resp: droidQuotaResponse(200, `{"data":[{"id":"configured-v1"}]}`)}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, catalog, repo.updates[ColabModelCatalogExtraKey])
	require.Equal(t, []string{"configured-v1"}, account.ColabModelIDs())
	upstream.resp = droidQuotaResponse(200, `{"data":[{"id":"configured-v2"}]}`)
	_, err = svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	groupID := int64(4)
	gateway := &GatewayService{accountRepo: &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {*account}}}}
	require.Equal(t, []string{"configured-v2"}, gateway.GetAvailableModels(context.Background(), &groupID, PlatformColab))
	upstream.resp = droidQuotaResponse(503, `{}`)
	_, err = svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.Error(t, err)
	require.Equal(t, []string{"configured-v2"}, account.ColabModelIDs())
	account.Credentials["base_url"] = "https://different-pool.example/v1"
	require.Empty(t, account.ColabModelIDs(), "a different Pool cannot reuse this catalog")
}

func TestColabCatalogSurvivesStaleFormsAndCannotBypassMissingProxy(t *testing.T) {
	account := colabTestAccount()
	latest := &UpstreamModelCatalog{Source: buildV1ModelsURL(account.GetOpenAIBaseURL()), Models: []string{"current-model"}}
	account.Extra = map[string]any{ColabModelCatalogExtraKey: latest}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	admin := &adminServiceImpl{accountRepo: repo}
	updated, err := admin.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{
		Extra: map[string]any{ColabModelCatalogExtraKey: map[string]any{"models": []string{"stale"}}, "quota_limit": 20},
	})
	require.NoError(t, err)
	require.Equal(t, latest.Models, updated.GetColabModelCatalog().Models)
	proxyID := int64(9)
	account.ProxyID = &proxyID
	upstream := &httpUpstreamRecorder{}
	svc := &AccountTestService{httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	_, err = svc.FetchUpstreamSupportedModels(context.Background(), account)
	require.Error(t, err)
	require.Empty(t, upstream.requests)
	account.ProxyID = nil
	req, err := svc.buildUpstreamModelsRequest(context.Background(), account)
	require.NoError(t, err)
	require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
}
