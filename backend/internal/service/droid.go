package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	AccountModeFactory = "factory"
	droidClientVersion = "0.242.0"
	droidSystemPrompt  = "You are Droid, an AI software engineering agent built by Factory."
)

// Compatibility candidates from the public Factory adapter reference, not a live catalog.
func DefaultDroidModelIDs() []string {
	return []string{"claude-sonnet-4-6", "claude-opus-4-6", "claude-haiku-4-5-20251001",
		"gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "glm-5.3-flash", "glm-5", "kimi-k2.5", "minimax-m2.5",
		"gemini-3-flash-preview", "gemini-3.1-pro-preview", "gemini-3.5-flash", "gemini-3.6-flash", "gemini-3.7-flash", "gemini-3.8-flash",
		"grok-4.5", "grok-4.6", "grok-4.7", "deepseek-v4-flash-0731", "deepseek-v4-pro", "deepseek-v4.1-flash",
		"mistral-large-4", "mistral-medium-3.5", "qwen3.8-max", "nemotron-3-ultra", "inkling"}
}

func (a *Account) IsDroid() bool { return a != nil && a.Platform == PlatformDroid }

var droidAPIKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

func validateDroidAccount(account *Account) error {
	if !account.IsDroid() {
		return nil
	}
	key := account.GetCredential("api_key")
	if account.Type != AccountTypeAPIKey {
		return infraerrors.BadRequest("INVALID_FACTORY_ACCOUNT", "Droid / Factory accounts require API key credentials")
	}
	if len(key) < 8 || len(key) > 8192 || !droidAPIKeyPattern.MatchString(key) {
		return infraerrors.BadRequest("INVALID_FACTORY_KEY", "Factory API key is missing or invalid; enter the key without a Bearer prefix")
	}
	for _, field := range []string{"refresh_token", "access_token"} {
		if account.GetCredential(field) != "" {
			return infraerrors.BadRequest("INVALID_FACTORY_ACCOUNT", "Factory OAuth credentials are not supported")
		}
	}
	return nil
}

func droidModelRoute(model string) (protocol, provider, path string) {
	switch {
	case strings.HasPrefix(model, "claude-"):
		return APIProtocolAnthropic, "anthropic", "/api/llm/a/v1/messages"
	case strings.HasPrefix(model, "minimax-"):
		return APIProtocolAnthropic, "fireworks", "/api/llm/a/v1/messages"
	case strings.HasPrefix(model, "gpt-"):
		return APIProtocolResponses, "openai", "/api/llm/o/v1/responses"
	case strings.HasPrefix(model, "grok-"):
		return APIProtocolResponses, "xai", "/api/llm/o/v1/responses"
	case strings.HasPrefix(model, "gemini-"):
		// The transport converts the shared Chat envelope to Google on the wire.
		return APIProtocolChatCompletions, "google", "/api/llm/o/v1/chat/completions"
	case strings.HasPrefix(model, "mistral-"):
		return APIProtocolChatCompletions, "mistral", "/api/llm/o/v1/chat/completions"
	case strings.HasPrefix(model, "glm-"), strings.HasPrefix(model, "kimi-"),
		strings.HasPrefix(model, "deepseek-"), strings.HasPrefix(model, "qwen"),
		strings.HasPrefix(model, "nemotron-"), model == "inkling":
		return APIProtocolChatCompletions, "fireworks", "/api/llm/o/v1/chat/completions"
	default:
		return "", "", ""
	}
}

func prepareDroidRequest(request *http.Request, account *Account) (*http.Request, error) {
	if err := validateDroidAccount(account); err != nil {
		return nil, err
	}
	if request == nil || request.URL == nil || request.GetBody == nil {
		return nil, errors.New("factory request body is unavailable")
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, errors.New("factory request body is unavailable")
	}
	defer func() { _ = body.Close() }()
	payload, err := io.ReadAll(body)
	if err != nil {
		return nil, errors.New("factory request body is unavailable")
	}
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, errors.New("factory request body is invalid")
	}
	protocol, provider, path := droidModelRoute(envelope.Model)
	if path == "" {
		return nil, errors.New("factory model family is unsupported; sync the supported Factory model catalog")
	}
	if request.Method != http.MethodPost || request.URL.Path != path || request.URL.RawQuery != "" || request.URL.Fragment != "" || request.URL.User != nil {
		return nil, errors.New("factory endpoint does not match the selected model protocol")
	}
	payload, err = prepareDroidBody(payload, protocol)
	if err != nil {
		return nil, err
	}
	if provider == "mistral" {
		payload, err = prepareDroidMistralBody(payload)
		if err != nil {
			return nil, err
		}
	}
	if provider == "google" {
		payload, err = droidChatToGoogle(payload)
		if err != nil {
			return nil, err
		}
	}
	out := request.Clone(WithHTTPUpstreamRedirectsDisabled(request.Context()))
	if provider == "google" {
		out.URL.Path = droidGooglePath
	}
	out.Body = io.NopCloser(bytes.NewReader(payload))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	out.ContentLength = int64(len(payload))
	out.Host = ""
	for name := range out.Header {
		switch strings.ToLower(name) {
		case "authorization", "x-api-key", "x-goog-api-key", "cookie", "proxy-authorization",
			"x-api-provider", "x-factory-client", "x-client-version", "x-session-id", "x-assistant-message-id", "content-length",
			"session_id", "conversation_id", "x-codex-turn-state":
			delete(out.Header, name)
		}
	}
	out.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	out.Header.Set("X-API-Provider", provider)
	out.Header.Set("X-Session-ID", uuid.NewString())
	out.Header.Set("X-Assistant-Message-ID", uuid.NewString())
	out.Header.Set("Accept-Encoding", "identity")
	if protocol == APIProtocolAnthropic {
		out.Header.Set("X-API-Key", "placeholder")
	}
	if err := applyDroidClientHeaders(out.Header, account); err != nil {
		return nil, err
	}
	return out, nil
}

func applyDroidClientHeaders(header http.Header, account *Account) error {
	ua := strings.TrimSpace(account.GetCredential("user_agent"))
	if ua == "" {
		ua = "factory-cli/" + droidClientVersion
	}
	if strings.ContainsAny(ua, "\r\n") {
		return errors.New("factory user agent is invalid")
	}
	header.Set("User-Agent", ua)
	header.Set("X-Factory-Client", "cli")
	version := droidClientVersion
	if strings.HasPrefix(ua, "factory-cli/") {
		version = strings.TrimPrefix(ua, "factory-cli/")
		if version == "" || strings.ContainsAny(version, " \t") {
			return errors.New("factory client version is invalid")
		}
	}
	header.Set("X-Client-Version", version)
	return nil
}

// Factory requires its identity before caller instructions. RawMessage keeps
// tools, signed thinking, cache-control blocks and large JSON numbers intact.
func prepareDroidBody(payload []byte, protocol string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		return nil, errors.New("factory request body is invalid")
	}
	stripDroidEnvironmentNotice(fields, protocol)
	switch protocol {
	case APIProtocolAnthropic:
		system, err := prependDroidSystem(fields["system"])
		if err != nil {
			return nil, err
		}
		fields["system"] = system
	case APIProtocolResponses:
		var instructions string
		if raw := fields["instructions"]; len(raw) > 0 && json.Unmarshal(raw, &instructions) != nil {
			return nil, errors.New("factory Responses instructions must be a string")
		}
		if !strings.HasPrefix(instructions, droidSystemPrompt) {
			instructions = droidSystemPrompt + "\n\n" + instructions
		}
		fields["instructions"], _ = json.Marshal(instructions)
	case APIProtocolChatCompletions:
		var messages []json.RawMessage
		if raw := fields["messages"]; len(raw) > 0 && json.Unmarshal(raw, &messages) != nil {
			return nil, errors.New("factory Chat Completions messages must be an array")
		}
		leadingIdentity := false
		if len(messages) > 0 {
			var first struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(messages[0], &first) == nil && (first.Role == "system" || first.Role == "developer") {
				leadingIdentity = droidTextStartsWithIdentity(first.Content)
			}
		}
		if !leadingIdentity {
			prefix, _ := json.Marshal(map[string]string{"role": "system", "content": droidSystemPrompt})
			messages = append([]json.RawMessage{prefix}, messages...)
		}
		fields["messages"], _ = json.Marshal(messages)
	default:
		return nil, errors.New("factory request protocol is unsupported")
	}
	return json.Marshal(fields)
}

func droidTextStartsWithIdentity(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.HasPrefix(text, droidSystemPrompt)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	return json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 && blocks[0].Type == "text" && strings.HasPrefix(blocks[0].Text, droidSystemPrompt)
}

func prependDroidSystem(raw json.RawMessage) (json.RawMessage, error) {
	if droidTextStartsWithIdentity(raw) {
		return raw, nil
	}
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) == nil {
		if text != "" {
			text = "\n\n" + text
		}
		return json.Marshal(droidSystemPrompt + text)
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, errors.New("factory Messages system must be a string or content blocks")
	}
	prefix, _ := json.Marshal(map[string]string{"type": "text", "text": droidSystemPrompt})
	return json.Marshal(append([]json.RawMessage{prefix}, blocks...))
}

// Local rejection is not an upstream failure and must never rotate accounts.
type droidRequestError struct{ message string }

func (e *droidRequestError) Error() string { return e.message }

// Preserve only the classification, never the potentially sensitive original error.
type droidTransportError struct{ class upstreamTransportErrorClass }

func (e *droidTransportError) Error() string { return "Factory upstream request failed" }

func droidRoundTrip(request *http.Request, account *Account, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if !account.IsDroid() {
		return send(request)
	}
	prepared, err := prepareDroidRequest(request, account)
	if err != nil {
		message := err.Error()
		var validation *infraerrors.ApplicationError
		if errors.As(err, &validation) {
			message = validation.Message
		}
		return nil, &droidRequestError{message: message}
	}
	response, err := send(prepared)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, safeDroidError(err)
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("factory returned an invalid response")
	}
	if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		_ = response.Body.Close()
		return nil, errors.New("factory returned an unsupported content encoding")
	}
	key := account.GetCredential("api_key")
	response.Header = response.Header.Clone()
	for name, values := range response.Header {
		switch strings.ToLower(name) {
		case "authorization", "x-api-key", "x-goog-api-key", "set-cookie", "content-length", "content-md5", "etag":
			delete(response.Header, name)
		default:
			for i, value := range values {
				values[i] = strings.ReplaceAll(value, key, "[REDACTED]")
			}
		}
	}
	response.ContentLength = -1
	if response.StatusCode >= http.StatusMultipleChoices {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, droidErrorJSONMaxBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			return nil, safeDroidError(readErr)
		}
		response.Body = io.NopCloser(bytes.NewReader(sanitizeDroidJSONErrorBody(data, key)))
	} else if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body = newDroidErrorSSEReader(response.Body, key)
	}
	response.Body = &droidSecretReader{source: &droidJSONEscapeReader{source: response.Body}, secret: []byte(key)}
	if response.StatusCode == http.StatusOK {
		response, err = adaptDroidChatResponse(request, prepared, response)
		if err != nil {
			return nil, safeDroidError(err)
		}
	}
	return response, nil
}

func safeDroidError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		return io.EOF
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return &droidTransportError{class: classifyUpstreamTransportError(err)}
	}
}

// Hold only an unfinished secret prefix, so normal SSE frames flush immediately.
type droidSecretReader struct {
	source                 io.ReadCloser
	secret, pending, ready []byte
	terminal               error
	buffer                 [8192]byte
}

func (r *droidSecretReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.ready) == 0 {
		if r.terminal != nil {
			return 0, r.terminal
		}
		n, err := r.source.Read(r.buffer[:])
		r.pending = append(r.pending, r.buffer[:n]...)
		for len(r.secret) > 0 {
			i := bytes.Index(r.pending, r.secret)
			if i < 0 {
				break
			}
			r.ready = append(r.ready, r.pending[:i]...)
			r.ready = append(r.ready, "[REDACTED]"...)
			r.pending = r.pending[i+len(r.secret):]
		}
		keep := 0
		if err == nil {
			for size := min(len(r.pending), len(r.secret)-1); size > 0; size-- {
				if bytes.Equal(r.pending[len(r.pending)-size:], r.secret[:size]) {
					keep = size
					break
				}
			}
		}
		emit := len(r.pending) - keep
		r.ready = append(r.ready, r.pending[:emit]...)
		r.pending = append([]byte(nil), r.pending[emit:]...)
		if err != nil {
			r.terminal = safeDroidError(err)
		}
		if n == 0 && err == nil {
			return 0, nil
		}
	}
	n := copy(p, r.ready)
	r.ready = r.ready[n:]
	return n, nil
}

func (r *droidSecretReader) Close() error { return safeDroidError(r.source.Close()) }

func detachAccountUpstreamContext(ctx context.Context, account *Account, stream bool) (context.Context, context.CancelFunc) {
	if account.IsDroid() {
		return ctx, func() {}
	}
	return detachStreamUpstreamContext(ctx, stream)
}

// Normalize equivalent JSON escapes before matching credentials. Factory keys
// contain ASCII only; preserve backslash pairs so literal "\\u..." text is unchanged.
type droidJSONEscapeReader struct {
	source         io.ReadCloser
	pending, ready []byte
	terminal       error
	buffer         [8192]byte
}

func (r *droidJSONEscapeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.ready) == 0 {
		if r.terminal != nil {
			return 0, r.terminal
		}
		n, err := r.source.Read(r.buffer[:])
		r.pending = append(r.pending, r.buffer[:n]...)
		for len(r.pending) > 0 {
			if r.pending[0] != '\\' {
				r.ready = append(r.ready, r.pending[0])
				r.pending = r.pending[1:]
				continue
			}
			if len(r.pending) < 2 {
				break
			}
			size := 2
			if r.pending[1] == 'u' {
				size = 6
				if len(r.pending) < size {
					break
				}
			}
			chunk := r.pending[:size]
			switch chunk[1] {
			case '/':
				r.ready = append(r.ready, '/')
			case 'u':
				value, e := strconv.ParseUint(string(chunk[2:]), 16, 16)
				if e == nil && value < 128 && (value == '=' || droidAPIKeyPattern.MatchString(string(rune(value)))) {
					r.ready = append(r.ready, byte(value))
				} else {
					r.ready = append(r.ready, chunk...)
				}
			default:
				r.ready = append(r.ready, chunk...)
			}
			r.pending = r.pending[size:]
		}
		if err != nil {
			r.ready = append(r.ready, r.pending...)
			r.pending = nil
			r.terminal = safeDroidError(err)
		}
		if n == 0 && err == nil {
			return 0, nil
		}
	}
	n := copy(p, r.ready)
	r.ready = r.ready[n:]
	return n, nil
}
func (r *droidJSONEscapeReader) Close() error { return r.source.Close() }

func writeDroidStreamError(c *gin.Context, responses bool, message string, state *apicompat.AnthropicEventToResponsesState) {
	if c == nil || (c.Request != nil && c.Request.Context().Err() != nil) {
		return
	}
	if responses {
		id, sequence, model, created := "resp_"+uuid.NewString(), 0, "", time.Now().Unix()
		if state != nil {
			if state.ResponseID != "" {
				id = state.ResponseID
			}
			sequence, model = state.SequenceNumber, state.Model
			if state.Created > 0 {
				created = state.Created
			}
		}
		payload, _ := json.Marshal(apicompat.ResponsesStreamEvent{Type: "response.failed", SequenceNumber: sequence,
			Response: &apicompat.ResponsesResponse{ID: id, Object: "response", CreatedAt: created, Status: "failed", Model: model,
				Output: []apicompat.ResponsesOutput{}, Error: &apicompat.ResponsesError{Code: "upstream_error", Message: message}}})
		_, _ = fmt.Fprintf(c.Writer, "event: response.failed\ndata: %s\n\n", payload)
	} else {
		_, _ = fmt.Fprint(c.Writer, buildChatStreamErrorSSE("upstream_error", message))
	}
	c.Writer.Flush()
}

func convertDroidResponsesToAnthropic(account *Account, req *apicompat.ResponsesRequest) (*apicompat.AnthropicRequest, error) {
	if !account.IsDroid() {
		return apicompat.ResponsesToAnthropicRequest(req)
	}
	copyReq := *req
	if req.Reasoning != nil {
		reasoning := *req.Reasoning
		switch reasoning.Effort {
		case "none", "minimal":
			reasoning.Effort = "low"
		case "", "low", "medium", "high", "xhigh", "max":
		default:
			return nil, errors.New("unsupported Factory reasoning effort")
		}
		copyReq.Reasoning = &reasoning
	}
	out, err := apicompat.ResponsesToAnthropicRequestWithOptions(&copyReq, &apicompat.AnthropicResponsesOptions{PreserveThinkingSignatures: true})
	if err != nil {
		return nil, err
	}
	// Haiku/Sonnet 4.5 accept manual thinking budgets but reject the newer
	// output_config.effort field produced by the generic Responses converter.
	if strings.HasPrefix(req.Model, "claude-") && len(claude.EffortLevelsForModel(req.Model)) == 0 {
		out.OutputConfig = nil
	}
	if out.Thinking != nil && out.Thinking.Type == "enabled" && out.Thinking.BudgetTokens >= out.MaxTokens {
		if out.MaxTokens <= 1024 {
			return nil, errors.New("factory thinking requires max_output_tokens above 1024; lower reasoning effort or use Messages with an explicit budget")
		}
		// Respect the caller's output cap; do not silently increase billable generation.
		out.Thinking.BudgetTokens = out.MaxTokens - 1
	}
	return out, nil
}
