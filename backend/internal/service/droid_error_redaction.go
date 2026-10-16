package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	droidErrorJSONMaxBytes = 1 << 20
	droidErrorJSONMaxDepth = 16
	droidRedactedErrorJSON = `{"error":{"type":"upstream_error","message":"Factory upstream error details unavailable"}}`
)

// Nested JSON strings are decoded before redaction because the shared error
// extractor also decodes JSON embedded inside error.message.
func sanitizeDroidJSONErrorBody(data []byte, key string) []byte {
	if key == "" {
		return data
	}
	if len(data) > droidErrorJSONMaxBytes {
		return []byte(droidRedactedErrorJSON)
	}
	value, ok := decodeDroidErrorJSON(data)
	if !ok {
		return bytes.ReplaceAll(data, []byte(key), []byte("[REDACTED]"))
	}
	value, ok = redactDroidErrorJSONValue(value, key, 0)
	if !ok {
		return []byte(droidRedactedErrorJSON)
	}
	result, err := json.Marshal(value)
	if err != nil || len(result) > droidErrorJSONMaxBytes {
		return []byte(droidRedactedErrorJSON)
	}
	return result
}

func decodeDroidErrorJSON(data []byte) (any, bool) {
	if !json.Valid(data) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err == nil
}

func redactDroidErrorJSONValue(value any, key string, depth int) (any, bool) {
	if depth > droidErrorJSONMaxDepth {
		return nil, false
	}
	switch value := value.(type) {
	case string:
		value = strings.ReplaceAll(value, key, "[REDACTED]")
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			if nested, valid := decodeDroidErrorJSON([]byte(trimmed)); valid {
				nested, ok := redactDroidErrorJSONValue(nested, key, depth+1)
				if !ok {
					return nil, false
				}
				encoded, err := json.Marshal(nested)
				if err != nil || len(encoded) > droidErrorJSONMaxBytes {
					return nil, false
				}
				value = string(encoded)
			}
		}
		return value, true
	case map[string]any:
		redacted := make(map[string]any, len(value))
		for name, child := range value {
			child, ok := redactDroidErrorJSONValue(child, key, depth+1)
			if !ok {
				return nil, false
			}
			redacted[strings.ReplaceAll(name, key, "[REDACTED]")] = child
		}
		return redacted, true
	case []any:
		for i, child := range value {
			child, ok := redactDroidErrorJSONValue(child, key, depth+1)
			if !ok {
				return nil, false
			}
			value[i] = child
		}
	}
	return value, true
}

// Only error data lines are rewritten. Successful SSE events, comments and
// their line endings retain their original bytes.
type droidErrorSSEReader struct {
	source   io.ReadCloser
	reader   *bufio.Reader
	key      string
	event    string
	ready    []byte
	terminal error
}

func newDroidErrorSSEReader(source io.ReadCloser, key string) io.ReadCloser {
	return &droidErrorSSEReader{source: source, reader: bufio.NewReaderSize(source, 4096), key: key}
}

func (r *droidErrorSSEReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.ready) == 0 {
		if r.terminal != nil {
			return 0, r.terminal
		}
		line, err := r.readLine()
		if err != nil {
			r.terminal = safeDroidError(err)
		}
		if len(line) > 0 {
			r.ready = r.sanitizeLine(line)
		}
	}
	n := copy(p, r.ready)
	r.ready = r.ready[n:]
	return n, nil
}

func (r *droidErrorSSEReader) readLine() ([]byte, error) {
	var line []byte
	for {
		fragment, err := r.reader.ReadSlice('\n')
		if len(line)+len(fragment) > droidErrorJSONMaxBytes {
			return nil, errors.New("factory upstream error frame exceeded size limit")
		}
		line = append(line, fragment...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func (r *droidErrorSSEReader) sanitizeLine(line []byte) []byte {
	content := bytes.TrimSuffix(line, []byte("\n"))
	content = bytes.TrimSuffix(content, []byte("\r"))
	ending := line[len(content):]
	if len(content) == 0 {
		r.event = ""
		return line
	}
	if bytes.HasPrefix(content, []byte("event:")) {
		r.event = strings.TrimSpace(string(content[len("event:"):]))
		return line
	}
	if !bytes.HasPrefix(content, []byte("data:")) {
		return line
	}
	start := len("data:")
	if len(content) > start && content[start] == ' ' {
		start++
	}
	payload := content[start:]
	isError := r.event == "error" || r.event == "response.error" || r.event == "response.failed"
	if !isError {
		var envelope struct {
			Type     string          `json:"type"`
			Error    json.RawMessage `json:"error"`
			Response struct {
				Error json.RawMessage `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal(payload, &envelope) == nil {
			isError = envelope.Type == "error" || envelope.Type == "response.error" || envelope.Type == "response.failed" ||
				droidJSONErrorPresent(envelope.Error) || droidJSONErrorPresent(envelope.Response.Error)
		}
	}
	if !isError {
		return line
	}
	redacted := []byte(droidRedactedErrorJSON)
	if json.Valid(payload) {
		redacted = sanitizeDroidJSONErrorBody(payload, r.key)
	}
	out := make([]byte, 0, start+len(redacted)+len(ending))
	out = append(out, content[:start]...)
	out = append(out, redacted...)
	return append(out, ending...)
}

func droidJSONErrorPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (r *droidErrorSSEReader) Close() error { return safeDroidError(r.source.Close()) }
