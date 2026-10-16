package service

import (
	"bytes"
	"encoding/json"
	"strings"
)

const (
	droidEnvironmentNotice = "You have been invoked in the following environment"
	droidCompactionNotice  = "The conversation history before this point was compacted into the following summary:"
)

// Factory rejects these fixed preambles even in quoted diagnostic output.
// Normalize only ordinary prompt text, retaining environment details and the
// full compaction summary. Never rewrite tool arguments, signatures, encrypted
// reasoning, schemas, metadata, or multimodal data.
func normalizeDroidPromptNotice(fields map[string]json.RawMessage, protocol string) {
	switch protocol {
	case APIProtocolAnthropic:
		normalizeDroidPromptField(fields, "system", normalizeDroidPromptContent)
		normalizeDroidPromptField(fields, "messages", normalizeDroidPromptMessages)
	case APIProtocolResponses:
		normalizeDroidPromptField(fields, "instructions", normalizeDroidPromptText)
		normalizeDroidPromptField(fields, "input", normalizeDroidPromptInput)
	case APIProtocolChatCompletions:
		normalizeDroidPromptField(fields, "messages", normalizeDroidPromptMessages)
	}
}

// Return unchanged RawMessages as-is, including large numeric tool parameters.
func normalizeDroidPromptField(fields map[string]json.RawMessage, name string, strip func(json.RawMessage) json.RawMessage) bool {
	raw, ok := fields[name]
	if !ok {
		return false
	}
	cleaned := strip(raw)
	if bytes.Equal(raw, cleaned) {
		return false
	}
	fields[name] = cleaned
	return true
}

func normalizeDroidPromptText(raw json.RawMessage) json.RawMessage {
	var text string
	if json.Unmarshal(raw, &text) != nil ||
		(!strings.Contains(text, droidEnvironmentNotice) && !strings.Contains(text, droidCompactionNotice)) {
		return raw
	}
	text = strings.ReplaceAll(text, droidEnvironmentNotice+":", "")
	text = strings.ReplaceAll(text, droidEnvironmentNotice, "")
	text = strings.ReplaceAll(text, droidCompactionNotice, "[Conversation summary]")
	if strings.TrimSpace(text) == "" {
		// Anthropic text blocks must be nonempty. Retain a neutral heading if
		// the entire block was only the removed preamble.
		text = "[Environment]"
	}
	cleaned, _ := json.Marshal(text)
	return cleaned
}

func normalizeDroidPromptContent(raw json.RawMessage) json.RawMessage {
	return normalizeDroidPromptContentBlocks(raw, true)
}

func normalizeDroidPromptContentBlocks(raw json.RawMessage, allowToolResult bool) json.RawMessage {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return normalizeDroidPromptText(raw)
	}
	changed := false
	for i, block := range blocks {
		var fields map[string]json.RawMessage
		if json.Unmarshal(block, &fields) != nil || fields == nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(fields["type"], &kind)
		blockChanged := false
		switch kind {
		case "text", "input_text", "output_text":
			blockChanged = normalizeDroidPromptField(fields, "text", normalizeDroidPromptText)
		case "tool_result":
			if allowToolResult {
				blockChanged = normalizeDroidPromptField(fields, "content", func(content json.RawMessage) json.RawMessage {
					// Nested tool results are not valid content blocks.
					return normalizeDroidPromptContentBlocks(content, false)
				})
			}
		}
		if blockChanged {
			blocks[i], _ = json.Marshal(fields)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	cleaned, _ := json.Marshal(blocks)
	return cleaned
}

func normalizeDroidPromptInput(raw json.RawMessage) json.RawMessage {
	cleaned := normalizeDroidPromptText(raw)
	if !bytes.Equal(raw, cleaned) {
		return cleaned
	}
	return normalizeDroidPromptMessages(raw)
}

func normalizeDroidPromptMessages(raw json.RawMessage) json.RawMessage {
	var messages []json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return raw
	}
	changed := false
	for i, message := range messages {
		var fields map[string]json.RawMessage
		if json.Unmarshal(message, &fields) != nil || fields == nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(fields["type"], &kind)
		messageChanged := false
		switch kind {
		case "", "message":
			messageChanged = normalizeDroidPromptField(fields, "content", normalizeDroidPromptContent)
		case "function_call_output":
			messageChanged = normalizeDroidPromptField(fields, "output", normalizeDroidPromptContent)
		}
		if messageChanged {
			messages[i], _ = json.Marshal(fields)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	cleaned, _ := json.Marshal(messages)
	return cleaned
}
