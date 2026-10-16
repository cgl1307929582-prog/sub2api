//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDroidEnvironmentNoticeInPromptAndToolResults(t *testing.T) {
	const notice = "You have been invoked in the following environment:"
	const environment = "# Environment\n" + notice + "\n - Working directory: /workspace/project\n - Shell: zsh"
	quoted, err := json.Marshal(environment)
	require.NoError(t, err)
	want := "# Environment\n\n - Working directory: /workspace/project\n - Shell: zsh"
	for _, tc := range []struct {
		name, protocol, payload, path string
	}{
		{"messages_system_string", APIProtocolAnthropic, `{"system":` + string(quoted) + `}`, "system"},
		{"messages_system_block", APIProtocolAnthropic, `{"system":[{"type":"text","text":` + string(quoted) + `,"cache_control":{"type":"ephemeral"}}]}`, "system.1.text"},
		{"messages_user", APIProtocolAnthropic, `{"messages":[{"role":"user","content":` + string(quoted) + `}]}`, "messages.0.content"},
		{"messages_tool_string", APIProtocolAnthropic, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":` + string(quoted) + `}]}]}`, "messages.0.content.0.content"},
		{"messages_tool_block", APIProtocolAnthropic, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":` + string(quoted) + `,"cache_control":{"type":"ephemeral"}}]}]}]}`, "messages.0.content.0.content.0.text"},
		{"responses_instructions", APIProtocolResponses, `{"instructions":` + string(quoted) + `}`, "instructions"},
		{"responses_input_string", APIProtocolResponses, `{"input":` + string(quoted) + `}`, "input"},
		{"responses_message", APIProtocolResponses, `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(quoted) + `}]}]}`, "input.0.content.0.text"},
		{"responses_tool_string", APIProtocolResponses, `{"input":[{"type":"function_call_output","call_id":"call_1","output":` + string(quoted) + `}]}`, "input.0.output"},
		{"responses_tool_block", APIProtocolResponses, `{"input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":` + string(quoted) + `}]}]}`, "input.0.output.0.text"},
		{"chat_developer", APIProtocolChatCompletions, `{"messages":[{"role":"developer","content":` + string(quoted) + `}]}`, "messages.1.content"},
		{"chat_tool", APIProtocolChatCompletions, `{"messages":[{"role":"tool","tool_call_id":"call_1","content":` + string(quoted) + `}]}`, "messages.1.content"},
		{"chat_text_block", APIProtocolChatCompletions, `{"messages":[{"role":"user","content":[{"type":"text","text":` + string(quoted) + `}]}]}`, "messages.1.content.0.text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(tc.payload)
			out, err := prepareDroidBody(payload, tc.protocol)
			require.NoError(t, err)
			expected := want
			if tc.path == "system" || tc.path == "instructions" {
				expected = droidSystemPrompt + "\n\n" + want
			}
			require.Equal(t, expected, gjson.GetBytes(out, tc.path).String())
			require.NotContains(t, string(out), notice)
			require.Equal(t, tc.payload, string(payload), "caller request must remain unchanged")
			again, err := prepareDroidBody(out, tc.protocol)
			require.NoError(t, err)
			require.JSONEq(t, string(out), string(again), "retry must be idempotent")
		})
	}
}

func TestDroidEnvironmentNoticePreservesStructuredAndSignedFields(t *testing.T) {
	const payload = `{"model":"claude-opus-5-5","system":[{"type":"text","text":"# Environment\nYou have been invoked in the following environment:\nkeep this","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"You have been invoked in the following environment:"},"tools":[{"name":"lookup","description":"You have been invoked in the following environment:","input_schema":{"type":"object","maximum":9007199254740993}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"You have been invoked in the following environment:","signature":"keep-signature"},{"type":"redacted_thinking","data":"signed-data"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"query":"You have been invoked in the following environment:","number":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":[{"type":"text","text":"before You have been invoked in the following environment: after","cache_control":{"type":"ephemeral"}},{"type":"image","source":{"type":"url","url":"https://example.com/image.png"}},{"type":"document","source":{"type":"text","media_type":"text/plain","data":"You have been invoked in the following environment:"}}]}]}]}`
	out, err := prepareDroidBody([]byte(payload), APIProtocolAnthropic)
	require.NoError(t, err)
	require.Equal(t, "before  after", gjson.GetBytes(out, "messages.1.content.0.content.0.text").String())
	for originalPath, preparedPath := range map[string]string{
		"system.0.cache_control":           "system.1.cache_control",
		"metadata":                         "metadata",
		"tools":                            "tools",
		"messages.0":                       "messages.0",
		"messages.1.content.0.tool_use_id": "messages.1.content.0.tool_use_id",
		"messages.1.content.0.is_error":    "messages.1.content.0.is_error",
		"messages.1.content.0.content.0.cache_control": "messages.1.content.0.content.0.cache_control",
		"messages.1.content.0.content.1":               "messages.1.content.0.content.1",
		"messages.1.content.0.content.2":               "messages.1.content.0.content.2",
	} {
		require.JSONEq(t, gjson.Get(payload, originalPath).Raw, gjson.GetBytes(out, preparedPath).Raw, originalPath)
	}
	require.Contains(t, string(out), "9007199254740993")

	for _, tc := range []struct{ protocol, payload, path string }{
		{APIProtocolResponses, `{"input":[{"type":"function_call","name":"lookup","arguments":"{\"text\":\"You have been invoked in the following environment:\"}"},{"type":"reasoning","encrypted_content":"signed-data","summary":[{"type":"summary_text","text":"You have been invoked in the following environment:"}]}]}`, "input"},
		{APIProtocolChatCompletions, `{"messages":[{"role":"assistant","content":null,"reasoning_content":"You have been invoked in the following environment:","tool_calls":[{"type":"function","function":{"name":"lookup","arguments":"{\"text\":\"You have been invoked in the following environment:\"}"}}]}]}`, "messages.1"},
	} {
		out, err = prepareDroidBody([]byte(tc.payload), tc.protocol)
		require.NoError(t, err)
		originalPath := "input"
		if tc.protocol == APIProtocolChatCompletions {
			originalPath = "messages.0"
		}
		require.JSONEq(t, gjson.Get(tc.payload, originalPath).Raw, gjson.GetBytes(out, tc.path).Raw)
	}
}

func TestDroidEnvironmentNoticeOnlyTextRemainsValid(t *testing.T) {
	for _, input := range []string{
		"You have been invoked in the following environment:",
		"You have been invoked in the following environment",
		"\nYou have been invoked in the following environment: \n",
	} {
		raw, err := json.Marshal(input)
		require.NoError(t, err)
		var text string
		require.NoError(t, json.Unmarshal(stripDroidEnvironmentText(raw), &text))
		require.Equal(t, "[Environment]", text)
	}
	for _, raw := range []string{`"Keep all of these instructions"`, `"# Environment\nWorking directory: /workspace"`, `null`, `42`, `{}`, `[]`} {
		require.Equal(t, raw, string(stripDroidEnvironmentText(json.RawMessage(raw))))
	}
}

func TestDroidEnvironmentNoticeOnlyChangesFactoryOutboundCopy(t *testing.T) {
	const payload = `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"You have been invoked in the following environment:\nWorking directory: /workspace"}],"max_tokens":16}`
	for _, platform := range []string{PlatformDroid, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			account := droidTestAccount()
			account.Platform = platform
			request, err := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/a/v1/messages", bytes.NewBufferString(payload))
			require.NoError(t, err)
			response, err := droidRoundTrip(request, account, func(outbound *http.Request) (*http.Response, error) {
				replay, err := outbound.GetBody()
				require.NoError(t, err)
				defer replay.Close()
				body, err := io.ReadAll(replay)
				require.NoError(t, err)
				require.Equal(t, int64(len(body)), outbound.ContentLength)
				if platform == PlatformDroid {
					require.NotContains(t, string(body), "You have been invoked")
					require.Contains(t, string(body), "Working directory: /workspace")
				} else {
					require.Same(t, request, outbound)
					require.Equal(t, payload, string(body))
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(`{}`))}, nil
			})
			require.NoError(t, err)
			defer response.Body.Close()
			original, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			require.Equal(t, payload, string(original))
		})
	}
}
