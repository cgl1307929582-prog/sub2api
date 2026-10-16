package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func factoryCatalogFixture() string {
	return "# Models\n\nUnrelated prose: `claude-not-a-catalog-entry`\n\n" +
		"| Reasoning | Model ID | Model | Multiplier |\n| --- | --- | --- | --- |\n" +
		"| `low`, `high` (default) | `claude-future-test` | Claude Future<sup>*</sup> | 1 |\n" +
		"| `medium` | `gpt-future-test` | GPT Future | 1 |\n" +
		"| | `glm-future-test` | GLM Future | 1 |\n" +
		"| | `kimi-future-test` | Kimi Future | 1 |\n" +
		"| | `minimax-future-test` | MiniMax Future | 1 |\n" +
		"| | `gemini-future-test` | Gemini Future | 1 |\n" +
		"| | `gpt-image-future-test` | Image Future | 1 |\n" +
		"| | `grok-future-test` | Grok Future | 1 |\n" +
		"| | `grok-future-test` | Duplicate | 1 |\n"
}

func factoryCatalogAccount() *Account {
	return &Account{ID: 801, Platform: PlatformDroid, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "factory_test_secret", "base_url": "https://unrelated.example/v1",
			"model_mapping": map[string]any{"legacy-alias": "claude-old"}},
		Extra: map[string]any{"unrelated": true}}
}

func factoryCatalogResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

func TestDroidModelCatalogParsesNewIDsWithoutHardcodedVersions(t *testing.T) {
	catalog, err := parseDroidModelCatalog(factoryCatalogFixture())
	require.NoError(t, err)
	require.Equal(t, []string{"claude-future-test", "gemini-future-test", "glm-future-test", "gpt-future-test", "grok-future-test", "kimi-future-test", "minimax-future-test"}, catalog.Models)
	require.Equal(t, []string{"gpt-image-future-test"}, catalog.UnsupportedModels)
	entry := catalog.Metadata["claude-future-test"]
	require.Equal(t, "Claude Future", entry.DisplayName)
	require.Equal(t, []string{"low", "high"}, entry.SupportedReasoningLevels)
	require.Equal(t, "high", entry.DefaultReasoningLevel)
	require.Zero(t, entry.ContextWindow)
	require.Zero(t, entry.MaxOutputTokens)
	for _, body := range []string{"<html>error</html>", "`claude-only-prose`", "| Model | Model ID |\n| Unknown | `unsupported-family` |"} {
		_, err := parseDroidModelCatalog(body)
		require.Error(t, err)
	}
}

func TestDroidModelCatalogSyncRefreshesAndPreservesMappings(t *testing.T) {
	account := factoryCatalogAccount()
	proxyID := int64(12)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{Protocol: "http", Host: "proxy.example", Port: 8080}
	account.Credentials["header_overrides"] = map[string]any{"Authorization": "secret-header", "Cookie": "secret-cookie"}
	before, err := json.Marshal(account.Credentials)
	require.NoError(t, err)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		factoryCatalogResponse(factoryCatalogFixture()),
		factoryCatalogResponse(strings.ReplaceAll(factoryCatalogFixture(), "gpt-future-test", "gpt-next-release")),
	}}
	repo := &upstreamModelMetadataRepoStub{}
	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig(), httpUpstream: upstream, accountRepo: repo}
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, DroidModelCatalogURL, catalog.Source)
	_, err = time.Parse(time.RFC3339, catalog.SyncedAt)
	require.NoError(t, err)
	require.Equal(t, account.ID, repo.accountID)
	require.Len(t, repo.updates, 1)
	require.Equal(t, catalog, repo.updates[DroidModelCatalogExtraKey])
	require.Equal(t, true, account.Extra["unrelated"])
	require.Equal(t, "http://proxy.example:8080", upstream.lastProxyURL)
	req := upstream.lastReq
	require.Equal(t, DroidModelCatalogURL, req.URL.String())
	require.Equal(t, http.MethodGet, req.Method)
	require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
	deadline, ok := req.Context().Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(20*time.Second), deadline, 2*time.Second)
	for _, header := range []string{"Authorization", "X-API-Key", "Cookie", "X-API-Provider"} {
		require.Empty(t, req.Header.Get(header))
	}
	require.Equal(t, "factory_public_catalog", catalog.Warnings[0].Code)

	catalog, err = svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 2)
	require.Contains(t, catalog.Models, "gpt-next-release")
	require.NotContains(t, catalog.Models, "gpt-future-test")
	require.Equal(t, catalog.Models, account.GetDroidModelCatalog().Models)
	after, err := json.Marshal(account.Credentials)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
}

func TestDroidModelCatalogFailuresKeepLastSnapshot(t *testing.T) {
	for _, kind := range []string{"http", "network", "format", "size", "database", "missing_proxy", "url_policy"} {
		t.Run(kind, func(t *testing.T) {
			account := factoryCatalogAccount()
			old := &UpstreamModelCatalog{Source: DroidModelCatalogURL, Models: []string{"claude-old"}, SyncedAt: "2026-01-01T00:00:00Z"}
			account.Extra[DroidModelCatalogExtraKey] = old
			upstream := &httpUpstreamRecorder{resp: factoryCatalogResponse(factoryCatalogFixture())}
			repo := &upstreamModelMetadataRepoStub{}
			cfg := upstreamModelSyncTestConfig()
			switch kind {
			case "http":
				upstream.resp = factoryCatalogResponse("factory_test_secret")
				upstream.resp.StatusCode = http.StatusForbidden
			case "network":
				upstream.err = errors.New("factory_test_secret")
			case "format":
				upstream.resp = factoryCatalogResponse("<html>factory_test_secret</html>")
			case "size":
				cfg.Gateway.ModelsListReadMaxBytes = 16
			case "database":
				repo.err = errors.New("factory_test_secret")
			case "missing_proxy":
				id := int64(9)
				account.ProxyID = &id
			case "url_policy":
				cfg.Security.URLAllowlist.Enabled = true
			}
			svc := &AccountTestService{cfg: cfg, httpUpstream: upstream, accountRepo: repo}
			_, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "factory_test_secret")
			require.Same(t, old, account.Extra[DroidModelCatalogExtraKey])
			if kind != "database" {
				require.Nil(t, repo.updates)
			}
			if kind == "missing_proxy" || kind == "url_policy" {
				require.Empty(t, upstream.requests)
			}
		})
	}
}

func TestDroidModelCatalogPreviewDoesNotPersist(t *testing.T) {
	account := factoryCatalogAccount()
	account.ID = 0
	repo := &upstreamModelMetadataRepoStub{}
	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig(), accountRepo: repo,
		httpUpstream: &httpUpstreamRecorder{resp: factoryCatalogResponse(factoryCatalogFixture())}}
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Contains(t, catalog.Models, "claude-future-test")
	require.Nil(t, repo.updates)
	require.Nil(t, account.GetDroidModelCatalog())
}

func TestDroidModelCatalogSurvivesSavingStaleAccountForm(t *testing.T) {
	account := factoryCatalogAccount()
	latest := &UpstreamModelCatalog{Source: DroidModelCatalogURL, Models: []string{"claude-next-release"}}
	account.Extra[DroidModelCatalogExtraKey] = latest
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{
		Extra: map[string]any{DroidModelCatalogExtraKey: map[string]any{"models": []string{"claude-stale"}}, "quota_limit": 20},
	})
	require.NoError(t, err)
	require.Equal(t, latest.Models, updated.GetDroidModelCatalog().Models)
	require.Equal(t, 20, updated.Extra["quota_limit"])
}

func TestDroidModelIDsUsesCatalogWithAdministratorRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mapping map[string]any
		want    []string
	}{
		{"unrestricted", nil, []string{"claude-future-test", "gpt-future-test"}},
		{"explicit", map[string]any{"legacy-alias": "claude-future-test", "gpt-future-test": "gpt-future-test"}, []string{"gpt-future-test", "legacy-alias"}},
		{"wildcard", map[string]any{"claude-*": "claude-future-test"}, []string{"claude-future-test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := factoryCatalogAccount()
			account.Credentials["model_mapping"] = tc.mapping
			account.Extra[DroidModelCatalogExtraKey] = &UpstreamModelCatalog{Source: DroidModelCatalogURL, Models: []string{"claude-future-test", "gpt-future-test"}}
			require.Equal(t, tc.want, account.DroidModelIDs())
			groupID := int64(3)
			repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {*account}}}
			svc := &GatewayService{accountRepo: repo}
			require.Equal(t, tc.want, svc.GetAvailableModels(context.Background(), &groupID, PlatformDroid))
		})
	}
}
