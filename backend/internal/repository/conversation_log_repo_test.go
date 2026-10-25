package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestConversationLookupRequiresMatchingUserAndKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	r := NewConversationLogRepository(db)
	query := conversationSelect + `JOIN usage_logs u ON c.request_id=u.request_id AND c.api_key_id=u.api_key_id AND c.user_id=u.user_id WHERE u.id=$1 LIMIT 1`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, err = r.GetByUsageID(context.Background(), 42)
	require.ErrorIs(t, err, service.ErrConversationNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestConversationCleanupDoesNotTouchBilling(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	r := NewConversationLogRepository(db)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	query := `DELETE FROM conversation_logs WHERE id IN (SELECT id FROM conversation_logs WHERE created_at<$1 ORDER BY created_at,id LIMIT $2)`
	mock.ExpectExec(regexp.QuoteMeta(query)).WithArgs(cutoff, 1000).WillReturnResult(sqlmock.NewResult(0, 25))
	n, err := r.DeleteBefore(context.Background(), cutoff, 1000)
	require.NoError(t, err)
	require.Equal(t, int64(25), n)
	require.NoError(t, mock.ExpectationsWereMet())
}
