package service

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	DroidModelCatalogURL      = "https://docs.factory.com/models.md"
	DroidModelCatalogExtraKey = "droid_model_catalog"
	droidModelCatalogLimit    = 1 << 20
)

var (
	droidCatalogIDPattern = regexp.MustCompile("^`([a-z0-9][a-z0-9._-]{1,127})`$")
	droidCatalogFootnote  = regexp.MustCompile(`(?i)<sup>.*?</sup>`)
	droidCatalogMarkup    = regexp.MustCompile(`<[^>]*>`)
	droidCatalogEffort    = regexp.MustCompile("`([a-z]+)`(\\s*\\(default\\))?")
)

// Factory does not expose a standard /models endpoint. Its public catalog is
// fetched without credentials; it is not an account entitlement check.
func (s *AccountTestService) buildDroidModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	if account == nil || !account.IsDroid() {
		return nil, newUpstreamModelSyncConfigError("A Droid account is required", nil)
	}
	if err := validateDroidAccount(account); err != nil {
		return nil, newUpstreamModelSyncConfigError("A valid Factory API key account is required", nil)
	}
	if account.ProxyID != nil && account.Proxy == nil {
		return nil, newUpstreamModelSyncConfigError("Factory account proxy is unavailable", nil)
	}
	if _, err := s.validateUpstreamBaseURL(DroidModelCatalogURL); err != nil {
		return nil, newUpstreamModelSyncConfigError("Factory model catalog host is not allowed by the server URL policy", nil)
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), http.MethodGet, DroidModelCatalogURL, nil)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Cannot create Factory model catalog request", nil)
	}
	req.Header.Set("Accept", "text/markdown,text/plain;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "Sub2API-Factory-Model-Catalog")
	return req, nil
}

func (s *AccountTestService) fetchDroidModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, newUpstreamModelSyncConfigError("Upstream HTTP client is not configured", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := s.buildDroidModelsRequest(ctx, account)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpUpstream.Do(req, upstreamModelsProxyURL(account), account.ID, maxInt(account.Concurrency, 1))
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil || resp == nil || resp.Body == nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to fetch Factory official model catalog", nil)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamModelSyncError{Kind: UpstreamModelSyncErrorUpstream, StatusCode: resp.StatusCode,
			Message: fmt.Sprintf("Factory official model catalog returned HTTP %d", resp.StatusCode)}
	}
	limit := min(int64(droidModelCatalogLimit), resolveModelsListReadLimit(s.cfg))
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, newUpstreamModelSyncUpstreamError("Factory model catalog response is unreadable or too large", nil)
	}
	catalog, err := parseDroidModelCatalog(string(body))
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Factory model catalog format changed or contained no supported models; previous catalog was kept", nil)
	}
	catalog.Source = DroidModelCatalogURL
	catalog.SyncedAt = time.Now().UTC().Format(time.RFC3339)
	catalog.Warnings = []UpstreamModelSyncWarning{{Code: "factory_public_catalog",
		Message: "Models come from Factory's public catalog. Account permissions and availability must be verified with an account test."}}
	if len(catalog.UnsupportedModels) > 0 {
		catalog.Warnings = append(catalog.Warnings, UpstreamModelSyncWarning{Code: "factory_unsupported_protocol",
			Message: fmt.Sprintf("Excluded %d models whose Factory protocols are not implemented by this adapter.", len(catalog.UnsupportedModels))})
	}
	return catalog, nil
}

func (s *AccountTestService) syncDroidModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	catalog, err := s.fetchDroidModelCatalog(ctx, account)
	if err != nil {
		return nil, err
	}
	if account.ID > 0 && s.accountRepo != nil {
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{DroidModelCatalogExtraKey: catalog}); err != nil {
			return nil, newUpstreamModelSyncInternalError("Failed to save Factory model catalog", nil)
		}
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		account.Extra[DroidModelCatalogExtraKey] = catalog
	}
	return catalog, nil
}

func parseDroidModelCatalog(markdown string) (*UpstreamModelCatalog, error) {
	catalog := &UpstreamModelCatalog{Metadata: make(map[string]UpstreamModelMetadata)}
	modelColumn, idColumn, reasoningColumn := -1, -1, -1
	for _, line := range strings.Split(markdown, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			modelColumn, idColumn, reasoningColumn = -1, -1, -1
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if slices.Contains(cells, "Model ID") && slices.Contains(cells, "Model") {
			modelColumn = slices.Index(cells, "Model")
			idColumn = slices.Index(cells, "Model ID")
			reasoningColumn = slices.Index(cells, "Reasoning")
			continue
		}
		if idColumn < 0 || modelColumn < 0 || idColumn >= len(cells) || modelColumn >= len(cells) {
			continue
		}
		match := droidCatalogIDPattern.FindStringSubmatch(cells[idColumn])
		if match == nil {
			continue
		}
		id := match[1]
		protocol, _, _ := droidModelRoute(id)
		if protocol == "" || strings.HasPrefix(id, "gpt-image-") {
			catalog.UnsupportedModels = append(catalog.UnsupportedModels, id)
			continue
		}
		name := droidCatalogFootnote.ReplaceAllString(cells[modelColumn], "")
		name = html.UnescapeString(droidCatalogMarkup.ReplaceAllString(name, ""))
		name = strings.Trim(strings.TrimSpace(name), "`*†‡ ")
		if name == "" || len(name) > 256 {
			name = id
		}
		entry := UpstreamModelMetadata{ID: id, DisplayName: name}
		if reasoningColumn >= 0 && reasoningColumn < len(cells) {
			for _, effort := range droidCatalogEffort.FindAllStringSubmatch(cells[reasoningColumn], -1) {
				if !slices.Contains([]string{"off", "none", "minimal", "low", "medium", "high", "xhigh", "max"}, effort[1]) {
					continue
				}
				if !slices.Contains(entry.SupportedReasoningLevels, effort[1]) {
					entry.SupportedReasoningLevels = append(entry.SupportedReasoningLevels, effort[1])
				}
				if effort[2] != "" {
					entry.DefaultReasoningLevel = effort[1]
				}
			}
		}
		catalog.Metadata[id] = entry
		if len(catalog.Metadata) > 512 {
			return nil, fmt.Errorf("too many Factory models")
		}
	}
	if len(catalog.Metadata) == 0 {
		return nil, fmt.Errorf("no supported Factory models")
	}
	for id := range catalog.Metadata {
		catalog.Models = append(catalog.Models, id)
	}
	slices.Sort(catalog.Models)
	catalog.UnsupportedModels = dedupeAndSortModelIDs(catalog.UnsupportedModels)
	return catalog, nil
}

func (a *Account) GetDroidModelCatalog() *UpstreamModelCatalog {
	if !a.IsDroid() || a.Extra == nil {
		return nil
	}
	body, err := json.Marshal(a.Extra[DroidModelCatalogExtraKey])
	if err != nil {
		return nil
	}
	var catalog UpstreamModelCatalog
	if json.Unmarshal(body, &catalog) != nil || catalog.Source != DroidModelCatalogURL || len(catalog.Models) == 0 {
		return nil
	}
	return &catalog
}

// DroidModelIDs applies the administrator's mappings to the saved catalog.
// Explicit aliases and old entries are retained; syncing never edits mappings.
func (a *Account) DroidModelIDs() []string {
	ids := DefaultDroidModelIDs()
	if catalog := a.GetDroidModelCatalog(); catalog != nil {
		ids = catalog.Models
	}
	if mapping := a.GetModelMapping(); len(mapping) > 0 {
		filtered := make([]string, 0, len(ids)+len(mapping))
		for _, id := range ids {
			if a.IsModelSupported(id) {
				filtered = append(filtered, id)
			}
		}
		for id := range mapping {
			if !strings.Contains(id, "*") {
				filtered = append(filtered, id)
			}
		}
		ids = filtered
	}
	return dedupeAndSortModelIDs(ids)
}
