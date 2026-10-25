package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type conversationLogRepository struct{ db *sql.DB }

func NewConversationLogRepository(db *sql.DB) service.ConversationLogRepository {
	return &conversationLogRepository{db: db}
}

func (r *conversationLogRepository) Insert(ctx context.Context, e *service.ConversationLog) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO conversation_logs
(created_at,request_id,client_request_id,user_id,api_key_id,group_id,model,endpoint,status_code,stream,user_text,assistant_text,error_text,input_truncated,output_truncated,response_complete)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT (request_id,api_key_id) DO NOTHING`,
		e.CreatedAt, e.RequestID, e.ClientRequestID, e.UserID, e.APIKeyID, e.GroupID, e.Model, e.Endpoint, e.StatusCode, e.Stream, e.UserText, e.AssistantText, e.ErrorText, e.InputTruncated, e.OutputTruncated, e.ResponseComplete)
	return err
}

const conversationSelect = `SELECT c.id,c.created_at,c.request_id,c.client_request_id,c.user_id,c.api_key_id,c.group_id,c.model,c.endpoint,c.status_code,c.stream,c.user_text,c.assistant_text,c.error_text,c.input_truncated,c.output_truncated,c.response_complete FROM conversation_logs c `

func (r *conversationLogRepository) get(ctx context.Context, query string, id int64) (*service.ConversationLog, error) {
	e := new(service.ConversationLog)
	err := r.db.QueryRowContext(ctx, query, id).Scan(&e.ID, &e.CreatedAt, &e.RequestID, &e.ClientRequestID, &e.UserID, &e.APIKeyID, &e.GroupID, &e.Model, &e.Endpoint, &e.StatusCode, &e.Stream, &e.UserText, &e.AssistantText, &e.ErrorText, &e.InputTruncated, &e.OutputTruncated, &e.ResponseComplete)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrConversationNotFound
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (r *conversationLogRepository) GetByUsageID(ctx context.Context, id int64) (*service.ConversationLog, error) {
	return r.get(ctx, conversationSelect+`JOIN usage_logs u ON c.request_id=u.request_id AND c.api_key_id=u.api_key_id AND c.user_id=u.user_id WHERE u.id=$1 LIMIT 1`, id)
}

func (r *conversationLogRepository) GetByErrorID(ctx context.Context, id int64) (*service.ConversationLog, error) {
	return r.get(ctx, conversationSelect+`JOIN ops_error_logs e ON (c.request_id=e.request_id OR c.request_id='client:'||e.client_request_id) AND c.api_key_id=e.api_key_id AND c.user_id=e.user_id WHERE e.id=$1 LIMIT 1`, id)
}

func (r *conversationLogRepository) DeleteBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	result, err := r.db.ExecContext(ctx, `DELETE FROM conversation_logs WHERE id IN (SELECT id FROM conversation_logs WHERE created_at<$1 ORDER BY created_at,id LIMIT $2)`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
