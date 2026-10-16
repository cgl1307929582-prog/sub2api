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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const droidGoogleTextSSE = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"reason\",\"thought\":true},{\"text\":\"OK\"}]}}]}\n\n" +
	"data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":20,\"candidatesTokenCount\":1,\"thoughtsTokenCount\":7,\"cachedContentTokenCount\":5,\"totalTokenCount\":28}}\n\n"

func droidAdapterResponse(data string, stream bool) *http.Response {
	ct := "application/json"
	if stream {
		ct = "text/event-stream"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(data))}
}

func TestDroidGeminiAllIngressesAndAdminTest(t *testing.T) {
	for _, ingress := range routingMatrixIngresses() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", ingress.name, stream), func(t *testing.T) {
				body := routingMatrixCase{ingress: ingress, model: "gemini-3.8-flash"}.body()
				if stream {
					body = bytes.ReplaceAll(body, []byte(`"stream":false`), []byte(`"stream":true`))
				}
				upstream := &httpUpstreamRecorder{resp: droidAdapterResponse(droidGoogleTextSSE, true)}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				c, rec := newTestContext()
				c.Request = httptest.NewRequest("POST", ingress.path, bytes.NewReader(body))
				require.NoError(t, ingress.forward(svc, c, droidTestAccount(), body))
				require.Equal(t, http.StatusOK, rec.Code)
				require.Contains(t, rec.Body.String(), "OK")
				require.NotContains(t, rec.Body.String(), `"error"`)
				require.Equal(t, droidGooglePath, upstream.lastReq.URL.Path)
				require.False(t, gjson.GetBytes(upstream.lastBody, "stream").Exists())
				require.Equal(t, droidSystemPrompt, gjson.GetBytes(upstream.lastBody, "systemInstruction.parts.0.text").String())
			})
		}
	}
	svc, upstream := adaptiveCNAccountTestService(droidTestAccount(), droidAdapterResponse(droidGoogleTextSSE, true))
	c, rec := newTestContext()
	require.NoError(t, svc.TestAccountConnection(c, 801, "gemini-3.8-flash", "hi", AccountTestModeDefault))
	require.Equal(t, droidGooglePath, upstream.lastReq.URL.Path)
	require.Contains(t, rec.Body.String(), `"success":true`)
	require.Contains(t, rec.Body.String(), "OK")
}

func TestDroidGeminiUsageIncludesThoughtsAndCache(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body := []byte(fmt.Sprintf(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"stream":%v,"stream_options":{"include_usage":true}}`, stream))
		upstream := &httpUpstreamRecorder{resp: droidAdapterResponse(droidGoogleTextSSE, true)}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		c, rec := newTestContext()
		result, err := svc.ForwardAsChatCompletions(context.Background(), c, droidTestAccount(), body, "", "")
		require.NoError(t, err)
		require.Equal(t, 20, result.Usage.InputTokens)
		require.Equal(t, 8, result.Usage.OutputTokens)
		require.Equal(t, 5, result.Usage.CacheReadInputTokens)
		require.Contains(t, rec.Body.String(), `"reasoning_tokens":7`)
	}
}

func TestDroidGeminiRequestOptionsAndLargeToolArguments(t *testing.T) {
	id := droidGoogleCallID("call_first", "opaque-signature+/=")
	body := []byte(fmt.Sprintf(`{"model":"gemini-3.1-pro-preview","max_tokens":99,"max_completion_tokens":123,"stream":true,"reasoning_effort":"none","temperature":0.2,"stop":"END","response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object","properties":{"n":{"type":"integer"}}}}},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"n":{"type":"integer"}}}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}},"messages":[{"role":"system","content":"caller"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},{"role":"assistant","tool_calls":[{"id":%q,"type":"function","function":{"name":"lookup","arguments":"{\"n\":9007199254740993}"}}]},{"role":"tool","tool_call_id":%q,"content":"found"}]}`, id, id))
	req, _ := http.NewRequest("POST", "https://api.factory.ai/api/llm/o/v1/chat/completions", bytes.NewReader(body))
	prepared, err := prepareDroidRequest(req, droidTestAccount())
	require.NoError(t, err)
	data, err := io.ReadAll(prepared.Body)
	require.NoError(t, err)
	require.Equal(t, droidGooglePath, prepared.URL.Path)
	require.Equal(t, "/api/llm/o/v1/chat/completions", req.URL.Path)
	require.Equal(t, int64(123), gjson.GetBytes(data, "generationConfig.maxOutputTokens").Int())
	require.Equal(t, "LOW", gjson.GetBytes(data, "generationConfig.thinkingConfig.thinkingLevel").String())
	require.Equal(t, "END", gjson.GetBytes(data, "generationConfig.stopSequences.0").String())
	require.Equal(t, "opaque-signature+/=", gjson.GetBytes(data, "contents.1.parts.0.thoughtSignature").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(data, "contents.1.parts.0.functionCall.args.n").Raw)
	require.Equal(t, "lookup", gjson.GetBytes(data, "contents.2.parts.0.functionResponse.name").String())
	require.Equal(t, "image/png", gjson.GetBytes(data, "contents.0.parts.1.inlineData.mimeType").String())
	require.Equal(t, "ANY", gjson.GetBytes(data, "toolConfig.functionCallingConfig.mode").String())
	require.True(t, gjson.GetBytes(data, "tools.0.functionDeclarations.0.parametersJsonSchema").Exists())
	require.False(t, gjson.GetBytes(data, "stream").Exists())
}

func TestDroidGeminiToolRoundTripAcrossIngresses(t *testing.T) {
	toolSSE := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"n\":9007199254740993}},\"thoughtSignature\":\"opaque+/=\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	for _, ingress := range routingMatrixIngresses() {
		t.Run(ingress.name, func(t *testing.T) {
			body := routingMatrixCase{ingress: ingress, model: "gemini-3.8-flash"}.body()
			upstream := &httpUpstreamRecorder{resp: droidAdapterResponse(toolSSE, true)}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c, rec := newTestContext()
			c.Request = httptest.NewRequest("POST", ingress.path, bytes.NewReader(body))
			require.NoError(t, ingress.forward(svc, c, droidTestAccount(), body))
			response := rec.Body.Bytes()
			var followup string
			switch ingress.name {
			case "responses":
				call := gjson.GetBytes(response, "output.#(type==function_call)")
				require.True(t, call.Exists(), string(response))
				followup = fmt.Sprintf(`{"model":"gemini-3.8-flash","input":[%s,{"type":"function_call_output","call_id":%q,"output":"found"}]}`, call.Raw, call.Get("call_id").String())
			case "messages":
				call := gjson.GetBytes(response, "content.#(type==tool_use)")
				require.True(t, call.Exists(), string(response))
				followup = fmt.Sprintf(`{"model":"gemini-3.8-flash","max_tokens":128,"messages":[{"role":"assistant","content":[%s]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"found"}]}]}`, call.Raw, call.Get("id").String())
			default:
				message := gjson.GetBytes(response, "choices.0.message")
				id := message.Get("tool_calls.0.id").String()
				require.NotEmpty(t, id, string(response))
				followup = fmt.Sprintf(`{"model":"gemini-3.8-flash","messages":[%s,{"role":"tool","tool_call_id":%q,"content":"found"}]}`, message.Raw, id)
			}
			upstream.resp = nil
			upstream.err = errors.New("capture only")
			c2 := adaptiveProtocolTestContext(ingress.path, []byte(followup))
			_ = ingress.forward(svc, c2, droidTestAccount(), []byte(followup))
			require.Equal(t, "opaque+/=", gjson.GetBytes(upstream.lastBody, "contents.0.parts.0.thoughtSignature").String(), string(upstream.lastBody))
			require.Equal(t, "9007199254740993", gjson.GetBytes(upstream.lastBody, "contents.0.parts.0.functionCall.args.n").Raw)
			require.Equal(t, "lookup", gjson.GetBytes(upstream.lastBody, "contents.1.parts.0.functionResponse.name").String())
		})
	}
}

func TestDroidMistralContentBlocksAndUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		field := "message"
		if stream {
			field = "delta"
		}
		raw := fmt.Sprintf(`{"id":"chat_1","object":"chat.completion","model":"mistral-large-4","choices":[{"index":0,%q:{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"reason"}]},{"type":"text","text":"OK"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28,"reasoning_tokens":7},"large":9007199254740993}`, field)
		data, err := normalizeDroidChatJSON([]byte(raw))
		require.NoError(t, err)
		require.Equal(t, "OK", gjson.GetBytes(data, "choices.0."+field+".content").String())
		require.Equal(t, "reason", gjson.GetBytes(data, "choices.0."+field+".reasoning_content").String())
		require.Equal(t, "9007199254740993", gjson.GetBytes(data, "large").Raw)
		require.Equal(t, int64(7), gjson.GetBytes(data, "usage.completion_tokens_details.reasoning_tokens").Int())
		for _, ingress := range routingMatrixIngresses() {
			wire := raw
			if stream {
				wire = "data: " + raw + "\n\ndata: [DONE]\n\n"
			}
			body := routingMatrixCase{ingress: ingress, model: "mistral-large-4"}.body()
			if stream {
				body = bytes.ReplaceAll(body, []byte(`"stream":false`), []byte(`"stream":true`))
			}
			upstream := &httpUpstreamRecorder{resp: droidAdapterResponse(wire, stream)}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c, rec := newTestContext()
			c.Request = httptest.NewRequest("POST", ingress.path, bytes.NewReader(body))
			require.NoError(t, ingress.forward(svc, c, droidTestAccount(), body), ingress.name)
			require.Contains(t, rec.Body.String(), "OK")
			require.Equal(t, "mistral", upstream.lastReq.Header.Get("X-API-Provider"))
		}
	}
}

func TestDroidMistralReasoningUsesSupportedLevels(t *testing.T) {
	for input, want := range map[string]string{"none": "none", "off": "none", "low": "high", "medium": "high", "high": "high", "xhigh": "high"} {
		req, _ := http.NewRequest("POST", "https://api.factory.ai/api/llm/o/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"mistral-medium-3.5","reasoning_effort":%q,"messages":[]}`, input)))
		prepared, err := prepareDroidRequest(req, droidTestAccount())
		require.NoError(t, err)
		body, err := io.ReadAll(prepared.Body)
		require.NoError(t, err)
		require.Equal(t, want, gjson.GetBytes(body, "reasoning_effort").String())
	}
}

func TestDroidGeminiStreamErrorsNeverComplete(t *testing.T) {
	for _, wire := range []string{
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n",
		"data: {\"error\":{\"message\":\"factory_test_secret\"}}\n\n",
		"data: {invalid}\n\n", "data: " + strings.Repeat("x", droidErrorJSONMaxBytes) + "\n\n",
		droidGoogleTextSSE + "data: {\"error\":{\"message\":\"factory_test_secret\"}}\n\n",
	} {
		state := newDroidGoogleStream("gemini-3.8-flash")
		r := newDroidChatSSEReader(io.NopCloser(strings.NewReader(wire)), state.transform, func() error {
			if !state.finished {
				return errors.New("incomplete")
			}
			return nil
		}, true)
		data, err := io.ReadAll(r)
		require.Error(t, err)
		require.NotContains(t, string(data), "[DONE]")
		require.NotContains(t, string(data), "factory_test_secret")
		require.Contains(t, string(data), `"error"`)
	}
}

func TestDroidChatStreamCloseUnblocksUpstreamRead(t *testing.T) {
	source, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	reader := newDroidChatSSEReader(source, normalizeDroidChatJSON, nil, false)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(reader)
		done <- err
	}()
	require.NoError(t, reader.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("closing a client stream left the upstream read blocked")
	}
}

func TestDroidGeminiFramesSplitAcrossReadsAndCRLF(t *testing.T) {
	for _, size := range []int{1, 7, 4096} {
		state := newDroidGoogleStream("gemini-3.8-flash")
		wire := strings.ReplaceAll(droidGoogleTextSSE, "\n", "\r\n")
		source := &droidChunkReader{data: []byte(wire), chunk: size}
		reader := newDroidChatSSEReader(source, state.transform, nil, true)
		data, err := bufferDroidGoogleChat(reader, state)
		require.NoError(t, err)
		require.True(t, json.Valid(data))
		require.Equal(t, "OK", gjson.GetBytes(data, "choices.0.message.content").String())
	}
}

func TestDroidNewFamiliesIgnoreStaleSavedProtocolRules(t *testing.T) {
	account := droidTestAccount()
	account.Credentials["protocol_rules"] = []any{map[string]any{"pattern": "*", "protocol": APIProtocolAnthropic}}
	for _, model := range DefaultDroidModelIDs() {
		protocol, _, _ := droidModelRoute(model)
		require.Equal(t, protocol, account.resolveModelRoutedProtocol(model))
	}
}

func TestDroidChatStreamsFailOnEventOnlyErrors(t *testing.T) {
	errorEvent := "event: error\ndata: {\"message\":\"generation failed\"}\n\n"
	newGoogle := func(wire string) (*droidGoogleStream, io.ReadCloser) {
		state := newDroidGoogleStream("gemini-3.8-flash")
		return state, newDroidChatSSEReader(io.NopCloser(strings.NewReader(wire)), state.transform, func() error {
			if !state.finished {
				return errors.New("incomplete")
			}
			return nil
		}, true)
	}
	for _, name := range []string{"error", "response.error", "response.failed"} {
		wire := droidGoogleTextSSE + strings.Replace(errorEvent, "event: error", "event: "+name, 1)
		_, reader := newGoogle(wire)
		data, err := io.ReadAll(reader)
		require.Error(t, err, name)
		require.NotContains(t, string(data), "[DONE]")
		require.Contains(t, string(data), `"error"`)

		state, reader := newGoogle(wire)
		_, err = bufferDroidGoogleChat(reader, state)
		require.Error(t, err, "buffered Gemini responses must not succeed after %s", name)
	}

	chunk := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"OK"}}]}` + "\n\n"
	reader := newDroidChatSSEReader(io.NopCloser(strings.NewReader(chunk+errorEvent+"data: [DONE]\n\n")), normalizeDroidChatJSON, nil, false)
	data, err := io.ReadAll(reader)
	require.Error(t, err)
	require.NotContains(t, string(data), "[DONE]")

	// An event name without data is never dispatched and must not leak into
	// the next event.
	state, reader := newGoogle("event: error\n\n" + droidGoogleTextSSE)
	data, err = bufferDroidGoogleChat(reader, state)
	require.NoError(t, err)
	require.Equal(t, "OK", gjson.GetBytes(data, "choices.0.message.content").String())
	reader = newDroidChatSSEReader(io.NopCloser(strings.NewReader("event: error\n\n"+chunk+"data: [DONE]\n\n")), normalizeDroidChatJSON, nil, false)
	data, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(data), "data: [DONE]")
}

func TestDroidChatStreamFailsOnErrorEventWithEmptyData(t *testing.T) {
	chunk := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"OK"}}]}` + "\n\n"
	reader := newDroidChatSSEReader(io.NopCloser(strings.NewReader(chunk+"event: error\ndata:\n\ndata: [DONE]\n\n")), normalizeDroidChatJSON, nil, false)
	data, err := io.ReadAll(reader)
	require.Error(t, err)
	require.NotContains(t, string(data), "[DONE]")

	// An empty data event without an error name is still ignored.
	reader = newDroidChatSSEReader(io.NopCloser(strings.NewReader(chunk+"data:\n\ndata: [DONE]\n\n")), normalizeDroidChatJSON, nil, false)
	data, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(data), "data: [DONE]")
}
