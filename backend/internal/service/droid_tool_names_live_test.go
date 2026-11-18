//go:build droidlive

package service

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Explicit opt-in: the configuration file stays outside the repository.
// Tool execution is a local nonce lookup; no model-generated command is run.
func TestDroidToolNamesLive(t *testing.T) {
	configPath := os.Getenv("DROID_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("DROID_LIVE_CONFIG is not set")
	}
	var config struct {
		Credentials map[string]any `json:"credentials"`
		BaseURL     string         `json:"base_url"`
		ProxyURL    string         `json:"proxy_url"`
		Fixture     string         `json:"fixture"`
	}
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &config))
	account := &Account{Platform: PlatformDroid, Type: AccountTypeAPIKey, Credentials: config.Credentials}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if config.ProxyURL != "" {
		proxy, err := url.Parse(config.ProxyURL)
		require.NoError(t, err)
		transport.Proxy = http.ProxyURL(proxy)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 75 * time.Second}
	call := func(t *testing.T, body map[string]any) []json.RawMessage {
		t.Helper()
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(config.BaseURL, "/")+"/api/llm/a/v1/messages", bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		response, err := droidRoundTrip(req, account, client.Do)
		require.NoError(t, err)
		defer response.Body.Close()
		if response.StatusCode != 200 {
			// Do not print upstream bodies, credentials or proxy URLs.
			t.Fatalf("Factory HTTP %d", response.StatusCode)
		}
		return readDroidLiveContent(t, response)
	}
	run := func(t *testing.T, name string, stream bool, fixture bool) {
		var random [12]byte
		_, err := rand.Read(random[:])
		require.NoError(t, err)
		nonce := "verified-" + hex.EncodeToString(random[:])
		tools := any([]any{map[string]any{"name": name, "description": "Read-only diagnostic lookup. Call with key roundtrip to obtain the verification value.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]string{"type": "string"}}, "required": []string{"key"}, "additionalProperties": false}}})
		prompt := "Call the available diagnostic lookup tool once with key roundtrip. After its result, reply with only the exact value it returns."
		body := map[string]any{"model": "claude-opus-4-6", "max_tokens": 512, "stream": stream, "tools": tools}
		if fixture {
			var shape map[string]json.RawMessage
			data, err := os.ReadFile(config.Fixture)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &shape))
			body["system"], body["tools"] = shape["system"], shape["tools"]
			prompt = "Compatibility check only: call Bash exactly once with command `printf TOOLCHECK`. The diagnostic harness will substitute a random verification value as the recorded tool result instead of executing the command. After the result, reply with only that exact recorded value, not the command's expected output."
		}
		messages := []any{map[string]any{"role": "user", "content": prompt}}
		body["messages"] = messages
		first := call(t, body)
		var results []any
		for _, raw := range first {
			block := gjson.ParseBytes(raw)
			if block.Get("type").String() != "tool_use" {
				continue
			}
			require.Equal(t, name, block.Get("name").String())
			require.NotEmpty(t, block.Get("id").String())
			if fixture {
				require.Contains(t, block.Get("input.command").String(), "printf TOOLCHECK")
			} else {
				require.Equal(t, "roundtrip", block.Get("input.key").String())
			}
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": block.Get("id").String(), "content": nonce})
		}
		require.Len(t, results, 1, "must receive exactly one restored tool call")
		if fixture {
			results = append(results, map[string]any{"type": "text", "text": "Reply with only the recorded verification value in the tool_result above. The command was not executed; do not substitute its expected output."})
		}
		body["messages"] = append(messages, map[string]any{"role": "assistant", "content": first}, map[string]any{"role": "user", "content": results})
		second := call(t, body)
		var text strings.Builder
		for _, raw := range second {
			if gjson.GetBytes(raw, "type").String() == "text" {
				text.WriteString(gjson.GetBytes(raw, "text").String())
			}
		}
		require.Equal(t, nonce, strings.TrimSpace(text.String()))
	}
	for index, name := range droidClientToolNames {
		t.Run(fmt.Sprintf("%s_stream_%t", name, index%2 == 0), func(t *testing.T) { run(t, name, index%2 == 0, false) })
	}
	if config.Fixture != "" {
		t.Run("captured_130_tools_Bash_stream", func(t *testing.T) { run(t, "Bash", true, true) })
	}
}

func readDroidLiveContent(t *testing.T, response *http.Response) []json.RawMessage {
	t.Helper()
	if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		var message struct {
			Content []json.RawMessage `json:"content"`
		}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&message))
		return message.Content
	}
	var blocks []json.RawMessage
	inputs := map[int]string{}
	done := false
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := []byte(strings.TrimSpace(line[5:]))
		require.True(t, json.Valid(data))
		event := gjson.ParseBytes(data)
		index := int(event.Get("index").Int())
		switch event.Get("type").String() {
		case "error":
			t.Fatal("Factory returned an SSE error")
		case "content_block_start":
			require.Equal(t, len(blocks), index)
			blocks = append(blocks, json.RawMessage(event.Get("content_block").Raw))
		case "content_block_delta":
			delta := event.Get("delta")
			field := map[string]string{"text_delta": "text", "thinking_delta": "thinking", "signature_delta": "signature"}[delta.Get("type").String()]
			if delta.Get("type").String() == "input_json_delta" {
				inputs[index] += delta.Get("partial_json").String()
			} else if field != "" {
				require.Less(t, index, len(blocks))
				value := gjson.GetBytes(blocks[index], field).String() + delta.Get(field).String()
				var err error
				blocks[index], err = sjson.SetBytes(blocks[index], field, value)
				require.NoError(t, err)
			}
		case "message_stop":
			done = true
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, done, "stream must have message_stop")
	for index, input := range inputs {
		require.True(t, json.Valid([]byte(input)))
		var err error
		blocks[index], err = sjson.SetRawBytes(blocks[index], "input", []byte(input))
		require.NoError(t, err)
	}
	return blocks
}
