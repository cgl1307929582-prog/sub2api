package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const droidChatBodyLimit = 32 << 20

// Mistral exposes only high and none. Generic ingress converters can supply
// medium even when the caller omitted an effort; never forward that invalid value.
func prepareDroidMistralBody(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil, errors.New("invalid Factory Mistral request")
	}
	if raw := fields["reasoning_effort"]; len(raw) > 0 {
		var effort string
		if json.Unmarshal(raw, &effort) != nil {
			return nil, errors.New("invalid Factory Mistral reasoning effort")
		}
		switch effort {
		case "":
			delete(fields, "reasoning_effort")
		case "none", "off":
			fields["reasoning_effort"] = json.RawMessage(`"none"`)
		case "minimal", "low", "medium", "high", "xhigh", "max":
			fields["reasoning_effort"] = json.RawMessage(`"high"`)
		default:
			return nil, errors.New("unsupported Factory Mistral reasoning effort")
		}
	}
	return json.Marshal(fields)
}

// This transport adapter lets the existing three ingress converters share the
// same Factory wire handling, including the administrator's connection test.
func adaptDroidChatResponse(original, prepared *http.Request, response *http.Response) (*http.Response, error) {
	provider := prepared.Header.Get("X-API-Provider")
	if prepared.URL.Path != droidGooglePath && prepared.URL.Path != "/api/llm/o/v1/chat/completions" {
		return response, nil
	}
	body, err := original.GetBody()
	if err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	err = json.NewDecoder(body).Decode(&request)
	_ = body.Close()
	if err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	if provider == "google" {
		state := newDroidGoogleStream(request.Model)
		if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
			_ = response.Body.Close()
			return nil, errors.New("factory Gemini did not return its native event stream")
		}
		response.Body = newDroidChatSSEReader(response.Body, state.transform, func() error {
			if !state.finished {
				return errors.New("factory Gemini stream ended before a finish reason")
			}
			return nil
		}, true)
		if !request.Stream {
			data, err := bufferDroidGoogleChat(response.Body, state)
			_ = response.Body.Close()
			if err != nil {
				return nil, err
			}
			response.Body = io.NopCloser(bytes.NewReader(data))
			response.Header.Set("Content-Type", "application/json")
		}
		return response, nil
	}
	if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		response.Body = newDroidChatSSEReader(response.Body, normalizeDroidChatJSON, nil, false)
		return response, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, droidChatBodyLimit+1))
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(data) > droidChatBodyLimit {
		return nil, errors.New("factory Chat response exceeded size limit")
	}
	data, err = normalizeDroidChatJSON(data)
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

// Mistral's content can be an array of text/thinking blocks, including in SSE
// deltas. Normalize it before the shared OpenAI converters parse string fields.
// RawMessage preserves every unrelated provider field and large JSON integer.
func normalizeDroidChatJSON(data []byte) ([]byte, error) {
	var frame map[string]json.RawMessage
	if json.Unmarshal(data, &frame) != nil || frame == nil {
		return nil, errors.New("invalid Factory Chat response")
	}
	var choices []map[string]json.RawMessage
	if raw := frame["choices"]; len(raw) > 0 && json.Unmarshal(raw, &choices) != nil {
		return nil, errors.New("invalid Factory Chat choices")
	}
	for _, choice := range choices {
		for _, field := range []string{"message", "delta"} {
			var message map[string]json.RawMessage
			if json.Unmarshal(choice[field], &message) != nil || message == nil {
				continue
			}
			content := bytes.TrimSpace(message["content"])
			if len(content) == 0 || content[0] != '[' {
				continue
			}
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking []struct {
					Text string `json:"text"`
				} `json:"thinking"`
			}
			if json.Unmarshal(content, &parts) != nil {
				return nil, errors.New("invalid Factory Chat content blocks")
			}
			var text, reasoning strings.Builder
			for _, part := range parts {
				switch part.Type {
				case "text":
					_, _ = text.WriteString(part.Text)
				case "thinking":
					for _, thought := range part.Thinking {
						_, _ = reasoning.WriteString(thought.Text)
					}
				default:
					return nil, errors.New("unsupported Factory Chat response content block")
				}
			}
			message["content"], _ = json.Marshal(text.String())
			if reasoning.Len() > 0 {
				var existing string
				_ = json.Unmarshal(message["reasoning_content"], &existing)
				message["reasoning_content"], _ = json.Marshal(existing + reasoning.String())
			}
			choice[field], _ = json.Marshal(message)
		}
	}
	if choices != nil {
		frame["choices"], _ = json.Marshal(choices)
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(frame["usage"], &usage) == nil && usage != nil {
		// Inkling reports reasoning_tokens at the usage root; Fireworks may use
		// output_tokens_details. Keep the original fields and expose the standard.
		if len(usage["completion_tokens_details"]) == 0 || string(usage["completion_tokens_details"]) == "null" {
			if raw := usage["output_tokens_details"]; len(raw) > 0 {
				usage["completion_tokens_details"] = raw
			} else if raw := usage["reasoning_tokens"]; len(raw) > 0 {
				usage["completion_tokens_details"], _ = json.Marshal(map[string]json.RawMessage{"reasoning_tokens": raw})
			}
		}
		frame["usage"], _ = json.Marshal(usage)
	}
	return json.Marshal(frame)
}

// A pull-based reader avoids goroutines and buffers only one bounded SSE frame.
// Closing it closes the upstream socket; incomplete streams never gain [DONE].
type droidChatSSEReader struct {
	source    io.ReadCloser
	scanner   *bufio.Scanner
	transform func([]byte) ([]byte, error)
	finish    func() error
	google    bool
	ready     []byte
	terminal  error
	eof, done bool
}

func newDroidChatSSEReader(source io.ReadCloser, transform func([]byte) ([]byte, error), finish func() error, google bool) io.ReadCloser {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), droidErrorJSONMaxBytes)
	return &droidChatSSEReader{source: source, scanner: scanner, transform: transform, finish: finish, google: google}
}

func (r *droidChatSSEReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.ready) == 0 {
		if r.terminal != nil {
			return 0, r.terminal
		}
		data, err := r.next()
		if err != nil {
			r.terminal = err
			if !errors.Is(err, io.EOF) {
				// Safe constant: upstream error text may contain credentials.
				r.ready = []byte("data: " + droidRedactedErrorJSON + "\n\n")
			}
		} else {
			r.ready = data
		}
	}
	n := copy(p, r.ready)
	r.ready = r.ready[n:]
	return n, nil
}

func (r *droidChatSSEReader) next() ([]byte, error) {
	if r.eof {
		if r.finish != nil {
			if err := r.finish(); err != nil {
				return nil, err
			}
		}
		if r.google && !r.done {
			r.done = true
			return []byte("data: [DONE]\n\n"), nil
		}
		if !r.done {
			return nil, errors.New("factory Chat stream ended before DONE")
		}
		return nil, io.EOF
	}
	var data []byte
	event, sawData := "", false
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		if len(line) == 0 {
			if len(data) > 0 {
				break
			}
			if sawData && droidSSEEventIsError(event) {
				return nil, errors.New("factory upstream stream returned an error event")
			}
			event, sawData = "", false
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(line[len("event:"):]))
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		sawData = true
		if len(data) > 0 {
			data = append(data, '\n')
		}
		data = append(data, bytes.TrimSpace(line[5:])...)
		if len(data) > droidErrorJSONMaxBytes {
			return nil, errors.New("factory Chat stream frame exceeded size limit")
		}
	}
	if len(data) == 0 || r.scanner.Err() != nil {
		if err := r.scanner.Err(); err != nil {
			return nil, safeDroidError(err)
		}
		r.eof = true
		return nil, nil
	}
	// The event name alone can mark a failure; the payload need not repeat an
	// error field. Never let such a frame be skipped and later gain [DONE].
	if droidSSEEventIsError(event) {
		return nil, errors.New("factory upstream stream returned an error event")
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		if r.google {
			return nil, nil
		}
		r.done = true
		return []byte("data: [DONE]\n\n"), nil
	}
	converted, err := r.transform(data)
	if err != nil {
		return nil, err
	}
	if len(converted) == 0 {
		return nil, nil
	}
	return append(append([]byte("data: "), converted...), []byte("\n\n")...), nil
}

func (r *droidChatSSEReader) Close() error { return r.source.Close() }

func bufferDroidGoogleChat(body io.Reader, state *droidGoogleStream) ([]byte, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, droidChatBodyLimit+1))
	scanner.Buffer(make([]byte, 4096), droidErrorJSONMaxBytes)
	var text, reasoning strings.Builder
	var tools []apicompat.ChatToolCall
	finish, done, size := "", false, 0
	for scanner.Scan() {
		line := scanner.Bytes()
		size += len(line) + 1
		if size > droidChatBodyLimit {
			return nil, errors.New("factory Gemini response exceeded size limit")
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if bytes.Equal(data, []byte("[DONE]")) {
			done = true
			continue
		}
		var chunk apicompat.ChatCompletionsChunk
		if json.Unmarshal(data, &chunk) != nil {
			return nil, errors.New("invalid Factory Gemini converted chunk")
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				_, _ = text.WriteString(*choice.Delta.Content)
			}
			if choice.Delta.ReasoningContent != nil {
				_, _ = reasoning.WriteString(*choice.Delta.ReasoningContent)
			}
			for _, call := range choice.Delta.ToolCalls {
				call.Index = nil
				tools = append(tools, call)
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !done || !state.finished {
		return nil, errors.New("incomplete Factory Gemini response")
	}
	content, _ := json.Marshal(text.String())
	return json.Marshal(apicompat.ChatCompletionsResponse{ID: state.id, Object: "chat.completion", Created: state.created, Model: state.model,
		Choices: []apicompat.ChatChoice{{Index: 0, Message: apicompat.ChatMessage{Role: "assistant", Content: content, ReasoningContent: reasoning.String(), ToolCalls: tools}, FinishReason: finish}}, Usage: state.usage})
}
