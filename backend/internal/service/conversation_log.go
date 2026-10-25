package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	SettingKeyConversationLogging = "conversation_logging"
	ConversationTextLimit         = 1024 * 1024
	conversationQueueBytes        = 32 * 1024 * 1024
)

var ErrConversationNotFound = infraerrors.NotFound("CONVERSATION_NOT_FOUND", "Conversation content was not recorded or has expired")

type ConversationLogConfig struct {
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retention_days"` // 0 keeps content until explicitly changed.
}

type ConversationLog struct {
	ID               int64     `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	RequestID        string    `json:"request_id"`
	ClientRequestID  string    `json:"client_request_id"`
	UserID           int64     `json:"user_id"`
	APIKeyID         int64     `json:"api_key_id"`
	GroupID          *int64    `json:"group_id"`
	Model            string    `json:"model"`
	Endpoint         string    `json:"endpoint"`
	StatusCode       int       `json:"status_code"`
	Stream           bool      `json:"stream"`
	UserText         string    `json:"user_text"`
	AssistantText    string    `json:"assistant_text"`
	ErrorText        string    `json:"error_text"`
	InputTruncated   bool      `json:"input_truncated"`
	OutputTruncated  bool      `json:"output_truncated"`
	ResponseComplete bool      `json:"response_complete"`
}

type ConversationLogRepository interface {
	Insert(context.Context, *ConversationLog) error
	GetByUsageID(context.Context, int64) (*ConversationLog, error)
	GetByErrorID(context.Context, int64) (*ConversationLog, error)
	DeleteBefore(context.Context, time.Time, int) (int64, error)
}

type ConversationLogService struct {
	repo       ConversationLogRepository
	settings   SettingRepository
	config     atomic.Pointer[ConversationLogConfig]
	configMu   sync.Mutex
	queue      chan *ConversationLog
	queueBytes atomic.Int64
	dropped    atomic.Uint64
	failed     atomic.Uint64
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.RWMutex
	stopped    bool
	startOnce  sync.Once
}

func NewConversationLogService(repo ConversationLogRepository, settings SettingRepository) *ConversationLogService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &ConversationLogService{repo: repo, settings: settings, queue: make(chan *ConversationLog, 128), ctx: ctx, cancel: cancel}
	s.config.Store(&ConversationLogConfig{RetentionDays: 7})
	return s
}

func ProvideConversationLogService(repo ConversationLogRepository, settings SettingRepository) *ConversationLogService {
	s := NewConversationLogService(repo, settings)
	s.Start()
	return s
}

func (s *ConversationLogService) Start() {
	s.startOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.GetConfig(ctx)
		cancel()
		s.wg.Add(2)
		go s.runWriter()
		go s.runMaintenance()
	})
}

func (s *ConversationLogService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *ConversationLogService) Config() ConversationLogConfig {
	if s == nil {
		return ConversationLogConfig{RetentionDays: 7}
	}
	return *s.config.Load()
}

func (s *ConversationLogService) GetConfig(ctx context.Context) (ConversationLogConfig, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg := ConversationLogConfig{RetentionDays: 7}
	if s.settings != nil {
		raw, err := s.settings.GetValue(ctx, SettingKeyConversationLogging)
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			return s.Config(), err
		}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				return s.Config(), err
			}
		}
	}
	if cfg.RetentionDays < 0 || cfg.RetentionDays > 3650 {
		return s.Config(), fmt.Errorf("invalid conversation retention")
	}
	s.config.Store(&cfg)
	return cfg, nil
}

func (s *ConversationLogService) SetConfig(ctx context.Context, cfg ConversationLogConfig) error {
	if cfg.RetentionDays < 0 || cfg.RetentionDays > 3650 {
		return infraerrors.BadRequest("INVALID_RETENTION", "Retention must be between 0 and 3650 days")
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := s.settings.Set(ctx, SettingKeyConversationLogging, string(raw)); err != nil {
		return err
	}
	s.config.Store(&cfg)
	return nil
}

func (s *ConversationLogService) Stats() map[string]uint64 {
	return map[string]uint64{"dropped": s.dropped.Load(), "write_failures": s.failed.Load()}
}

// ConversationRequestID uses the same local identity as ordinary usage billing.
// It never relies on an upstream-generated response ID.
func ConversationRequestID(ctx context.Context) string { return resolveUsageBillingRequestID(ctx, "") }

// BoundConversationText keeps valid PostgreSQL UTF-8 text and never stores NUL.
func BoundConversationText(text string, limit int) (string, bool) {
	text = strings.ReplaceAll(strings.ToValidUTF8(text, "�"), "\x00", "�")
	if len(text) <= limit {
		return text, false
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return strings.Clone(text[:end]), true
}

func conversationBytes(entry *ConversationLog) int64 {
	return int64(len(entry.UserText) + len(entry.AssistantText) + len(entry.ErrorText) + 1024)
}

// Record only queues bounded text. Database latency/failure cannot delay a stream.
func (s *ConversationLogService) Record(entry *ConversationLog) {
	if s == nil || entry == nil || !s.Config().Enabled || entry.UserID <= 0 || entry.APIKeyID <= 0 {
		return
	}
	copy := *entry
	var cut bool
	copy.UserText, cut = BoundConversationText(copy.UserText, ConversationTextLimit)
	copy.InputTruncated = copy.InputTruncated || cut
	copy.AssistantText, cut = BoundConversationText(copy.AssistantText, ConversationTextLimit)
	copy.OutputTruncated = copy.OutputTruncated || cut
	copy.ErrorText, _ = BoundConversationText(copy.ErrorText, 4096)
	copy.Model, _ = BoundConversationText(copy.Model, 200)
	copy.Endpoint, _ = BoundConversationText(copy.Endpoint, 256)
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = time.Now().UTC()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stopped {
		return
	}
	size := conversationBytes(&copy)
	if s.queueBytes.Add(size) > conversationQueueBytes {
		s.queueBytes.Add(-size)
		s.dropped.Add(1)
		return
	}
	select {
	case s.queue <- &copy:
	default:
		s.queueBytes.Add(-size)
		s.dropped.Add(1)
	}
}

func (s *ConversationLogService) write(ctx context.Context, entry *ConversationLog) {
	defer s.queueBytes.Add(-conversationBytes(entry))
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.repo.Insert(writeCtx, entry); err != nil {
		s.failed.Add(1)
		// Do not write conversation text or SQL arguments to system logs.
		slog.Warn("conversation log write failed", "request_id", entry.RequestID)
	}
}

func (s *ConversationLogService) runWriter() {
	defer s.wg.Done()
	for {
		select {
		case entry := <-s.queue:
			// A graceful shutdown must not cancel an entry already dequeued.
			s.write(context.Background(), entry)
		case <-s.ctx.Done():
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			for {
				select {
				case entry := <-s.queue:
					s.write(ctx, entry)
				case <-ctx.Done():
					return
				default:
					return
				}
			}
		}
	}
}

func (s *ConversationLogService) runMaintenance() {
	defer s.wg.Done()
	refresh := time.NewTicker(30 * time.Second)
	cleanup := time.NewTicker(time.Hour)
	defer refresh.Stop()
	defer cleanup.Stop()
	s.cleanup()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-refresh.C:
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			_, _ = s.GetConfig(ctx)
			cancel()
		case <-cleanup.C:
			s.cleanup()
		}
	}
}

func (s *ConversationLogService) cleanup() {
	days := s.Config().RetentionDays
	if days <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	for ctx.Err() == nil {
		n, err := s.repo.DeleteBefore(ctx, cutoff, 1000)
		if err != nil || n < 1000 {
			return
		}
	}
}

func (s *ConversationLogService) GetByUsageID(ctx context.Context, id int64) (*ConversationLog, error) {
	return s.repo.GetByUsageID(ctx, id)
}
func (s *ConversationLogService) GetByErrorID(ctx context.Context, id int64) (*ConversationLog, error) {
	return s.repo.GetByErrorID(ctx, id)
}
