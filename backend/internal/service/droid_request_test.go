//go:build unit

package service

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDroidRequestIdentityPreservesCallerPayload(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, payload, systemPath, preservedPath string
	}{
		{"messages_string", APIProtocolAnthropic, `{"system":"caller instructions","tools":[{"name":"lookup","input_schema":{"maximum":9007199254740993}}]}`, "system", "tools"},
		{"messages_blocks", APIProtocolAnthropic, `{"system":[{"type":"text","text":"caller instructions","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"keep-signature"}]}]}`, "system.0.text", "messages"},
		{"responses", APIProtocolResponses, `{"instructions":"caller instructions","input":[{"type":"function_call_output","call_id":"call_1","output":"result"}]}`, "instructions", "input"},
		{"chat", APIProtocolChatCompletions, `{"messages":[{"role":"developer","content":"caller instructions"},{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`, "messages.0.content", "tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(tc.payload)
			prepared, err := prepareDroidBody(original, tc.protocol)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(gjson.GetBytes(prepared, tc.systemPath).String(), droidSystemPrompt))
			require.JSONEq(t, gjson.GetBytes(original, tc.preservedPath).Raw, gjson.GetBytes(prepared, tc.preservedPath).Raw)
			require.Contains(t, string(prepared), "caller instructions")
			require.Equal(t, tc.payload, string(original))
			if tc.name == "messages_blocks" {
				require.JSONEq(t, gjson.GetBytes(original, "system.0").Raw, gjson.GetBytes(prepared, "system.1").Raw)
			}
			if tc.name == "messages_string" {
				require.Contains(t, string(prepared), "9007199254740993")
			}
			if tc.name == "chat" {
				require.JSONEq(t, gjson.GetBytes(original, "messages.0").Raw, gjson.GetBytes(prepared, "messages.1").Raw)
				require.JSONEq(t, gjson.GetBytes(original, "messages.1").Raw, gjson.GetBytes(prepared, "messages.2").Raw)
			}
			// Retries must not grow the identity prefix or shift cache blocks again.
			again, err := prepareDroidBody(prepared, tc.protocol)
			require.NoError(t, err)
			require.JSONEq(t, string(prepared), string(again))
		})
	}
}

func TestDroidRequestBodyAndClientHeadersAgree(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5-20251001","system":"Keep my instructions","messages":[]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.factory.ai/api/llm/a/v1/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Client-Version", "untrusted-client-version")
	req.Header.Set("Content-Length", "1")
	prepared, err := prepareDroidRequest(req, droidTestAccount())
	require.NoError(t, err)
	defer prepared.Body.Close()
	actual, err := io.ReadAll(prepared.Body)
	require.NoError(t, err)
	require.Equal(t, int64(len(actual)), prepared.ContentLength)
	require.Empty(t, prepared.Header.Get("Content-Length"))
	require.Equal(t, "factory-cli/"+droidClientVersion, prepared.Header.Get("User-Agent"))
	require.Equal(t, droidClientVersion, prepared.Header.Get("X-Client-Version"))
	replayed, err := prepared.GetBody()
	require.NoError(t, err)
	defer replayed.Close()
	replay, err := io.ReadAll(replayed)
	require.NoError(t, err)
	require.Equal(t, actual, replay)
	original, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, body, original)
	require.Equal(t, "untrusted-client-version", req.Header.Get("X-Client-Version"))

	account := droidTestAccount()
	account.Credentials["user_agent"] = "factory-cli/0.243.0"
	updated, err := prepareDroidRequest(req, account)
	require.NoError(t, err)
	defer updated.Body.Close()
	require.Equal(t, "0.243.0", updated.Header.Get("X-Client-Version"))
}

func TestDroidRequestRejectsMalformedInstructionFields(t *testing.T) {
	for _, tc := range []struct{ protocol, payload string }{
		{APIProtocolAnthropic, `{"system":{"text":"invalid"}}`},
		{APIProtocolResponses, `{"instructions":["invalid"]}`},
		{APIProtocolChatCompletions, `{"messages":"invalid"}`},
	} {
		_, err := prepareDroidBody([]byte(tc.payload), tc.protocol)
		require.Error(t, err)
	}
}

func TestDroidClaudeEffortUsesModelCapabilities(t *testing.T) {
	for _, model := range []string{"claude-haiku-4-5-20251001", "claude-sonnet-4-5", "claude-sonnet-4-6", "claude-opus-4-6"} {
		for _, effort := range []string{"low", "medium"} {
			request := &apicompat.ResponsesRequest{Model: model, Input: []byte(`"hello"`), Reasoning: &apicompat.ResponsesReasoning{Effort: effort}}
			converted, err := convertDroidResponsesToAnthropic(droidTestAccount(), request)
			require.NoError(t, err)
			if strings.Contains(model, "4-5") {
				require.Nil(t, converted.OutputConfig, model)
			} else {
				require.Equal(t, effort, converted.OutputConfig.Effort, model)
			}
			if effort == "medium" {
				require.Equal(t, "enabled", converted.Thinking.Type)
				require.Positive(t, converted.Thinking.BudgetTokens)
			} else {
				require.Nil(t, converted.Thinking)
			}
		}
	}
}
