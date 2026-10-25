package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type conversationTestRecorder struct {
	enabled bool
	entries []*service.ConversationLog
}

func (r *conversationTestRecorder) Config() service.ConversationLogConfig {
	return service.ConversationLogConfig{Enabled: r.enabled}
}
func (r *conversationTestRecorder) Record(e *service.ConversationLog) {
	r.entries = append(r.entries, e)
}

func TestConversationInputLatestHumanText(t *testing.T) {
	for _, tc := range []struct{ name, body, text string }{
		{"anthropic", `{"system":"hidden system","messages":[{"role":"user","content":"old"},{"role":"assistant","content":"old reply"},{"role":"user","content":[{"type":"text","text":"new question"},{"type":"image","source":{"data":"DO NOT STORE"}}]},{"role":"user","content":[{"type":"tool_result","content":"hidden tool result"}]}]}`, "new question\n[image]"},
		{"responses string", `{"input":"hello"}`, "hello"},
		{"responses array", `{"instructions":"hidden","input":[{"role":"developer","content":"hidden"},{"role":"user","content":[{"type":"input_text","text":"你好"},{"type":"input_file","file_data":"SECRET BYTES"}]}]}`, "你好\n[file]"},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"question"},{"inlineData":{"data":"BYTES"}}]}]}`, "question\n[attachment]"},
		{"invalid", `{"input":`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) { text, _, _ := conversationInput([]byte(tc.body)); require.Equal(t, tc.text, text) })
	}
}

func TestConversationStreamProtocolsAndFragmentation(t *testing.T) {
	tests := []struct {
		name, events, want string
		complete           bool
	}{
		{"anthropic", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"HIDDEN\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "你好", true},
		{"responses dedup", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n", "answer", true},
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n", "hello", true},
		{"gemini", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"private\",\"thought\":true},{\"text\":\"world\"}]},\"finishReason\":\"STOP\"}]}\n\n", "world", true},
		{"interrupted", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "partial", false},
		{"multiline SSE", "data: {\"choices\":\ndata: [{\"delta\":{\"content\":\"yes\"},\"finish_reason\":\"stop\"}]}\n\n", "yes", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &conversationResponseCapture{}
			// Split even UTF-8 sequences and JSON escapes across writes.
			for _, b := range []byte(tc.events) {
				p.feed([]byte{b}, "text/event-stream")
			}
			p.finish()
			require.Equal(t, tc.want, p.text.String())
			require.Equal(t, tc.complete, p.complete)
			require.False(t, p.truncated)
		})
	}
}

func TestConversationMiddlewareDoesNotChangeBodyOrStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			recorder := &conversationTestRecorder{enabled: enabled}
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 1})
				c.Next()
			})
			r.Use(ConversationLoggerMiddleware(recorder))
			input := `{"model":"test","stream":true,"messages":[{"role":"user","content":"question"}]}`
			output := "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n"
			r.POST("/v1/chat/completions", func(c *gin.Context) {
				body, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.Equal(t, input, string(body))
				c.Header("Content-Type", "text/event-stream")
				for i, b := range []byte(output) {
					if i%2 == 0 {
						_, _ = c.Writer.Write([]byte{b})
					} else {
						_, _ = c.Writer.WriteString(string(b))
					}
					c.Writer.Flush()
				}
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(input))
			req = req.WithContext(context.WithValue(req.Context(), ctxkey.ClientRequestID, "trace-123"))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, output, w.Body.String())
			require.True(t, w.Flushed)
			require.Equal(t, 200, w.Code)
			if !enabled {
				require.Empty(t, recorder.entries)
				return
			}
			require.Len(t, recorder.entries, 1)
			e := recorder.entries[0]
			require.Equal(t, "question", e.UserText)
			require.Equal(t, "answer", e.AssistantText)
			require.True(t, e.ResponseComplete)
			require.Equal(t, "client:trace-123", e.RequestID)
		})
	}
}

func TestConversationBoundsNeverAlterUpstreamBytes(t *testing.T) {
	input := strings.Repeat("x", conversationRawLimit+100)
	r := &conversationBodyReader{ReadCloser: io.NopCloser(strings.NewReader(input))}
	actual, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, input, string(actual))
	require.True(t, r.truncated)
	require.Len(t, r.body, conversationRawLimit)
	p := &conversationResponseCapture{}
	p.appendText(strings.Repeat("文", service.ConversationTextLimit/3+5))
	require.True(t, p.truncated)
	require.LessOrEqual(t, p.text.Len(), service.ConversationTextLimit)
	require.True(t, utf8.ValidString(p.text.String()))
	p.feed([]byte("data: "+strings.Repeat("x", conversationEventLimit+100)+"\n\ndata: [DONE]\n\n"), "text/event-stream")
	p.finish()
	require.True(t, p.complete)
	require.True(t, p.truncated)
	require.LessOrEqual(t, len(p.pending), conversationEventLimit)
}

func TestConversationOrdinaryRepliesAndErrors(t *testing.T) {
	for _, tc := range []struct {
		body, text string
		complete   bool
	}{
		{`{"content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"answer"}]}`, "answer", true},
		{`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reply"}]}]}`, "reply", true},
		{`{"error":{"message":"upstream failed"}}`, "", false},
	} {
		p := &conversationResponseCapture{}
		p.feed([]byte(tc.body), "application/json")
		p.finish()
		require.Equal(t, tc.text, p.text.String())
		require.Equal(t, tc.complete, p.complete)
	}
}
