package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// These names need a transport-local namespace on Factory. Keep the mapping
// deterministic so repeated requests and restored client histories share names.
var droidClientToolNames = [...]string{
	"Agent", "Bash", "EnterPlanMode", "ExitPlanMode", "NotebookEdit",
	"TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "Write",
}

type droidToolNamesKey struct{}

type droidToolNames struct {
	forward map[string]string
	reverse map[string]string
	literal map[string]bool
}

func prepareDroidToolNames(body []byte, protocol string) ([]byte, *droidToolNames, error) {
	if !gjson.ValidBytes(body) {
		return nil, nil, errors.New("invalid Factory tool request")
	}
	fields := droidRequestToolNameFields(body, protocol)
	occupied := make(map[string]bool, len(fields))
	for _, field := range fields {
		occupied[field.String()] = true
	}
	names := &droidToolNames{forward: map[string]string{}, reverse: map[string]string{}, literal: occupied}
	for index, name := range droidClientToolNames {
		if !occupied[name] {
			continue
		}
		base := fmt.Sprintf("sub2api_droid_tool_%02d", index)
		alias := base
		for suffix := 1; droidToolAliasConflicts(alias, occupied); suffix++ {
			alias = fmt.Sprintf("%s_%d", base, suffix)
		}
		names.forward[name], names.reverse[alias] = alias, name
		occupied[alias] = true
	}
	if len(names.forward) == 0 {
		return body, nil, nil
	}
	return replaceDroidToolNameFields(body, fields, names.forward), names, nil
}

// A generated alias must not shadow a caller tool or prefix a different name.
// The latter also makes complete aliases unambiguous in fragmented Chat SSE.
func droidToolAliasConflicts(alias string, occupied map[string]bool) bool {
	for name := range occupied {
		if strings.HasPrefix(name, alias) {
			return true
		}
	}
	return false
}

func droidRequestToolNameFields(body []byte, protocol string) []gjson.Result {
	root := gjson.ParseBytes(body)
	var fields []gjson.Result
	add := func(name gjson.Result) {
		if name.Type == gjson.String && name.String() != "" {
			fields = append(fields, name)
		}
	}
	var tools func(gjson.Result)
	tools = func(value gjson.Result) {
		value.ForEach(func(_, tool gjson.Result) bool {
			typ := tool.Get("type").String()
			if shouldMimicToolName(typ) {
				add(tool.Get("name"))
				add(tool.Get("function.name"))
			} else if typ == "namespace" {
				tools(tool.Get("tools"))
			}
			return true
		})
	}
	tools(root.Get("tools"))
	choice := root.Get("tool_choice")
	switch choice.Get("type").String() {
	case "tool", "function", "custom":
		add(choice.Get("name"))
		add(choice.Get("function.name"))
	case "allowed_tools":
		tools(choice.Get("tools"))
	}
	switch protocol {
	case APIProtocolAnthropic:
		root.Get("messages").ForEach(func(_, message gjson.Result) bool {
			visitDroidAnthropicToolNames(message.Get("content"), add)
			return true
		})
	case APIProtocolResponses:
		root.Get("input").ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "function_call", "custom_tool_call":
				add(item.Get("name"))
			case "additional_tools":
				tools(item.Get("tools"))
			}
			return true
		})
	case APIProtocolChatCompletions:
		root.Get("functions").ForEach(func(_, fn gjson.Result) bool { add(fn.Get("name")); return true })
		add(root.Get("function_call.name"))
		root.Get("messages").ForEach(func(_, message gjson.Result) bool {
			visitDroidChatToolNames(message, add)
			if role := message.Get("role").String(); role == "tool" || role == "function" {
				add(message.Get("name"))
			}
			return true
		})
	}
	return fields
}

func visitDroidAnthropicToolNames(content gjson.Result, add func(gjson.Result)) {
	if !content.IsArray() {
		return
	}
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "tool_use":
			add(block.Get("name"))
		case "tool_reference":
			add(block.Get("tool_name"))
		case "tool_result", "tool_search_tool_result":
			visitDroidAnthropicToolNames(block.Get("content"), add)
		}
		return true
	})
}

func visitDroidChatToolNames(message gjson.Result, add func(gjson.Result)) {
	message.Get("tool_calls").ForEach(func(_, call gjson.Result) bool {
		add(call.Get("function.name"))
		return true
	})
	add(message.Get("function_call.name"))
}

// Edit only structural name fields, never arbitrary strings inside arguments,
// schemas, text, tool outputs, signatures, IDs, metadata or cache controls.
func replaceDroidToolNameFields(body []byte, fields []gjson.Result, mapping map[string]string) []byte {
	var edits []toolNameSpan
	for _, field := range fields {
		to, ok := mapping[field.String()]
		if !ok || field.Type != gjson.String || field.Index < 0 || field.Index+len(field.Raw) > len(body) ||
			!bytes.Equal(body[field.Index:field.Index+len(field.Raw)], []byte(field.Raw)) {
			continue
		}
		encoded, _ := json.Marshal(to)
		edits = append(edits, toolNameSpan{field.Index, field.Index + len(field.Raw), encoded})
	}
	if len(edits) == 0 {
		return body
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	out, position := make([]byte, 0, len(body)), 0
	for _, edit := range edits {
		if edit.start < position {
			continue
		}
		out = append(out, body[position:edit.start]...)
		out = append(out, edit.value...)
		position = edit.end
	}
	return append(out, body[position:]...)
}

func restoreDroidToolNamesJSON(body []byte, names *droidToolNames) []byte {
	if names == nil || !gjson.ValidBytes(body) {
		return body
	}
	root := gjson.ParseBytes(body)
	var fields []gjson.Result
	add := func(name gjson.Result) { fields = append(fields, name) }
	item := func(value gjson.Result) {
		switch value.Get("type").String() {
		case "tool_use", "function_call", "custom_tool_call":
			add(value.Get("name"))
		case "tool_reference":
			add(value.Get("tool_name"))
		case "tool_search_tool_result":
			visitDroidAnthropicToolNames(value.Get("content"), add)
		}
	}
	item(root)
	item(root.Get("item"))
	item(root.Get("content_block"))
	for _, path := range []string{"content", "message.content", "output", "response.output"} {
		root.Get(path).ForEach(func(_, value gjson.Result) bool { item(value); return true })
	}
	switch root.Get("type").String() {
	case "response.function_call_arguments.done", "response.function_call_arguments.delta", "response.custom_tool_call_input.done", "response.custom_tool_call_input.delta":
		add(root.Get("name"))
	}
	root.Get("choices").ForEach(func(_, choice gjson.Result) bool {
		visitDroidChatToolNames(choice.Get("message"), add)
		visitDroidChatToolNames(choice.Get("delta"), add)
		return true
	})
	return replaceDroidToolNameFields(body, fields, names.reverse)
}

func restoreDroidToolNamesResponse(request *http.Request, response *http.Response) error {
	names, _ := request.Context().Value(droidToolNamesKey{}).(*droidToolNames)
	if names == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body = newDroidToolNameSSEReader(response.Body, names)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, droidChatBodyLimit+1))
	_ = response.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > droidChatBodyLimit {
		return errors.New("factory tool response exceeded size limit")
	}
	response.Body = io.NopCloser(bytes.NewReader(restoreDroidToolNamesJSON(body, names)))
	return nil
}

func withDroidToolNames(ctx context.Context, names *droidToolNames) context.Context {
	return context.WithValue(ctx, droidToolNamesKey{}, names)
}
