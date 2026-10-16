package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const ColabModelCatalogExtraKey = "colab_model_catalog"

func (s *AccountTestService) syncColabModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	if err := validateColabAccount(account); err != nil {
		return nil, err
	}
	ids, body, err := s.fetchUpstreamModelList(ctx, account)
	if err != nil {
		return nil, err
	}
	_, metadata, err := extractUpstreamModelCatalog(body, false)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Invalid Pool model catalog", nil)
	}
	catalog := &UpstreamModelCatalog{Models: ids, Metadata: metadata, Source: buildV1ModelsURL(account.GetOpenAIBaseURL()), SyncedAt: time.Now().UTC().Format(time.RFC3339)}
	// Configured models can be discovered without a live GPU. Capability fields
	// remain partial unless explicitly supplied; do not enrich a private model by name.
	catalog.Warnings = []UpstreamModelSyncWarning{{Code: "colab_config_catalog", Message: "Models reflect Pool configuration. GPU readiness is shown separately in Pool status."}}
	if account.ID > 0 && s.accountRepo != nil {
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{ColabModelCatalogExtraKey: catalog}); err != nil {
			return nil, newUpstreamModelSyncInternalError("Failed to save Pool model catalog", nil)
		}
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		account.Extra[ColabModelCatalogExtraKey] = catalog
	}
	return catalog, nil
}

func (a *Account) GetColabModelCatalog() *UpstreamModelCatalog {
	if !a.IsColab() {
		return nil
	}
	body, err := json.Marshal(a.Extra[ColabModelCatalogExtraKey])
	if err != nil {
		return nil
	}
	var catalog UpstreamModelCatalog
	if json.Unmarshal(body, &catalog) != nil || catalog.Source != buildV1ModelsURL(a.GetOpenAIBaseURL()) || len(catalog.Models) == 0 {
		return nil
	}
	return &catalog
}

func (a *Account) ColabModelIDs() []string {
	var ids []string
	if catalog := a.GetColabModelCatalog(); catalog != nil {
		for _, id := range catalog.Models {
			if a.IsModelSupported(id) {
				ids = append(ids, id)
			}
		}
	}
	for id := range a.GetModelMapping() {
		if !strings.Contains(id, "*") {
			ids = append(ids, id)
		}
	}
	return dedupeAndSortModelIDs(ids)
}
