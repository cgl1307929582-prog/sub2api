package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

type conversationSettingsStub struct {
	SettingRepository
	mu    sync.Mutex
	value string
}

func (s *conversationSettingsStub) GetValue(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value == "" {
		return "", ErrSettingNotFound
	}
	return s.value, nil
}
func (s *conversationSettingsStub) Set(_ context.Context, _, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
	return nil
}

type conversationRepoStub struct {
	ConversationLogRepository
	inserted chan *ConversationLog
	fail     bool
	cutoff   chan time.Time
}

func (r *conversationRepoStub) Insert(_ context.Context, e *ConversationLog) error {
	if r.fail {
		return errors.New("database unavailable")
	}
	r.inserted <- e
	return nil
}
func (r *conversationRepoStub) DeleteBefore(_ context.Context, t time.Time, _ int) (int64, error) {
	if r.cutoff != nil {
		select {
		case r.cutoff <- t:
		default:
		}
	}
	return 0, nil
}

func TestConversationConfigValidationAndDisabledDefault(t *testing.T) {
	s := NewConversationLogService(&conversationRepoStub{}, &conversationSettingsStub{})
	defer s.Stop()
	cfg, err := s.GetConfig(context.Background())
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
	require.Equal(t, 7, cfg.RetentionDays)
	require.Error(t, s.SetConfig(context.Background(), ConversationLogConfig{Enabled: true, RetentionDays: -1}))
	require.False(t, s.Config().Enabled)
	require.NoError(t, s.SetConfig(context.Background(), ConversationLogConfig{Enabled: true, RetentionDays: 30}))
	require.Equal(t, 30, s.Config().RetentionDays)
	require.NoError(t, s.SetConfig(context.Background(), ConversationLogConfig{RetentionDays: 0}))
	s.Record(&ConversationLog{UserID: 1, APIKeyID: 2, UserText: "must not enqueue"})
	require.Empty(t, s.queue)
}

func TestConversationAsyncWriterAndRetention(t *testing.T) {
	r := &conversationRepoStub{inserted: make(chan *ConversationLog, 1), cutoff: make(chan time.Time, 1)}
	s := NewConversationLogService(r, &conversationSettingsStub{value: `{"enabled":true,"retention_days":7}`})
	s.Start()
	defer s.Stop()
	entry := &ConversationLog{UserID: 1, APIKeyID: 2, RequestID: "client:test", UserText: "hello\x00world", AssistantText: "answer"}
	s.Record(entry)
	entry.UserText = "mutated later"
	select {
	case got := <-r.inserted:
		require.Equal(t, "hello�world", got.UserText)
	case <-time.After(time.Second):
		t.Fatal("record was not written")
	}
	select {
	case cutoff := <-r.cutoff:
		require.WithinDuration(t, time.Now().AddDate(0, 0, -7), cutoff, time.Minute)
	case <-time.After(time.Second):
		t.Fatal("retention was not run")
	}
}

func TestConversationQueueBoundsAndContentLimits(t *testing.T) {
	s := NewConversationLogService(&conversationRepoStub{}, &conversationSettingsStub{})
	defer s.Stop()
	require.NoError(t, s.SetConfig(context.Background(), ConversationLogConfig{Enabled: true, RetentionDays: 7}))
	text := strings.Repeat("文", ConversationTextLimit/3+100)
	for i := 0; i < 100; i++ {
		s.Record(&ConversationLog{UserID: 1, APIKeyID: 2, UserText: text, AssistantText: text})
	}
	require.Greater(t, s.Stats()["dropped"], uint64(0))
	require.LessOrEqual(t, s.queueBytes.Load(), int64(conversationQueueBytes))
	entry := <-s.queue
	require.True(t, entry.InputTruncated)
	require.True(t, entry.OutputTruncated)
	require.True(t, utf8.ValidString(entry.UserText))
}

func TestConversationWriteFailureIsObservable(t *testing.T) {
	s := NewConversationLogService(&conversationRepoStub{fail: true}, &conversationSettingsStub{value: `{"enabled":true,"retention_days":0}`})
	s.Start()
	defer s.Stop()
	s.Record(&ConversationLog{UserID: 1, APIKeyID: 2, UserText: "question"})
	require.Eventually(t, func() bool { return s.Stats()["write_failures"] == 1 }, time.Second, 10*time.Millisecond)
}
