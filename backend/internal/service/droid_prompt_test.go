//go:build unit

package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDroidPromptNoticesInPromptAndToolResults(t *testing.T) {
	const summary = "\n\n<summary>\n## 已完成\n- 保留工具结果、换行与 emoji：✅\n- Checkpoint: SUM-7319\n- JSON: {\"number\":9007199254740993}\n\n## 下一步\n继续检查 /workspace/project。\n</summary>\n"
	for _, tc := range []struct{ name, notice, text, want string }{
		{
			"environment", "You have been invoked in the following environment:",
			"# Environment\nYou have been invoked in the following environment:\n - Working directory: /workspace/project\n - Shell: zsh",
			"# Environment\n\n - Working directory: /workspace/project\n - Shell: zsh",
		},
		{
			"compaction", "The conversation history before this point was compacted into the following summary:",
			"The conversation history before this point was compacted into the following summary:" + summary,
			"[Conversation summary]" + summary,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDroidPromptNoticeLocations(t, tc.notice, tc.text, tc.want)
		})
	}
}

func testDroidPromptNoticeLocations(t *testing.T, notice, text, want string) {
	t.Helper()
	quoted, err := json.Marshal(text)
	require.NoError(t, err)
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

func TestDroidPromptNoticesPreserveStructuredAndSignedFields(t *testing.T) {
	for _, tc := range []struct{ name, notice, replacement string }{
		{"environment", "You have been invoked in the following environment:", ""},
		{"compaction", "The conversation history before this point was compacted into the following summary:", "[Conversation summary]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDroidPromptPreservesStructuredFields(t, tc.notice, tc.replacement)
		})
	}
}

func testDroidPromptPreservesStructuredFields(t *testing.T, notice, replacement string) {
	t.Helper()
	payload := `{"model":"claude-opus-5-5","system":[{"type":"text","text":"# Environment\nYou have been invoked in the following environment:\nkeep this","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"You have been invoked in the following environment:"},"tools":[{"name":"lookup","description":"You have been invoked in the following environment:","input_schema":{"type":"object","maximum":9007199254740993}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"You have been invoked in the following environment:","signature":"keep-signature"},{"type":"redacted_thinking","data":"signed-data"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"query":"You have been invoked in the following environment:","number":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":[{"type":"text","text":"before You have been invoked in the following environment: after","cache_control":{"type":"ephemeral"}},{"type":"image","source":{"type":"url","url":"https://example.com/image.png"}},{"type":"document","source":{"type":"text","media_type":"text/plain","data":"You have been invoked in the following environment:"}}]}]}]}`
	payload = strings.ReplaceAll(payload, "You have been invoked in the following environment:", notice)
	out, err := prepareDroidBody([]byte(payload), APIProtocolAnthropic)
	require.NoError(t, err)
	require.Equal(t, "before "+replacement+" after", gjson.GetBytes(out, "messages.1.content.0.content.0.text").String())
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
		tc.payload = strings.ReplaceAll(tc.payload, "You have been invoked in the following environment:", notice)
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
		require.NoError(t, json.Unmarshal(normalizeDroidPromptText(raw), &text))
		require.Equal(t, "[Environment]", text)
	}
	for _, raw := range []string{`"Keep all of these instructions"`, `"# Environment\nWorking directory: /workspace"`, `null`, `42`, `{}`, `[]`} {
		require.Equal(t, raw, string(normalizeDroidPromptText(json.RawMessage(raw))))
	}
}

func TestDroidCompactionNoticeExactMatch(t *testing.T) {
	const notice = "The conversation history before this point was compacted into the following summary:"
	for _, tc := range []struct{ text, want string }{
		{notice, "[Conversation summary]"},
		{"before\n" + notice + "\nafter", "before\n[Conversation summary]\nafter"},
		{notice + "\n" + notice, "[Conversation summary]\n[Conversation summary]"},
		{"The conversation history before this point was compacted into the following summary", "The conversation history before this point was compacted into the following summary"},
		{"An unrelated conversation summary must be preserved.", "An unrelated conversation summary must be preserved."},
	} {
		raw, err := json.Marshal(tc.text)
		require.NoError(t, err)
		var text string
		require.NoError(t, json.Unmarshal(normalizeDroidPromptText(raw), &text))
		require.Equal(t, tc.want, text)
	}
}

func TestDroidPromptNoticesOnlyChangeFactoryOutboundCopy(t *testing.T) {
	const payload = `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"You have been invoked in the following environment:\nWorking directory: /workspace\nThe conversation history before this point was compacted into the following summary:\n\n<summary>Checkpoint: SUM-7319</summary>"}],"max_tokens":16}`
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
					require.Equal(t, "\nWorking directory: /workspace\n[Conversation summary]\n\n<summary>Checkpoint: SUM-7319</summary>", gjson.GetBytes(body, "messages.0.content").String())
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
