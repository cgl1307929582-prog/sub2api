//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDroidToolNamesAllTenPreserveHistoryAndArguments(t *testing.T) {
	for _, name := range droidClientToolNames {
		t.Run(name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"tools":[{"name":%q,"description":"Bash is literal prose","input_schema":{"type":"object","properties":{"name":{"const":"Bash"}},"maximum":9007199254740993},"cache_control":{"type":"ephemeral","ttl":"1h"}}],"tool_choice":{"type":"tool","name":%q},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"Bash","signature":"keep-signed-Bash"},{"type":"tool_use","id":"call_Bash","name":%q,"input":{"name":"Bash","large":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_Bash","content":[{"type":"text","text":"Bash"},{"type":"tool_reference","tool_name":%q}]}]}],"metadata":{"name":"Bash"}}`, name, name, name, name)
			original := []byte(payload)
			body, names, err := prepareDroidToolNames(original, APIProtocolAnthropic)
			require.NoError(t, err)
			require.Equal(t, payload, string(original))
			alias := gjson.GetBytes(body, "tools.0.name").String()
			require.NotEqual(t, name, alias)
			require.Equal(t, name, names.reverse[alias])
			for _, path := range []string{"tool_choice.name", "messages.0.content.1.name", "messages.1.content.0.content.1.tool_name"} {
				require.Equal(t, alias, gjson.GetBytes(body, path).String(), path)
			}
			for _, path := range []string{"tools.0.input_schema", "tools.0.description", "tools.0.cache_control", "messages.0.content.0", "messages.0.content.1.input", "messages.0.content.1.id", "messages.1.content.0.tool_use_id", "messages.1.content.0.content.0", "metadata"} {
				require.Equal(t, gjson.Get(payload, path).Raw, gjson.GetBytes(body, path).Raw, path)
			}
			// The client stores original names; sending that history again must
			// use the same wire names without changing any client-side records.
			second, secondNames, err := prepareDroidToolNames(original, APIProtocolAnthropic)
			require.NoError(t, err)
			require.Equal(t, body, second)
			require.Equal(t, names.reverse, secondNames.reverse)
			response := []byte(fmt.Sprintf(`{"type":"message","content":[{"type":"thinking","thinking":%q,"signature":"opaque"},{"type":"text","text":%q},{"type":"tool_use","id":"call_Bash","name":%q,"input":{"name":%q,"large":9007199254740993}}],"metadata":{"name":%q}}`, alias, alias, alias, alias, alias))
			restored := restoreDroidToolNamesJSON(response, names)
			require.Equal(t, name, gjson.GetBytes(restored, "content.2.name").String())
			for _, path := range []string{"content.0", "content.1", "content.2.input", "content.2.id", "metadata"} {
				require.Equal(t, gjson.GetBytes(response, path).Raw, gjson.GetBytes(restored, path).Raw, path)
			}
		})
	}
}

func TestDroidToolNamesProtocolReferencesAndCollision(t *testing.T) {
	for _, tc := range []struct {
		protocol, body string
		paths          []string
	}{
		{APIProtocolResponses, `{"tools":[{"type":"function","name":"Bash","parameters":{"name":"Bash"}},{"type":"web_search_preview","name":"Write"}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"Bash"}]},"input":[{"type":"function_call","id":"f","call_id":"c","name":"Bash","arguments":"{\"name\":\"Bash\"}"},{"type":"function_call_output","call_id":"c","output":"Bash"}]}`, []string{"tools.0.name", "tool_choice.tools.0.name", "input.0.name"}},
		{APIProtocolChatCompletions, `{"tools":[{"type":"function","function":{"name":"Bash","parameters":{"name":"Bash"}}}],"tool_choice":{"type":"function","function":{"name":"Bash"}},"functions":[{"name":"Bash"}],"function_call":{"name":"Bash"},"messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"Bash","arguments":"{\"name\":\"Bash\"}"}}],"function_call":{"name":"Bash","arguments":"{}"}},{"role":"function","name":"Bash","content":"Bash"}]}`, []string{"tools.0.function.name", "tool_choice.function.name", "functions.0.name", "function_call.name", "messages.0.tool_calls.0.function.name", "messages.0.function_call.name", "messages.1.name"}},
		{APIProtocolAnthropic, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"Bash","id":"old-id","input":{"name":"Bash"}},{"type":"server_tool_use","name":"Write","id":"server-id","input":{}}]}]}`, []string{"messages.0.content.0.name"}},
	} {
		body, names, err := prepareDroidToolNames([]byte(tc.body), tc.protocol)
		require.NoError(t, err)
		for _, path := range tc.paths {
			require.Equal(t, names.forward["Bash"], gjson.GetBytes(body, path).String(), path)
		}
		require.NotContains(t, names.forward, "Write", "provider-owned tools must be untouched")
		require.Contains(t, string(body), `"name":"Bash"`, "arguments/schemas retain literal values")
	}
	input := []byte(`{"tools":[{"name":"Bash"},{"name":"sub2api_droid_tool_01"},{"name":"sub2api_droid_tool_01_1_suffix"}]}`)
	body, names, err := prepareDroidToolNames(input, APIProtocolAnthropic)
	require.NoError(t, err)
	alias := gjson.GetBytes(body, "tools.0.name").String()
	require.NotEqual(t, "sub2api_droid_tool_01", alias)
	require.NotEqual(t, "sub2api_droid_tool_01_1", alias)
	require.Equal(t, "sub2api_droid_tool_01", gjson.GetBytes(body, "tools.1.name").String())
	require.Equal(t, "Bash", names.reverse[alias])
	unaffected := []byte(`{"tools":[{"name":"read"}],"messages":[{"role":"user","content":"Bash"}],"metadata":{"name":"Write"}}`)
	unchanged, absent, err := prepareDroidToolNames(unaffected, APIProtocolAnthropic)
	require.NoError(t, err)
	require.Nil(t, absent)
	require.Equal(t, unaffected, unchanged)
}

func TestDroidToolNamesRoundTripWireProtocols(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6", "gpt-5.4", "glm-5", "gemini-4-argon"} {
		t.Run(model, func(t *testing.T) {
			protocol, provider, path := droidModelRoute(model)
			definition := `{"type":"function","function":{"name":"Bash","parameters":{"type":"object","properties":{}}}}`
			if protocol == APIProtocolAnthropic {
				definition = `{"name":"Bash","input_schema":{"type":"object","properties":{}}}`
			} else if protocol == APIProtocolResponses {
				definition = `{"type":"function","name":"Bash","parameters":{"type":"object","properties":{}}}`
			}
			payload := fmt.Sprintf(`{"model":%q,"stream":false,"messages":[],"input":[],"tools":[%s]}`, model, definition)
			req, err := http.NewRequest(http.MethodPost, "https://api.factory.ai"+path, strings.NewReader(payload))
			require.NoError(t, err)
			resp, err := droidRoundTrip(req, droidTestAccount(), func(sent *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(sent.Body)
				require.NoError(t, err)
				namePath := "tools.0.name"
				if provider == "google" {
					namePath = "tools.0.functionDeclarations.0.name"
				} else if protocol == APIProtocolChatCompletions {
					namePath = "tools.0.function.name"
				}
				alias := gjson.GetBytes(body, namePath).String()
				require.NotEmpty(t, alias)
				require.NotEqual(t, "Bash", alias)
				ct, response := "application/json", ""
				switch {
				case provider == "google":
					ct = "text/event-stream"
					response = fmt.Sprintf("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":%q,\"args\":{\"value\":\"kept\"}}}]},\"finishReason\":\"STOP\"}]}\n\n", alias)
				case protocol == APIProtocolAnthropic:
					response = fmt.Sprintf(`{"type":"message","content":[{"type":"tool_use","id":"call1","name":%q,"input":{"value":"kept"}}]}`, alias)
				case protocol == APIProtocolResponses:
					response = fmt.Sprintf(`{"object":"response","output":[{"type":"function_call","id":"item1","call_id":"call1","name":%q,"arguments":"{}"}]}`, alias)
				default:
					response = fmt.Sprintf(`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, alias)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			namePath := "choices.0.message.tool_calls.0.function.name"
			if protocol == APIProtocolAnthropic {
				namePath = "content.0.name"
			} else if protocol == APIProtocolResponses {
				namePath = "output.0.name"
			}
			require.Equal(t, "Bash", gjson.GetBytes(body, namePath).String())
			original, err := req.GetBody()
			require.NoError(t, err)
			defer original.Close()
			unchanged, err := io.ReadAll(original)
			require.NoError(t, err)
			require.Equal(t, payload, string(unchanged))
		})
	}
}

func TestDroidToolNamesNonDroidBypassAndCancellation(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.example/v1/messages", strings.NewReader(`{"tools":[{"name":"Bash"}]}`))
	require.NoError(t, err)
	expected := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("unchanged"))}
	actual, err := droidRoundTrip(req, &Account{Platform: PlatformAnthropic}, func(sent *http.Request) (*http.Response, error) {
		require.Same(t, req, sent)
		return expected, nil
	})
	require.NoError(t, err)
	require.Same(t, expected, actual)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = droidRoundTrip(req.WithContext(ctx), droidTestAccount(), func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled requests must not send")
		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func toolNamesForTest(t *testing.T) *droidToolNames {
	t.Helper()
	_, names, err := prepareDroidToolNames([]byte(`{"tools":[{"name":"Bash"},{"name":"Agent"},{"name":"sub2api_"}]}`), APIProtocolAnthropic)
	require.NoError(t, err)
	return names
}

func TestDroidToolNameSSEPreservesEventsAndOpaqueFields(t *testing.T) {
	names := toolNamesForTest(t)
	a := names.forward["Bash"]
	for _, body := range []string{
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":%q,"id":"call1","input":{"name":%q,"large":9007199254740993}}}`, a, a),
		fmt.Sprintf(`{"type":"response.output_item.added","item":{"type":"function_call","name":%q,"call_id":"call1","arguments":"{}"},"metadata":{"name":%q}}`, a, a),
		fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"function_call","name":%q,"call_id":"call1","arguments":"{}"}]}}`, a),
	} {
		for _, chunk := range []int{1, 7, 4096} {
			wire := ": ping\r\nid: event1\r\nevent: named_event\r\ndata: " + body + "\r\n\r\n"
			reader := newDroidToolNameSSEReader(&droidChunkReader{data: []byte(wire), chunk: chunk}, names)
			got, err := io.ReadAll(reader)
			require.NoError(t, err)
			expected := ": ping\r\nid: event1\r\nevent: named_event\r\ndata: " + string(restoreDroidToolNamesJSON([]byte(body), names)) + "\r\n\r\n"
			require.Equal(t, expected, string(got))
		}
	}
	wire := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"signature_delta\",\"signature\":\"" + a + "\"}}\n\n"
	got, err := io.ReadAll(newDroidToolNameSSEReader(io.NopCloser(strings.NewReader(wire)), names))
	require.NoError(t, err)
	require.Equal(t, wire, string(got))
	multiline := fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\ndata: \"content_block\":{\"type\":\"tool_use\",\"name\":%q}}\n\n", a)
	got, err = io.ReadAll(newDroidToolNameSSEReader(io.NopCloser(strings.NewReader(multiline)), names))
	require.NoError(t, err)
	require.Contains(t, string(got), `"name":"Bash"`)
	require.Contains(t, string(got), "event: content_block_start\n")
}

func TestDroidToolNameSSEFragmentedParallelCalls(t *testing.T) {
	names := toolNamesForTest(t)
	chunk := func(choice, index int, name, args string) string {
		data, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": choice, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": fmt.Sprintf("call%d", index), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}}}})
		require.NoError(t, err)
		return "data: " + string(data) + "\n\n"
	}
	a, b := names.forward["Bash"], names.forward["Agent"]
	wire := chunk(0, 0, a[:8], "") + chunk(1, 0, b[:10], "") + chunk(0, 0, a[8:], `{"x":`) + chunk(1, 0, b[10:], `{}`) + chunk(0, 0, "", `"Bash"}`) + "data: [DONE]\n\n"
	got, err := io.ReadAll(newDroidToolNameSSEReader(&droidChunkReader{data: []byte(wire), chunk: 1}, names))
	require.NoError(t, err)
	require.NotContains(t, string(got), "sub2api_droid_tool_")
	var emitted []string
	for _, line := range strings.Split(string(got), "\n") {
		if strings.HasPrefix(line, "data: {") {
			emitted = append(emitted, gjson.Get(line[6:], "choices.0.delta.tool_calls.0.function.name").String())
		}
	}
	require.Equal(t, []string{"Bash", "Agent", "", "", ""}, emitted)
	require.Contains(t, string(got), `\"Bash\"`)
	// A real caller tool may itself be a prefix of an internal alias.
	literal := chunk(0, 0, "sub2api_", "") + chunk(0, 0, "", "{}") + "data: [DONE]\n\n"
	got, err = io.ReadAll(newDroidToolNameSSEReader(io.NopCloser(strings.NewReader(literal)), names))
	require.NoError(t, err)
	require.Contains(t, string(got), `"name":"sub2api_"`)
}

func TestDroidToolNameSSEFlushAndIncompleteStream(t *testing.T) {
	names := toolNamesForTest(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	wrapped := newDroidToolNameSSEReader(reader, names)
	defer wrapped.Close()
	frame := fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"name\":%q}}\n\n", names.forward["Bash"])
	go func() { _, _ = io.WriteString(writer, frame) }()
	done := make(chan []byte, 1)
	go func() { buf := make([]byte, 4096); n, _ := wrapped.Read(buf); done <- buf[:n] }()
	select {
	case first := <-done:
		require.Contains(t, string(first), `"name":"Bash"`)
	case <-time.After(2 * time.Second):
		t.Fatal("complete first event waited for another event or EOF")
	}
	partial := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"sub2api_\"}}]}}]}\n\n"
	got, err := io.ReadAll(newDroidToolNameSSEReader(io.NopCloser(strings.NewReader(partial)), names))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Empty(t, got)
	errorFrame := "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"upstream failed\"}}\n\n"
	got, err = io.ReadAll(newDroidToolNameSSEReader(io.NopCloser(strings.NewReader(partial+errorFrame)), names))
	require.NoError(t, err)
	require.Equal(t, errorFrame, string(got), "incomplete names must not hide upstream errors")
	oversized := bytes.Repeat([]byte("a"), droidErrorJSONMaxBytes+1)
	_, err = io.ReadAll(newDroidToolNameSSEReader(io.NopCloser(bytes.NewReader(oversized)), names))
	require.Error(t, err)
}

func TestDroidToolNamesPreparedRequestRetry(t *testing.T) {
	payload := `{"model":"claude-opus-4-6","tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"diagnostic"}]}`
	req, err := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/a/v1/messages", strings.NewReader(payload))
	require.NoError(t, err)
	prepared, err := prepareDroidRequest(req, droidTestAccount())
	require.NoError(t, err)
	defer prepared.Body.Close()
	retried, err := prepareDroidRequest(prepared, droidTestAccount())
	require.NoError(t, err)
	defer retried.Body.Close()
	names := retried.Context().Value(droidToolNamesKey{}).(*droidToolNames)
	require.Equal(t, "Bash", names.reverse[names.forward["Bash"]])
	response := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"content":[{"type":"tool_use","name":%q}]}`, names.forward["Bash"])))}
	require.NoError(t, restoreDroidToolNamesResponse(retried, response))
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "Bash", gjson.GetBytes(data, "content.0.name").String())
}

func TestDroidToolNamesServerSearchReferences(t *testing.T) {
	request := []byte(`{"tools":[{"name":"Bash","defer_loading":true},{"type":"tool_search_tool_regex_20251119","name":"tool_search"}],"messages":[{"role":"assistant","content":[{"type":"server_tool_use","name":"tool_search","input":{"pattern":"Bash"}},{"type":"tool_search_tool_result","content":[{"type":"tool_reference","tool_name":"Bash"}]}]}]}`)
	prepared, names, err := prepareDroidToolNames(request, APIProtocolAnthropic)
	require.NoError(t, err)
	require.Equal(t, names.forward["Bash"], gjson.GetBytes(prepared, "messages.0.content.1.content.0.tool_name").String())
	require.Equal(t, "tool_search", gjson.GetBytes(prepared, "messages.0.content.0.name").String())
	require.Equal(t, "Bash", gjson.GetBytes(prepared, "messages.0.content.0.input.pattern").String())
	response := []byte(fmt.Sprintf(`{"type":"content_block_start","content_block":{"type":"tool_search_tool_result","tool_use_id":"search1","content":[{"type":"tool_reference","tool_name":%q}]}}`, names.forward["Bash"]))
	restored := restoreDroidToolNamesJSON(response, names)
	require.Equal(t, "Bash", gjson.GetBytes(restored, "content_block.content.0.tool_name").String())
	require.Equal(t, "search1", gjson.GetBytes(restored, "content_block.tool_use_id").String())
}
