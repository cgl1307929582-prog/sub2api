package service

import (
	"bytes"
	"encoding/json"
	"strings"
)

const (
	droidEnvironmentNotice  = "You have been invoked in the following environment"
	droidCompactionNotice   = "The conversation history before this point was compacted into the following summary:"
	droidClaudeCodeIdentity = "You are Claude Code, Anthropic's official CLI for Claude"
	droidContinuationNotice = "This session is being continued from a previous conversation that ran out of context."
	droidModelNameNotice    = "You are powered by the model named"
	droidModelIDNotice      = "The exact model ID is"
	droidTaskTrackingNotice = "If you're working on tasks that would benefit from tracking progress, consider using TaskCreate to add new tasks and TaskUpdate to update task status (set to in_progress when starting, completed when done)."
	droidTaskTrackingText   = "If tracking task progress would help, use TaskCreate for new tasks and TaskUpdate for status changes (in_progress when starting, completed when done)."
	droidHookSetupNotice    = `Automated behaviors ("from now on when X", "each time X", "whenever X", "before/after X") require hooks configured in settings.json - the harness executes these, not Claude, so memory/preferences cannot fulfill them.`
	droidHookSetupText      = `Use hooks in settings.json for recurring or event-triggered actions ("from now on when X", "each time X", "whenever X", "before/after X"). These actions are run by the harness; storing a memory or preference does not configure a hook.`
)

var droidPromptNoticeReplacer = strings.NewReplacer(
	droidClaudeCodeIdentity+", running within the Claude Agent SDK.", "You are Claude Code. Running within the Claude Agent SDK.",
	"You are Claude Code, running within the Claude Agent SDK.", "You are Claude Code. Running within the Claude Agent SDK.",
	droidEnvironmentNotice+":", "",
	droidEnvironmentNotice, "",
	droidCompactionNotice, "[Conversation summary]",
	droidClaudeCodeIdentity, "You are Claude Code",
	droidContinuationNotice, "[Conversation summary]",
	droidModelNameNotice, "Selected model:",
	droidModelIDNotice, "Model ID:",
	droidTaskTrackingNotice, droidTaskTrackingText,
	droidHookSetupNotice, droidHookSetupText,
)

// Factory rejects these fixed preambles even in quoted diagnostic output.
// Normalize only ordinary prompt text, retaining environment details, model
// identifiers, task instructions, and the full compaction summary. Never rewrite
// tool arguments, signatures, encrypted reasoning, schemas, metadata, or
// multimodal data.
func normalizeDroidPromptNotice(fields map[string]json.RawMessage, protocol string) {
	normalizeDroidPromptField(fields, "tools", normalizeDroidWidgetToolDescription)
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
	if json.Unmarshal(raw, &text) != nil {
		return raw
	}
	normalized := droidPromptNoticeReplacer.Replace(text)
	if normalized == text {
		return raw
	}
	text = normalized
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

// Normalize only the exact Desktop widget description rejected by Factory.
// The tool name, input schema, permissions, and all other descriptions survive.
const droidWidgetDescription = "Read context from an embedded interactive widget. Widgets are rendered alongside chat from prior tool calls and can be interacted with by the user. Call this when you need to know the current state of a widget."
const droidWidgetDescriptionText = "Get the current state of an embedded interactive widget. These widgets appear next to chat after earlier tool calls and the user can interact with them. Use this tool to inspect a widget's current state."

func normalizeDroidWidgetToolDescription(raw json.RawMessage) json.RawMessage {
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return raw
	}
	changed := false
	for i, tool := range tools {
		var fields map[string]json.RawMessage
		if json.Unmarshal(tool, &fields) != nil {
			continue
		}
		definition := fields
		var nested map[string]json.RawMessage
		if f, ok := fields["function"]; ok && json.Unmarshal(f, &nested) == nil && nested != nil {
			definition = nested
		}
		var name, description string
		_ = json.Unmarshal(definition["name"], &name)
		_ = json.Unmarshal(definition["description"], &description)
		if name != "mcp__ccd_session__read_widget_context" || description != droidWidgetDescription {
			continue
		}
		definition["description"], _ = json.Marshal(droidWidgetDescriptionText)
		if nested != nil {
			fields["function"], _ = json.Marshal(nested)
		}
		tools[i], _ = json.Marshal(fields)
		changed = true
	}
	if !changed {
		return raw
	}
	cleaned, _ := json.Marshal(tools)
	return cleaned
}
