package apicompat

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicResponsesSignedThinkingOptInToolRoundTrip(t *testing.T) {
	opts := &AnthropicResponsesOptions{PreserveThinkingSignatures: true}
	for _, model := range []string{"claude-sonnet-4-6", "claude-opus-4-6", "claude-haiku-4-5-20251001"} {
		t.Run(model, func(t *testing.T) {
			blocks := []AnthropicContentBlock{
				{Type: "thinking", Thinking: "Check the current status.", Signature: "test-signed-thinking"},
				{Type: "redacted_thinking", Data: "test-opaque-data"},
				{Type: "text", Text: "Checking now."},
				{Type: "tool_use", ID: "toolu_lookup", Name: "lookup", Input: json.RawMessage(`{"query":"status"}`)},
			}
			response := AnthropicToResponsesResponseWithOptions(&AnthropicResponse{
				Model: model, Content: blocks, StopReason: AnthropicStopReasonPtr("tool_use"),
			}, opts)
			require.Len(t, response.Output, 4)
			require.Equal(t, "reasoning", response.Output[0].Type)
			require.NotEmpty(t, response.Output[0].EncryptedContent)
			require.Equal(t, "reasoning", response.Output[1].Type)
			require.NotEmpty(t, response.Output[1].EncryptedContent)
			require.Equal(t, "message", response.Output[2].Type)
			require.Equal(t, "function_call", response.Output[3].Type)

			// Simulate a client replaying the wire response plus its tool result.
			wire, err := json.Marshal(response.Output)
			require.NoError(t, err)
			var items []ResponsesInputItem
			require.NoError(t, json.Unmarshal(wire, &items))
			items = append(items, ResponsesInputItem{
				Type: "function_call_output", CallID: response.Output[3].CallID, Output: "ready",
			})
			input, err := json.Marshal(items)
			require.NoError(t, err)
			request, err := ResponsesToAnthropicRequestWithOptions(&ResponsesRequest{
				Model: model, Input: input,
			}, opts)
			require.NoError(t, err)
			require.Len(t, request.Messages, 2)
			require.Equal(t, "assistant", request.Messages[0].Role)
			wantBlocks, err := json.Marshal(blocks)
			require.NoError(t, err)
			require.JSONEq(t, string(wantBlocks), string(request.Messages[0].Content))
			require.Equal(t, "user", request.Messages[1].Role)
			require.JSONEq(t, `[{"type":"tool_result","tool_use_id":"toolu_lookup","content":"ready"}]`, string(request.Messages[1].Content))
		})
	}
}

func TestAnthropicResponsesThinkingOptionsKeepLegacyDefault(t *testing.T) {
	response := &AnthropicResponse{Model: "claude-sonnet-4-6", Content: []AnthropicContentBlock{
		{Type: "thinking", Thinking: "A plan", Signature: "test-signature"},
		{Type: "text", Text: "Hello"},
	}}
	for _, opts := range []*AnthropicResponsesOptions{nil, {}} {
		converted := AnthropicToResponsesResponseWithOptions(response, opts)
		require.Empty(t, converted.Output[0].EncryptedContent)
		require.Equal(t, "A plan", converted.Output[0].Summary[0].Text)
	}
	require.Empty(t, AnthropicToResponsesResponse(response).Output[0].EncryptedContent)

	opts := &AnthropicResponsesOptions{PreserveThinkingSignatures: true}
	converted := AnthropicToResponsesResponseWithOptions(response, opts)
	input, err := json.Marshal(converted.Output)
	require.NoError(t, err)
	request, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: response.Model, Input: input})
	require.NoError(t, err)
	require.Len(t, request.Messages, 1)
	require.NotContains(t, string(request.Messages[0].Content), "test-signature")
	require.NotContains(t, string(request.Messages[0].Content), `"thinking"`)
}

func TestAnthropicResponsesThinkingOptInRejectsInvalidEnvelopes(t *testing.T) {
	opts := &AnthropicResponsesOptions{PreserveThinkingSignatures: true}
	for _, payload := range []string{
		"!",
		base64.RawStdEncoding.EncodeToString([]byte(`not JSON`)),
		base64.RawStdEncoding.EncodeToString([]byte(`{"type":"thinking","thinking":"unsigned"}`)),
		base64.RawStdEncoding.EncodeToString([]byte(`{"type":"text","signature":"test-signature"}`)),
	} {
		input, err := json.Marshal([]ResponsesInputItem{{Type: "reasoning", EncryptedContent: anthropicThinkingEnvelopePrefix + payload}})
		require.NoError(t, err)
		_, err = ResponsesToAnthropicRequestWithOptions(&ResponsesRequest{Model: "claude-sonnet-4-6", Input: input}, opts)
		require.Error(t, err)
	}

	request, err := ResponsesToAnthropicRequestWithOptions(&ResponsesRequest{
		Model: "claude-sonnet-4-6",
		Input: json.RawMessage(`[{"type":"reasoning","encrypted_content":"unrelated-openai-ciphertext"},{"role":"user","content":"hello"}]`),
	}, opts)
	require.NoError(t, err)
	require.Len(t, request.Messages, 1)
	require.Equal(t, "user", request.Messages[0].Role)
	require.NotContains(t, string(request.Messages[0].Content), "ciphertext")
}
