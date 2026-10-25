package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestConversationWSTurnsUseTheirOwnUsageIdentity(t *testing.T) {
	r := &conversationTestRecorder{enabled: true}
	c := newConversationWSCapture(r, &service.APIKey{ID: 2, UserID: 1}, context.Background(), "/v1/responses")
	c.start(1, []byte(`{"input":"first question"}`), "gpt-test")
	c.response(1, []byte(`{"type":"response.output_text.delta","delta":"first answer"}`))
	c.response(1, []byte(`{"type":"response.completed","response":{"status":"completed"}}`))
	// The next accepted frame can arrive before the previous usage callback finishes.
	c.start(2, []byte(`{"input":"second question"}`), "gpt-other")
	c.end(1, &service.OpenAIForwardResult{RequestID: "resp_first", OpenAIWSMode: true}, nil)
	c.response(2, []byte(`{"type":"response.output_text.delta","delta":"second partial"}`))
	c.end(2, &service.OpenAIForwardResult{RequestID: "resp_second", OpenAIWSMode: true}, errors.New("disconnected"))
	require.Len(t, r.entries, 2)
	require.Equal(t, "resp_first", r.entries[0].RequestID)
	require.Equal(t, "first question", r.entries[0].UserText)
	require.Equal(t, "first answer", r.entries[0].AssistantText)
	require.True(t, r.entries[0].ResponseComplete)
	require.Equal(t, "resp_second", r.entries[1].RequestID)
	require.Equal(t, "second question", r.entries[1].UserText)
	require.Equal(t, "second partial", r.entries[1].AssistantText)
	require.False(t, r.entries[1].ResponseComplete)
	require.Empty(t, c.turns)
}
