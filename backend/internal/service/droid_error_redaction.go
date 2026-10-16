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
		if droidNestedJSONDocument(trimmed) {
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
// their line endings retain their original bytes. A data line that is valid
// JSON on its own flushes immediately. A data line that is not (the start of a
// JSON document split across several data lines, which SSE clients join with
// "\n") holds the rest of its event, bounded by droidErrorJSONMaxBytes, until
// the blank line or EOF so the joined payload can be classified and redacted.
type droidErrorSSEReader struct {
	source io.ReadCloser
	reader *bufio.Reader
	key    string
	event  string
	// payloads holds the data of held lines only. flushedData records that
	// this event already emitted a standalone JSON data line; appending more
	// data lines to a complete JSON value can never yield valid JSON.
	payloads    [][]byte
	flushedData bool
	held        [][]byte
	heldSize    int
	holding     bool
	ready       []byte
	terminal    error
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
		if r.terminal != nil && r.holding {
			if errors.Is(r.terminal, io.EOF) {
				r.ready = append(r.ready, r.flushHeld()...)
			} else {
				// The event is incomplete; never release held fragments.
				r.resetHeld()
			}
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
		var out []byte
		if r.holding {
			out = r.flushHeld()
		}
		r.event = ""
		r.flushedData = false
		return append(out, line...)
	}
	if bytes.HasPrefix(content, []byte("event:")) {
		r.event = strings.TrimSpace(string(content[len("event:"):]))
	}
	var payload []byte
	isData := bytes.HasPrefix(content, []byte("data:"))
	start := len("data:")
	if isData {
		if len(content) > start && content[start] == ' ' {
			start++
		}
		payload = content[start:]
	}
	if r.holding {
		if r.heldSize+len(line) > droidErrorJSONMaxBytes {
			r.resetHeld()
			r.terminal = errors.New("factory upstream error frame exceeded size limit")
			return nil
		}
		r.held = append(r.held, line)
		r.heldSize += len(line)
		if isData {
			r.payloads = append(r.payloads, payload)
		}
		return nil
	}
	if !isData {
		return line
	}
	if !json.Valid(payload) {
		r.holding = true
		r.held = [][]byte{line}
		r.heldSize = len(line)
		r.payloads = [][]byte{payload}
		return nil
	}
	r.flushedData = true
	// The event name may arrive after this line, so a non-error payload is
	// also checked for a key hidden under nested JSON string escaping.
	if !droidSSEEventIsError(r.event) && !droidSSEPayloadIsError(payload) && !droidJSONPayloadHidesKey(payload, r.key) {
		return line
	}
	redacted := sanitizeDroidJSONErrorBody(payload, r.key)
	out := make([]byte, 0, start+len(redacted)+len(ending))
	out = append(out, content[:start]...)
	out = append(out, redacted...)
	return append(out, ending...)
}

// flushHeld classifies the whole event using every data line joined by "\n".
// Error events collapse their held data lines into one redacted data line.
func (r *droidErrorSSEReader) flushHeld() []byte {
	held := r.held
	joined := bytes.Join(r.payloads, []byte("\n"))
	r.resetHeld()
	valid := !r.flushedData && json.Valid(joined)
	isError := droidSSEEventIsError(r.event) || (valid && (droidSSEPayloadIsError(joined) || droidJSONPayloadHidesKey(joined, r.key)))
	var out []byte
	if !isError {
		for _, line := range held {
			out = append(out, line...)
		}
		return out
	}
	redacted := []byte(droidRedactedErrorJSON)
	if valid {
		redacted = sanitizeDroidJSONErrorBody(joined, r.key)
	}
	written := false
	for _, line := range held {
		if !bytes.HasPrefix(line, []byte("data:")) {
			out = append(out, line...)
			continue
		}
		if written {
			continue
		}
		written = true
		content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		out = append(out, "data: "...)
		out = append(out, redacted...)
		out = append(out, line[len(content):]...)
	}
	return out
}

func (r *droidErrorSSEReader) resetHeld() {
	r.held, r.heldSize, r.holding, r.payloads = nil, 0, false, nil
}

func droidSSEEventIsError(event string) bool {
	return event == "error" || event == "response.error" || event == "response.failed"
}

func droidSSEPayloadIsError(payload []byte) bool {
	var envelope struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return false
	}
	return droidSSEEventIsError(envelope.Type) || droidJSONErrorPresent(envelope.Error) || droidJSONErrorPresent(envelope.Response.Error)
}

// droidJSONPayloadHidesKey reports whether decoding a valid JSON payload, and
// any JSON documents nested in its strings, reveals the key. Single-level
// escapes are already normalized by droidJSONEscapeReader before
// droidSecretReader matches raw bytes; a key hidden one level deeper always
// leaves an escaped backslash pair, so other payloads skip the decode.
// Only nested string documents count toward droidErrorJSONMaxDepth, so a
// deeply structured success event is not misjudged; exceeding it is treated
// as hiding the key.
func droidJSONPayloadHidesKey(payload []byte, key string) bool {
	if key == "" || !bytes.Contains(payload, []byte(`\\`)) {
		return false
	}
	value, ok := decodeDroidErrorJSON(payload)
	return ok && droidJSONValueHidesKey(value, key, 0)
}

func droidJSONValueHidesKey(value any, key string, depth int) bool {
	if depth > droidErrorJSONMaxDepth {
		return true
	}
	switch value := value.(type) {
	case string:
		if strings.Contains(value, key) {
			return true
		}
		trimmed := strings.TrimSpace(value)
		if droidNestedJSONDocument(trimmed) {
			if nested, valid := decodeDroidErrorJSON([]byte(trimmed)); valid {
				return droidJSONValueHidesKey(nested, key, depth+1)
			}
		}
	case map[string]any:
		for name, child := range value {
			if strings.Contains(name, key) || droidJSONValueHidesKey(child, key, depth) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if droidJSONValueHidesKey(child, key, depth) {
				return true
			}
		}
	}
	return false
}

// A string may itself be a JSON document: an object, an array, or a quoted
// string whose escapes hide characters until it is decoded again.
func droidNestedJSONDocument(trimmed string) bool {
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, `"`)
}

func droidJSONErrorPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (r *droidErrorSSEReader) Close() error { return safeDroidError(r.source.Close()) }
