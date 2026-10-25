package handler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type conversationWSTurn struct {
	entry    service.ConversationLog
	response conversationResponseCapture
}

type conversationWSCapture struct {
	mu       sync.Mutex
	recorder conversationRecorder
	base     service.ConversationLog
	turns    map[int]*conversationWSTurn
}

func newConversationWSCapture(recorder conversationRecorder, key *service.APIKey, ctx context.Context, endpoint string) *conversationWSCapture {
	c := &conversationWSCapture{recorder: recorder, turns: make(map[int]*conversationWSTurn)}
	c.base = service.ConversationLog{RequestID: service.ConversationRequestID(ctx), UserID: key.UserID, APIKeyID: key.ID, Endpoint: endpoint, Stream: true, StatusCode: 200}
	c.base.ClientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
	if key.GroupID != nil {
		id := *key.GroupID
		c.base.GroupID = &id
	}
	return c
}

func (c *conversationWSCapture) start(turn int, body []byte, model string) {
	if c.recorder == nil || !c.recorder.Config().Enabled {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.turns[turn]; exists {
		return
	}
	// Only the current and immediately previous turn can be in flight.
	for id := range c.turns {
		if id < turn-1 {
			delete(c.turns, id)
		}
	}
	entry := c.base
	entry.CreatedAt = time.Now().UTC()
	entry.Model = model
	if len(body) > conversationRawLimit {
		entry.InputTruncated = true
	} else {
		text, requestModel, _ := conversationInput(body)
		entry.UserText, entry.InputTruncated = service.BoundConversationText(text, service.ConversationTextLimit)
		if requestModel != "" {
			entry.Model = requestModel
		}
	}
	c.turns[turn] = &conversationWSTurn{entry: entry}
}

func (c *conversationWSCapture) response(turn int, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.turns[turn]
	if state == nil {
		return
	}
	if len(payload) > conversationEventLimit {
		state.response.truncated = true
		return
	}
	state.response.eventJSON(payload)
}

func (c *conversationWSCapture) end(turn int, result *service.OpenAIForwardResult, turnErr error) {
	c.mu.Lock()
	state := c.turns[turn]
	delete(c.turns, turn)
	c.mu.Unlock()
	if state == nil {
		return
	}
	state.entry.AssistantText = state.response.text.String()
	state.entry.OutputTruncated = state.response.truncated
	state.entry.ErrorText = state.response.errorText
	state.entry.ResponseComplete = state.response.complete && turnErr == nil && state.entry.ErrorText == ""
	if result != nil && result.RequestID != "" {
		state.entry.RequestID = result.RequestID
	} else {
		state.entry.RequestID = fmt.Sprintf("%s.ws-%d", state.entry.RequestID, turn)
	}
	if turnErr != nil {
		state.entry.StatusCode = 502
		if state.entry.ErrorText == "" {
			state.entry.ErrorText = "WebSocket response interrupted"
		}
	}
	c.recorder.Record(&state.entry)
}
