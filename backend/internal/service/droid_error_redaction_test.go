//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func droidNestedErrorBody(t *testing.T, escaped string, levels int) []byte {
	t.Helper()
	body := []byte(`{"error":{"message":"invalid ` + escaped + `"}}`)
	for range levels {
		var err error
		body, err = json.Marshal(map[string]any{"error": map[string]any{"message": string(body)}})
		require.NoError(t, err)
	}
	return body
}

func droidNestedErrorMessage(t *testing.T, body []byte) string {
	t.Helper()
	for range droidErrorJSONMaxDepth {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &envelope))
		if !strings.HasPrefix(envelope.Error.Message, "{") {
			return envelope.Error.Message
		}
		body = []byte(envelope.Error.Message)
	}
	t.Fatal("nested error was not terminated")
	return ""
}

func TestDroidErrorJSONRedactsNestedUnicodeAndSlash(t *testing.T) {
	for _, key := range []string{"factory_test_secret", "factory/key+test="} {
		unicode := ""
		for _, character := range key {
			unicode += fmt.Sprintf(`\u%04x`, character)
		}
		for _, escaped := range []string{unicode, strings.ReplaceAll(key, "/", `\/`)} {
			for _, levels := range []int{0, 1, 3} {
				body := droidNestedErrorBody(t, escaped, levels)
				redacted := sanitizeDroidJSONErrorBody(body, key)
				require.Equal(t, "invalid [REDACTED]", droidNestedErrorMessage(t, redacted))
				if levels == 1 {
					require.Equal(t, "invalid [REDACTED]", extractUpstreamErrorMessage(redacted))
				}
			}
		}
	}
}

func TestDroidErrorJSONPreservesLiteralTextAndNumbers(t *testing.T) {
	body := []byte(`{"error":{"message":"literal \\u0073 and slash \/; no credential"},"id":9007199254740993}`)
	redacted := sanitizeDroidJSONErrorBody(body, "factory_test_secret")
	require.JSONEq(t, string(body), string(redacted))
	require.Contains(t, string(redacted), "9007199254740993")
	require.Equal(t, "literal \\u0073 and slash /; no credential", extractUpstreamErrorMessage(redacted))
	require.Equal(t, "plain [REDACTED]", string(sanitizeDroidJSONErrorBody([]byte("plain factory_test_secret"), "factory_test_secret")))
}

func TestDroidErrorJSONBoundsFailClosed(t *testing.T) {
	key := "factory_test_secret"
	oversized := []byte(`{"error":{"message":"` + strings.Repeat("x", droidErrorJSONMaxBytes) + key + `"}}`)
	require.Equal(t, droidRedactedErrorJSON, string(sanitizeDroidJSONErrorBody(oversized, key)))
	deep := []byte(strings.Repeat("[", droidErrorJSONMaxDepth+1) + `"` + key + `"` + strings.Repeat("]", droidErrorJSONMaxDepth+1))
	require.Equal(t, droidRedactedErrorJSON, string(sanitizeDroidJSONErrorBody(deep, key)))
}

type droidRedactionTestSource struct {
	data     []byte
	chunk    int
	readErr  error
	closeErr error
	closed   bool
}

func (r *droidRedactionTestSource) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		if r.readErr != nil {
			return 0, r.readErr
		}
		return 0, io.EOF
	}
	n := min(len(p), len(r.data), r.chunk)
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func (r *droidRedactionTestSource) Close() error {
	r.closed = true
	return r.closeErr
}

func TestDroidErrorSSEReaderRedactsNestedHTTP200Errors(t *testing.T) {
	key := "factory/key+test="
	escaped := strings.ReplaceAll(key, "/", `\u002f`)
	nested := droidNestedErrorBody(t, escaped, 1)
	for _, event := range []string{"", "error", "response.error", "response.failed"} {
		for chunk := 1; chunk <= 8; chunk++ {
			prefix := ""
			if event != "" {
				prefix = "event: " + event + "\r\n"
			}
			payload := append([]byte(prefix+"data: "), nested...)
			payload = append(payload, []byte("\r\n\r\n")...)
			reader := newDroidErrorSSEReader(&droidRedactionTestSource{data: payload, chunk: chunk}, key)
			out, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.True(t, bytes.HasPrefix(out, []byte(prefix+"data: ")))
			require.True(t, bytes.HasSuffix(out, []byte("\r\n\r\n")))
			data := bytes.TrimSuffix(bytes.TrimPrefix(out, []byte(prefix+"data: ")), []byte("\r\n\r\n"))
			require.Equal(t, "invalid [REDACTED]", extractUpstreamErrorMessage(data))
		}
	}

	// An unlabelled Responses event can carry the error inside response.error.
	payload := []byte(`data:{"type":"response.failed","response":{"error":{"message":"factory\u002fkey+test="}}}` + "\n")
	reader := newDroidErrorSSEReader(io.NopCloser(bytes.NewReader(payload)), key)
	out, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(out), `"message":"[REDACTED]"`)
	require.True(t, bytes.HasPrefix(out, []byte("data:")))
}

func TestDroidErrorSSEReaderPreservesSuccessfulEvents(t *testing.T) {
	input := ": heartbeat\r\n\r\nevent: response.output_text.delta\r\n" +
		`data: {"type":"response.output_text.delta","delta":"literal \\u0073 and slash \/"}` + "\r\n\r\n" +
		`data: {"type":"response.completed","response":{"error":null}}` + "\n\ndata: [DONE]\n\n"
	for chunk := 1; chunk <= 8; chunk++ {
		reader := newDroidErrorSSEReader(&droidRedactionTestSource{data: []byte(input), chunk: chunk}, "factory_test_secret")
		out, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Equal(t, input, string(out))
	}
}

func TestDroidErrorSSEReaderFlushesLinesAndHandlesFinalEOF(t *testing.T) {
	line := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ready\"}\n"
	source := &droidRedactionTestSource{data: []byte(line), chunk: len(line), readErr: errors.New("next event unavailable")}
	reader := newDroidErrorSSEReader(source, "factory_test_secret")
	buffer := make([]byte, len(line)+10)
	n, err := reader.Read(buffer)
	require.NoError(t, err, "a complete line must flush without waiting for the next event")
	require.Equal(t, line, string(buffer[:n]))

	for _, ending := range []string{"", "\n", "\r\n"} {
		payload := `data: {"error":{"message":"factory_test_\u0073ecret"}}` + ending
		reader := newDroidErrorSSEReader(io.NopCloser(strings.NewReader(payload)), "factory_test_secret")
		out, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Equal(t, `data: {"error":{"message":"[REDACTED]"}}`+ending, string(out))
	}
}

func TestDroidErrorSSEReaderBoundsAndSafeErrors(t *testing.T) {
	key := "factory_test_secret"
	oversized := "data: " + strings.Repeat("x", droidErrorJSONMaxBytes) + key + "\n"
	reader := newDroidErrorSSEReader(io.NopCloser(strings.NewReader(oversized)), key)
	out, err := io.ReadAll(reader)
	require.Error(t, err)
	require.Empty(t, out)
	require.NotContains(t, err.Error(), key)

	for _, readErr := range []error{context.Canceled, errors.New("transport " + key)} {
		source := &droidRedactionTestSource{data: []byte(": ok\n"), chunk: 1, readErr: readErr, closeErr: errors.New("close " + key)}
		reader := newDroidErrorSSEReader(source, key)
		out, err := io.ReadAll(reader)
		require.Equal(t, ": ok\n", string(out))
		require.Error(t, err)
		require.NotContains(t, err.Error(), key)
		if errors.Is(readErr, context.Canceled) {
			require.ErrorIs(t, err, context.Canceled)
		}
		closeErr := reader.Close()
		require.True(t, source.closed)
		require.Error(t, closeErr)
		require.NotContains(t, closeErr.Error(), key)
	}

	reader = newDroidErrorSSEReader(io.NopCloser(strings.NewReader("event: error\ndata: malformed "+key+"\n\n")), key)
	out, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.NotContains(t, string(out), key)
	require.Contains(t, string(out), "Factory upstream error details unavailable")
}

// joinDroidSSEDataLines mimics an SSE client: data lines of one event joined by "\n".
func joinDroidSSEDataLines(t *testing.T, out []byte) [][]byte {
	t.Helper()
	var events [][]byte
	for _, event := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n\n") {
		var data []string
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if len(data) > 0 {
			events = append(events, []byte(strings.Join(data, "\n")))
		}
	}
	return events
}

func TestDroidErrorSSEReaderRedactsMultiLineErrorEvents(t *testing.T) {
	key := "factory_test_secret"
	nested, err := json.Marshal(`{"error":{"message":"factory_test_\u0073ecret"}}`)
	require.NoError(t, err)
	body := "data: {\"type\":\"error\",\n" + `data: "error":{"message":` + string(nested) + "}}\n\n"
	for _, prefix := range []string{"", "event: error\n", ": comment\n"} {
		for _, ending := range []string{"\n", "\r\n"} {
			wire := strings.ReplaceAll(prefix+body, "\n", ending)
			for _, chunk := range []int{1, 2, 3, 5, 8, 4096} {
				reader := newDroidErrorSSEReader(&droidRedactionTestSource{data: []byte(wire), chunk: chunk}, key)
				out, err := io.ReadAll(reader)
				require.NoError(t, err)
				events := joinDroidSSEDataLines(t, out)
				require.Len(t, events, 1)
				require.True(t, json.Valid(events[0]), "collapsed error must stay valid JSON: %s", events[0])
				message := extractUpstreamErrorMessage(events[0])
				require.NotContains(t, message, key)
				require.Contains(t, message, "[REDACTED]")
				require.Equal(t, 1, strings.Count(string(out), "data:"), "error data lines collapse into one")
				require.True(t, strings.HasSuffix(string(out), ending+ending))
				if prefix != "" {
					require.True(t, strings.HasPrefix(string(out), strings.ReplaceAll(prefix, "\n", ending)))
				}
			}
		}
	}

	// An error named only by the event line, whose joined data is not JSON.
	reader := newDroidErrorSSEReader(io.NopCloser(strings.NewReader("event: error\ndata: first "+key+"\ndata: second\n\n")), key)
	out, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NotContains(t, string(out), key)
	require.Equal(t, "event: error\ndata: "+droidRedactedErrorJSON+"\n\n", string(out))

	// The event line may follow the data lines inside the same event.
	reader = newDroidErrorSSEReader(io.NopCloser(strings.NewReader("data: {\"message\":\ndata: \""+key+"\"}\nevent: error\n\n")), key)
	out, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.NotContains(t, string(out), key)

	// A held error event ending at EOF without a blank line is still redacted.
	reader = newDroidErrorSSEReader(io.NopCloser(strings.NewReader("data: {\"error\":\ndata: {\"message\":\""+key+"\"}}")), key)
	out, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, `data: {"error":{"message":"[REDACTED]"}}`+"\n", string(out))
}

func TestDroidErrorSSEReaderPreservesMultiLineSuccessEvents(t *testing.T) {
	input := "data: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"literal \\\\u0073\"}\nid: 7\n\n" +
		"data:\ndata: {\"type\":\"done\"}\n\n" +
		"data: [DONE]\n\n"
	for _, chunk := range []int{1, 3, 8, 4096} {
		reader := newDroidErrorSSEReader(&droidRedactionTestSource{data: []byte(input), chunk: chunk}, "factory_test_secret")
		out, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Equal(t, input, string(out))
	}
}

func TestDroidErrorSSEReaderNeverReleasesIncompleteHeldEvents(t *testing.T) {
	key := "factory_test_secret"
	for _, readErr := range []error{context.Canceled, errors.New("transport " + key)} {
		source := &droidRedactionTestSource{data: []byte(": ok\ndata: {\"error\":\ndata: \"" + key + "\"}\n"), chunk: 4, readErr: readErr}
		out, err := io.ReadAll(newDroidErrorSSEReader(source, key))
		require.Error(t, err)
		require.NotContains(t, err.Error(), key)
		require.Equal(t, ": ok\n", string(out))
	}

	// A held event is bounded by the same frame limit as a single line.
	line := "data: " + strings.Repeat("x", 1<<16) + "\n"
	wire := "data: {\"error\":\n" + strings.Repeat(line, droidErrorJSONMaxBytes/len(line)+1) + "data: \"" + key + "\"}\n\n"
	out, err := io.ReadAll(newDroidErrorSSEReader(io.NopCloser(strings.NewReader(wire)), key))
	require.Error(t, err)
	require.NotContains(t, err.Error(), key)
	require.Empty(t, out)
}

func droidHiddenKeyPayload(t *testing.T, field string) string {
	t.Helper()
	nested, err := json.Marshal(`{"error":{"message":"factory_test_\u0073ecret"}}`)
	require.NoError(t, err)
	return `{"` + field + `":` + string(nested) + `}`
}

func TestDroidErrorSSEReaderRedactsCompleteDataBeforeErrorEventName(t *testing.T) {
	key := "factory_test_secret"
	payload := droidHiddenKeyPayload(t, "message")
	for _, ending := range []string{"\n", "\r\n"} {
		wire := "data: " + payload + ending + "event: error" + ending + ending
		for _, chunk := range []int{1, 7, 4096} {
			out, err := io.ReadAll(newDroidErrorSSEReader(&droidRedactionTestSource{data: []byte(wire), chunk: chunk}, key))
			require.NoError(t, err)
			events := joinDroidSSEDataLines(t, out)
			require.Len(t, events, 1)
			message := extractUpstreamErrorMessage(events[0])
			require.NotContains(t, message, key)
			require.Contains(t, message, "[REDACTED]")
			require.True(t, strings.HasSuffix(string(out), "event: error"+ending+ending))
		}
	}

	// Escaped backslashes without a hidden key, and deep structure, keep their bytes.
	deep := strings.Repeat(`{"a":`, droidErrorJSONMaxDepth+4) + `"C:\\path \\u0073"` + strings.Repeat("}", droidErrorJSONMaxDepth+4)
	input := "data: " + deep + "\n\n" + `data: {"delta":"{\"text\":\"a\\\\b\"}"}` + "\n\n"
	out, err := io.ReadAll(newDroidErrorSSEReader(io.NopCloser(strings.NewReader(input)), key))
	require.NoError(t, err)
	require.Equal(t, input, string(out))
}

func TestDroidRoundTripRedactsHiddenKeysInNativeStreams(t *testing.T) {
	for _, route := range []struct{ path, model string }{
		{"https://api.factory.ai/api/llm/a/v1/messages", "claude-sonnet-4-6"},
		{"https://api.factory.ai/api/llm/o/v1/responses", "gpt-5.4"},
	} {
		for name, wire := range map[string]string{
			"data_then_event": "data: " + droidHiddenKeyPayload(t, "message") + "\nevent: error\n\n",
			"multi_line":      "data: {\"type\":\"error\",\n" + "data: " + strings.TrimPrefix(droidHiddenKeyPayload(t, "error"), "{") + "\n\n",
		} {
			request, err := http.NewRequest(http.MethodPost, route.path, strings.NewReader(`{"model":"`+route.model+`","stream":true}`))
			require.NoError(t, err)
			resp, err := droidRoundTrip(request, droidTestAccount(), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
			})
			require.NoError(t, err, name)
			out, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.NoError(t, err, name)
			events := joinDroidSSEDataLines(t, out)
			require.Len(t, events, 1, name)
			decoded := droidDecodeAllJSONStrings(t, events[0])
			require.NotContains(t, decoded, "factory_test_secret", "%s %s", route.path, name)
			require.Contains(t, decoded, "[REDACTED]", "%s %s", route.path, name)
		}
	}
}

// droidDecodeAllJSONStrings independently decodes every nested JSON string
// layer, as a permissive client would, and returns all decoded text.
func droidDecodeAllJSONStrings(t *testing.T, data []byte) string {
	t.Helper()
	var value any
	require.NoError(t, json.Unmarshal(data, &value))
	var out strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			out.WriteString(v + "\n")
			var nested any
			if json.Unmarshal([]byte(v), &nested) == nil {
				walk(nested)
			}
		case map[string]any:
			for k, child := range v {
				out.WriteString(k + "\n")
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(value)
	return out.String()
}

// droidQuoteJSON wraps text as a JSON string document the given number of times.
func droidQuoteJSON(t *testing.T, text string, times int) string {
	t.Helper()
	for i := 0; i < times; i++ {
		encoded, err := json.Marshal(text)
		require.NoError(t, err)
		text = string(encoded)
	}
	return text
}

func TestDroidNestedJSONStringScalarDoesNotLeak(t *testing.T) {
	const key = "factory_test_secret"
	for layers := 2; layers <= 4; layers++ {
		// The innermost layer hides one key character behind a \u escape.
		hidden := strings.TrimSuffix(strings.TrimPrefix(droidQuoteJSON(t, key, 1), `"`), `"`)
		hidden = strings.Replace(hidden, "s", `\u0073`, 1)
		payload := `{"message":` + droidQuoteJSON(t, `"`+hidden+`"`, layers-1) + `}`
		errorPayload := `{"error":{"message":` + droidQuoteJSON(t, `"`+hidden+`"`, layers-1) + `}}`
		for name, wire := range map[string]string{
			"event_after":  "data: " + payload + "\nevent: error\n\n",
			"event_before": "event: error\ndata: " + payload + "\n\n",
			"unlabelled":   "data: " + errorPayload + "\n\n",
		} {
			redactor := newDroidErrorSSEReader(io.NopCloser(strings.NewReader(wire)), key)
			reader := &droidSecretReader{source: &droidJSONEscapeReader{source: redactor}, secret: []byte(key)}
			out, err := io.ReadAll(reader)
			require.NoError(t, err)
			events := joinDroidSSEDataLines(t, out)
			require.Len(t, events, 1, name)
			decoded := droidDecodeAllJSONStrings(t, events[0])
			require.NotContains(t, decoded, key, "layers=%d %s", layers, name)
			require.Contains(t, decoded, "[REDACTED]", "layers=%d %s", layers, name)
		}
	}

	// Quoted, escaped success text without the key keeps its original bytes.
	input := `data: {"delta":"\"say \\\"hi\\\"\""}` + "\n\n"
	out, err := io.ReadAll(newDroidErrorSSEReader(io.NopCloser(strings.NewReader(input)), key))
	require.NoError(t, err)
	require.Equal(t, input, string(out))
}
