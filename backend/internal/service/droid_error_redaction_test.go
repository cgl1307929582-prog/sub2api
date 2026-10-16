//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
