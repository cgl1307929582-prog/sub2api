package handler

import (
	"bytes"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const conversationRawLimit = 8 * 1024 * 1024
const conversationEventLimit = 2 * 1024 * 1024

type conversationRecorder interface {
	Config() service.ConversationLogConfig
	Record(*service.ConversationLog)
}

type conversationBodyReader struct {
	io.ReadCloser
	body      []byte
	truncated bool
}

func (r *conversationBodyReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	remaining := conversationRawLimit - len(r.body)
	keep := min(n, remaining)
	r.body = append(r.body, p[:keep]...)
	if keep < n {
		r.truncated = true
	}
	return n, err
}

func isConversationEndpoint(method, p string) bool {
	if method != http.MethodPost {
		return false
	}
	return strings.HasSuffix(p, "/messages") || strings.HasSuffix(p, "/responses") ||
		strings.HasSuffix(p, "/chat/completions") || strings.HasSuffix(p, ":generateContent") ||
		strings.HasSuffix(p, ":streamGenerateContent") || strings.HasSuffix(p, "/systemone")
}

// ConversationLoggerMiddleware runs after authentication and before body rewrites.
// It preserves the original reader, flush behaviour and every byte sent downstream.
func ConversationLoggerMiddleware(recorder conversationRecorder) gin.HandlerFunc {
	return func(c *gin.Context) {
		if recorder == nil || !recorder.Config().Enabled || !isConversationEndpoint(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}
		key, ok := middleware.GetAPIKeyFromContext(c)
		if !ok || key == nil || c.Request.Body == nil {
			c.Next()
			return
		}
		entry := &service.ConversationLog{CreatedAt: time.Now().UTC(), UserID: key.UserID, APIKeyID: key.ID,
			RequestID: service.ConversationRequestID(c.Request.Context()), Endpoint: c.Request.URL.Path}
		if key.GroupID != nil {
			id := *key.GroupID
			entry.GroupID = &id
		}
		entry.ClientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		body := &conversationBodyReader{ReadCloser: c.Request.Body}
		c.Request.Body = body
		capture := &conversationResponseCapture{}
		writer := &conversationResponseWriter{ResponseWriter: c.Writer, capture: capture}
		c.Writer = writer
		c.Next()
		capture.finish()
		entry.UserText, entry.Model, entry.Stream = conversationInput(body.body)
		if entry.Model == "" && strings.Contains(entry.Endpoint, "/models/") {
			entry.Model = strings.Split(path.Base(entry.Endpoint), ":")[0]
		}
		entry.InputTruncated = body.truncated || !gjson.ValidBytes(body.body)
		entry.Stream = entry.Stream || capture.sse
		entry.AssistantText = capture.text.String()
		entry.ErrorText = capture.errorText
		entry.OutputTruncated = capture.truncated
		entry.ResponseComplete = capture.complete && writer.Status() < 400 && capture.errorText == ""
		entry.StatusCode = writer.Status()
		recorder.Record(entry)
	}
}

type conversationResponseWriter struct {
	gin.ResponseWriter
	capture *conversationResponseCapture
}

func (w *conversationResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *conversationResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.capture.feed(p[:n], w.Header().Get("Content-Type"))
	return n, err
}
func (w *conversationResponseWriter) WriteString(p string) (int, error) {
	n, err := w.ResponseWriter.WriteString(p)
	w.capture.feed([]byte(p[:n]), w.Header().Get("Content-Type"))
	return n, err
}

// Only user-visible text is stored: no system/developer prompts, tools, hidden
// reasoning, headers, credentials, or image/audio/file bytes are copied.
func conversationParts(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	var parts []string
	content.ForEach(func(_, part gjson.Result) bool {
		kind := part.Get("type").String()
		if part.Get("thought").Bool() {
			return true
		}
		switch kind {
		case "text", "input_text", "output_text", "":
			if value := part.Get("text"); value.Type == gjson.String {
				parts = append(parts, value.String())
			}
		case "image", "image_url", "input_image":
			parts = append(parts, "[image]")
		case "document", "input_file", "file":
			parts = append(parts, "[file]")
		case "input_audio", "audio":
			parts = append(parts, "[audio]")
		case "refusal":
			if value := part.Get("refusal"); value.Type == gjson.String {
				parts = append(parts, value.String())
			}
		}
		if kind == "" && (part.Get("inlineData").Exists() || part.Get("fileData").Exists()) {
			parts = append(parts, "[attachment]")
		}
		return true
	})
	return strings.Join(parts, "\n")
}

func conversationInput(body []byte) (string, string, bool) {
	if !gjson.ValidBytes(body) {
		return "", "", false
	}
	root := gjson.ParseBytes(body)
	model, stream := root.Get("model").String(), root.Get("stream").Bool()
	var latest string
	messages := root.Get("messages")
	if !messages.Exists() {
		messages = root.Get("input")
	}
	if messages.Type == gjson.String {
		return messages.String(), model, stream
	}
	geminiInput := !messages.Exists()
	if geminiInput {
		messages = root.Get("contents")
	}
	messages.ForEach(func(_, message gjson.Result) bool {
		role := message.Get("role").String()
		if role != "user" && (!geminiInput || role != "") {
			return true
		}
		content := message.Get("content")
		if !content.Exists() {
			content = message.Get("parts")
		}
		// Tool-result-only user messages should not erase the last human question.
		if text := conversationParts(content); text != "" {
			latest = text
		}
		return true
	})
	return latest, model, stream
}

type conversationResponseCapture struct {
	sse           bool
	pending       []byte
	event         []byte
	raw           []byte
	skippingLine  bool
	skippingEvent bool
	text          strings.Builder
	deltaSeen     bool
	complete      bool
	truncated     bool
	errorText     string
}

func (p *conversationResponseCapture) appendText(text string) {
	if text == "" {
		return
	}
	remaining := service.ConversationTextLimit - p.text.Len()
	text, cut := service.BoundConversationText(text, remaining)
	p.truncated = p.truncated || cut
	_, _ = p.text.WriteString(text)
}

func (p *conversationResponseCapture) feed(chunk []byte, contentType string) {
	p.sse = p.sse || strings.Contains(strings.ToLower(contentType), "text/event-stream")
	if !p.sse {
		keep := min(len(chunk), conversationRawLimit-len(p.raw))
		p.raw = append(p.raw, chunk[:keep]...)
		p.truncated = p.truncated || keep < len(chunk)
		return
	}
	for len(chunk) > 0 {
		end := bytes.IndexByte(chunk, '\n')
		n := len(chunk)
		if end >= 0 {
			n = end
		}
		if !p.skippingLine {
			if len(p.pending)+n > conversationEventLimit {
				p.pending = nil
				p.skippingLine = true
				p.skippingEvent = true
				p.truncated = true
			} else {
				p.pending = append(p.pending, chunk[:n]...)
			}
		}
		if end < 0 {
			return
		}
		if !p.skippingLine {
			p.line(bytes.TrimSuffix(p.pending, []byte{'\r'}))
		}
		p.pending = p.pending[:0]
		p.skippingLine = false
		chunk = chunk[n+1:]
	}
}

func (p *conversationResponseCapture) line(line []byte) {
	if len(line) == 0 {
		if !p.skippingEvent && len(p.event) > 0 {
			p.eventJSON(bytes.TrimSpace(p.event))
		}
		p.event = p.event[:0]
		p.skippingEvent = false
		return
	}
	if p.skippingEvent || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimPrefix(line, []byte("data:"))
	if len(data) > 0 && data[0] == ' ' {
		data = data[1:]
	}
	if len(p.event)+len(data)+1 > conversationEventLimit {
		p.skippingEvent = true
		p.event = nil
		p.truncated = true
		return
	}
	p.event = append(p.event, data...)
	p.event = append(p.event, '\n')
}

func responseVisibleText(root gjson.Result) string {
	if root.Get("content").IsArray() {
		return conversationParts(root.Get("content"))
	}
	if message := root.Get("choices.0.message"); message.Exists() {
		if text := conversationParts(message.Get("content")); text != "" {
			return text
		}
		return message.Get("refusal").String()
	}
	if output := root.Get("output_text"); output.Type == gjson.String {
		return output.String()
	}
	if root.Get("output").IsArray() {
		var parts []string
		root.Get("output").ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "message" && item.Get("role").String() == "assistant" {
				parts = append(parts, conversationParts(item.Get("content")))
			}
			return true
		})
		return strings.Join(parts, "\n")
	}
	if root.Get("candidates").Exists() {
		return conversationParts(root.Get("candidates.0.content.parts"))
	}
	return ""
}

func (p *conversationResponseCapture) eventJSON(raw []byte) {
	if bytes.Equal(raw, []byte("[DONE]")) {
		p.complete = true
		return
	}
	if !gjson.ValidBytes(raw) {
		p.truncated = true
		return
	}
	root := gjson.ParseBytes(raw)
	if errorMessage := root.Get("error.message").String(); errorMessage != "" {
		p.errorText, _ = service.BoundConversationText(errorMessage, 4096)
	}
	switch root.Get("type").String() {
	case "content_block_start":
		if root.Get("content_block.type").String() == "text" {
			text := root.Get("content_block.text").String()
			p.appendText(text)
			p.deltaSeen = p.deltaSeen || text != ""
		}
	case "content_block_delta":
		if root.Get("delta.type").String() == "text_delta" {
			p.appendText(root.Get("delta.text").String())
			p.deltaSeen = true
		}
	case "message_stop":
		p.complete = true
	case "response.output_text.delta":
		p.appendText(root.Get("delta").String())
		p.deltaSeen = true
	case "response.refusal.delta":
		p.appendText(root.Get("delta").String())
		p.deltaSeen = true
	case "response.completed", "response.done":
		if !p.deltaSeen {
			p.appendText(responseVisibleText(root.Get("response")))
		}
		status := root.Get("response.status").String()
		p.complete = status == "" || status == "completed"
	case "response.failed", "response.incomplete":
		if !p.deltaSeen {
			p.appendText(responseVisibleText(root.Get("response")))
		}
		p.errorText = root.Get("response.error.message").String()
		if p.errorText == "" {
			p.errorText = root.Get("type").String()
		}
	case "error":
		if p.errorText == "" {
			p.errorText = "upstream_error"
		}
	default:
		if root.Get("choices.0").Exists() {
			text := conversationParts(root.Get("choices.0.delta.content"))
			if text == "" {
				text = root.Get("choices.0.delta.refusal").String()
			}
			p.appendText(text)
			p.deltaSeen = true
			if reason := root.Get("choices.0.finish_reason"); reason.Exists() && reason.Type != gjson.Null {
				p.complete = true
			}
		} else if root.Get("candidates").Exists() {
			p.appendText(responseVisibleText(root))
			if root.Get("candidates.0.finishReason").String() != "" {
				p.complete = true
			}
		}
	}
}

func (p *conversationResponseCapture) finish() {
	if p.sse {
		if len(p.pending) > 0 && !p.skippingLine {
			p.line(bytes.TrimSuffix(p.pending, []byte{'\r'}))
		}
		p.line(nil)
		return
	}
	if !gjson.ValidBytes(p.raw) {
		if len(p.raw) > 0 {
			p.truncated = true
		}
		return
	}
	root := gjson.ParseBytes(p.raw)
	if root.IsArray() {
		root.ForEach(func(_, item gjson.Result) bool { p.eventJSON([]byte(item.Raw)); return true })
	} else {
		p.appendText(responseVisibleText(root))
		p.errorText, _ = service.BoundConversationText(root.Get("error.message").String(), 4096)
		status := root.Get("status").String()
		p.complete = p.errorText == "" && (status == "" || status == "completed")
	}
}
