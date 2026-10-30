package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDroidCatalogAccountUpdateKeepsLockedDatabaseSnapshot(t *testing.T) {
	for _, platform := range []string{service.PlatformDroid, service.PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			client, mock := newOllamaCloudUsageRepositoryTestClient(t)
			account := &service.Account{ID: 2, Platform: platform, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "factory_test_secret"},
				Extra:       map[string]any{service.DroidModelCatalogExtraKey: map[string]any{"models": []string{"claude-stale"}}, "custom": true}}
			credentials, err := json.Marshal(account.Credentials)
			require.NoError(t, err)
			mock.ExpectQuery(`(?s)SELECT.*extra -> 'droid_model_catalog'.*FOR NO KEY UPDATE`).
				WithArgs(account.ID, platform, account.Type, string(credentials), nil).
				WillReturnRows(sqlmock.NewRows(openCodeGoMergeMockColumns()).AddRow(
					false, false, false, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil,
					`{"source":"https://docs.factory.com/models.md","models":["claude-latest"]}`))
			extra, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
			require.NoError(t, err)
			require.Equal(t, true, extra["custom"])
			account.Extra = extra
			if platform == service.PlatformDroid {
				require.Equal(t, []string{"claude-latest"}, account.GetDroidModelCatalog().Models)
			} else {
				require.NotContains(t, extra, service.DroidModelCatalogExtraKey)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
