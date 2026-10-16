package service

import (
	"bytes"
	"encoding/json"
	"strings"
)

const droidEnvironmentNotice = "You have been invoked in the following environment"

// Factory rejects this environment preamble even when it appears in quoted
// diagnostic output. Remove only that fixed sentence from ordinary prompt text;
// retain the environment details and never rewrite tool arguments, signatures,
// encrypted reasoning, schemas, metadata, or multimodal data.
func stripDroidEnvironmentNotice(fields map[string]json.RawMessage, protocol string) {
	switch protocol {
	case APIProtocolAnthropic:
		stripDroidEnvironmentField(fields, "system", stripDroidEnvironmentContent)
		stripDroidEnvironmentField(fields, "messages", stripDroidEnvironmentMessages)
	case APIProtocolResponses:
		stripDroidEnvironmentField(fields, "instructions", stripDroidEnvironmentText)
		stripDroidEnvironmentField(fields, "input", stripDroidEnvironmentInput)
	case APIProtocolChatCompletions:
		stripDroidEnvironmentField(fields, "messages", stripDroidEnvironmentMessages)
	}
}

// Return unchanged RawMessages as-is, including large numeric tool parameters.
func stripDroidEnvironmentField(fields map[string]json.RawMessage, name string, strip func(json.RawMessage) json.RawMessage) bool {
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

func stripDroidEnvironmentText(raw json.RawMessage) json.RawMessage {
	var text string
	if json.Unmarshal(raw, &text) != nil || !strings.Contains(text, droidEnvironmentNotice) {
		return raw
	}
	text = strings.ReplaceAll(text, droidEnvironmentNotice+":", "")
	text = strings.ReplaceAll(text, droidEnvironmentNotice, "")
	if strings.TrimSpace(text) == "" {
		// Anthropic text blocks must be nonempty. Retain a neutral heading if
		// the entire block was only the removed preamble.
		text = "[Environment]"
	}
	cleaned, _ := json.Marshal(text)
	return cleaned
}

func stripDroidEnvironmentContent(raw json.RawMessage) json.RawMessage {
	return stripDroidEnvironmentContentBlocks(raw, true)
}

func stripDroidEnvironmentContentBlocks(raw json.RawMessage, allowToolResult bool) json.RawMessage {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return stripDroidEnvironmentText(raw)
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
			blockChanged = stripDroidEnvironmentField(fields, "text", stripDroidEnvironmentText)
		case "tool_result":
			if allowToolResult {
				blockChanged = stripDroidEnvironmentField(fields, "content", func(content json.RawMessage) json.RawMessage {
					// Nested tool results are not valid content blocks.
					return stripDroidEnvironmentContentBlocks(content, false)
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

func stripDroidEnvironmentInput(raw json.RawMessage) json.RawMessage {
	cleaned := stripDroidEnvironmentText(raw)
	if !bytes.Equal(raw, cleaned) {
		return cleaned
	}
	return stripDroidEnvironmentMessages(raw)
}

func stripDroidEnvironmentMessages(raw json.RawMessage) json.RawMessage {
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
			messageChanged = stripDroidEnvironmentField(fields, "content", stripDroidEnvironmentContent)
		case "function_call_output":
			messageChanged = stripDroidEnvironmentField(fields, "output", stripDroidEnvironmentContent)
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
