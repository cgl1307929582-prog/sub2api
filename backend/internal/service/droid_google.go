package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/google/uuid"
)

const droidGooglePath = "/api/llm/g/v1/generate"
const droidGoogleSignatureMarker = "__droid_gsig_"

// Carry the opaque thought signature in a portable call ID, so a tool round
// trip survives every ingress format, restarts and multiple gateway instances.
func droidGoogleCallID(id, signature string) string {
	if id == "" {
		id = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if signature != "" {
		id += droidGoogleSignatureMarker + base64.RawURLEncoding.EncodeToString([]byte(signature))
	}
	return id
}

func droidGoogleCallSignature(id string) (string, string, error) {
	before, encoded, found := strings.Cut(id, droidGoogleSignatureMarker)
	if !found {
		return id, "", nil
	}
	signature, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(signature) > 256<<10 {
		return "", "", errors.New("invalid Factory Gemini tool signature")
	}
	return before, string(signature), nil
}

func droidGoogleMedia(uri string) (map[string]any, error) {
	if strings.HasPrefix(uri, "data:") {
		meta, data, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ",")
		if !ok || !strings.HasSuffix(meta, ";base64") {
			return nil, errors.New("factory Gemini media requires a base64 data URL")
		}
		return map[string]any{"inlineData": map[string]string{"mimeType": strings.TrimSuffix(meta, ";base64"), "data": data}}, nil
	}
	// Do not fetch caller-controlled URLs on the gateway.
	if strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "gs://") {
		return map[string]any{"fileData": map[string]string{"fileUri": uri}}, nil
	}
	return nil, errors.New("unsupported Factory Gemini media URL")
}

func droidGoogleContent(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []any{map[string]any{"text": text}}, nil
	}
	var parts []apicompat.ChatContentPart
	if json.Unmarshal(raw, &parts) != nil {
		return nil, errors.New("invalid Factory Gemini message content")
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			out = append(out, map[string]any{"text": part.Text})
		case "image_url", "file":
			uri := ""
			if part.ImageURL != nil {
				uri = part.ImageURL.URL
			}
			if part.File != nil {
				uri = part.File.FileData
			}
			media, err := droidGoogleMedia(uri)
			if err != nil {
				return nil, err
			}
			out = append(out, media)
		default:
			return nil, errors.New("unsupported Factory Gemini content part")
		}
	}
	return out, nil
}

func droidChatToGoogle(body []byte) ([]byte, error) {
	var req apicompat.ChatCompletionsRequest
	if json.Unmarshal(body, &req) != nil {
		return nil, errors.New("invalid Factory Gemini request")
	}
	contents := make([]any, 0, len(req.Messages))
	var system []any
	toolNames := map[string]string{}
	for _, message := range req.Messages {
		if message.Role == "tool" || message.Role == "function" {
			name := toolNames[message.ToolCallID]
			if name == "" {
				name = message.Name
			}
			if name == "" {
				return nil, errors.New("factory Gemini tool result has no matching function call")
			}
			var output any
			decoder := json.NewDecoder(strings.NewReader(string(message.Content)))
			decoder.UseNumber()
			if decoder.Decode(&output) != nil {
				return nil, errors.New("invalid Factory Gemini tool result")
			}
			part := map[string]any{"functionResponse": map[string]any{"name": name, "response": map[string]any{"output": output}}}
			// Parallel tool results must occupy one user turn.
			if len(contents) > 0 {
				last, _ := contents[len(contents)-1].(map[string]any)
				if last["role"] == "user" {
					previousParts, _ := last["parts"].([]any)
					last["parts"] = append(previousParts, part)
					continue
				}
			}
			contents = append(contents, map[string]any{"role": "user", "parts": []any{part}})
			continue
		}
		parts, err := droidGoogleContent(message.Content)
		if err != nil {
			return nil, err
		}
		if message.Role == "system" || message.Role == "developer" {
			system = append(system, parts...)
			continue
		}
		role := message.Role
		if role == "assistant" {
			role = "model"
		}
		if role != "user" && role != "model" {
			return nil, errors.New("unsupported Factory Gemini message role")
		}
		for _, call := range message.ToolCalls {
			_, signature, err := droidGoogleCallSignature(call.ID)
			if err != nil {
				return nil, err
			}
			if !json.Valid([]byte(call.Function.Arguments)) {
				return nil, errors.New("invalid Factory Gemini function arguments")
			}
			part := map[string]any{"functionCall": map[string]any{"name": call.Function.Name, "args": json.RawMessage(call.Function.Arguments)}}
			if signature != "" {
				part["thoughtSignature"] = signature
			}
			parts = append(parts, part)
			toolNames[call.ID] = call.Function.Name
		}
		if len(parts) > 0 {
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
	}
	config := map[string]any{}
	if req.MaxCompletionTokens != nil {
		config["maxOutputTokens"] = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		config["maxOutputTokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		config["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		config["topP"] = *req.TopP
	}
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		var stops []string
		var stop string
		if json.Unmarshal(req.Stop, &stop) == nil {
			stops = []string{stop}
		} else if json.Unmarshal(req.Stop, &stops) != nil {
			return nil, errors.New("invalid Factory Gemini stop sequences")
		}
		config["stopSequences"] = stops
	}
	if req.ReasoningEffort != "" {
		level := strings.ToUpper(req.ReasoningEffort)
		switch req.ReasoningEffort {
		case "off", "none", "minimal":
			level = "LOW"
			if strings.HasPrefix(req.Model, "gemini-3-flash") || strings.HasPrefix(req.Model, "gemini-3.5-flash") {
				level = "MINIMAL"
			}
		case "xhigh", "max":
			level = "HIGH"
		case "low", "medium", "high":
		default:
			return nil, errors.New("unsupported Factory Gemini reasoning effort")
		}
		config["thinkingConfig"] = map[string]any{"thinkingLevel": level, "includeThoughts": true}
	}
	if len(req.ResponseFormat) > 0 {
		var format struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		}
		if json.Unmarshal(req.ResponseFormat, &format) != nil {
			return nil, errors.New("invalid Factory Gemini response format")
		}
		switch format.Type {
		case "json_object":
			config["responseMimeType"] = "application/json"
		case "json_schema":
			config["responseMimeType"] = "application/json"
			config["responseJsonSchema"] = format.JSONSchema.Schema
		case "", "text":
		default:
			return nil, errors.New("unsupported Factory Gemini response format")
		}
	}
	out := map[string]any{"model": req.Model, "contents": contents, "generationConfig": config}
	if len(system) > 0 {
		out["systemInstruction"] = map[string]any{"parts": system}
	}
	functions := append([]apicompat.ChatFunction(nil), req.Functions...)
	for _, tool := range req.Tools {
		if tool.Type != "function" || tool.Function == nil {
			return nil, errors.New("factory Gemini supports function tools only")
		}
		functions = append(functions, *tool.Function)
	}
	if len(functions) > 0 {
		decls := make([]any, 0, len(functions))
		for _, fn := range functions {
			decl := map[string]any{"name": fn.Name, "description": fn.Description}
			if len(fn.Parameters) > 0 {
				decl["parametersJsonSchema"] = fn.Parameters
			}
			decls = append(decls, decl)
		}
		out["tools"] = []any{map[string]any{"functionDeclarations": decls}}
		choice := req.ToolChoice
		if len(choice) == 0 {
			choice = req.FunctionCall
		}
		if len(choice) > 0 && string(choice) != "null" {
			var mode string
			var named struct {
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			fc := map[string]any{}
			if json.Unmarshal(choice, &mode) == nil {
				switch mode {
				case "auto":
					fc["mode"] = "AUTO"
				case "none":
					fc["mode"] = "NONE"
				case "required":
					fc["mode"] = "ANY"
				default:
					return nil, errors.New("invalid Factory Gemini tool choice")
				}
			} else {
				if json.Unmarshal(choice, &named) != nil {
					return nil, errors.New("invalid Factory Gemini tool choice")
				}
				name := named.Function.Name
				if name == "" {
					name = named.Name
				}
				if name == "" {
					return nil, errors.New("invalid Factory Gemini named tool")
				}
				fc["mode"], fc["allowedFunctionNames"] = "ANY", []string{name}
			}
			out["toolConfig"] = map[string]any{"functionCallingConfig": fc}
		}
	}
	// Factory always streams this endpoint and rejects a JSON "stream" field.
	return json.Marshal(out)
}

type droidGoogleFrame struct {
	Candidates []struct {
		Index   int `json:"index"`
		Content struct {
			Parts []struct {
				Text             string `json:"text"`
				Thought          bool   `json:"thought"`
				ThoughtSignature string `json:"thoughtSignature"`
				FunctionCall     *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
					ID   string          `json:"id"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Usage *struct {
		Prompt   int `json:"promptTokenCount"`
		Output   int `json:"candidatesTokenCount"`
		Thoughts int `json:"thoughtsTokenCount"`
		Cached   int `json:"cachedContentTokenCount"`
		Total    int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error json.RawMessage `json:"error"`
}

type droidGoogleStream struct {
	id, model string
	created   int64
	toolIndex int
	finished  bool
	usage     *apicompat.ChatUsage
}

func newDroidGoogleStream(model string) *droidGoogleStream {
	return &droidGoogleStream{id: "chatcmpl-" + uuid.NewString(), model: model, created: time.Now().Unix()}
}

func (s *droidGoogleStream) transform(data []byte) ([]byte, error) {
	var frame droidGoogleFrame
	if json.Unmarshal(data, &frame) != nil {
		return nil, errors.New("invalid Factory Gemini response frame")
	}
	if len(frame.Error) > 0 && string(frame.Error) != "null" {
		return nil, errors.New("factory Gemini returned a stream error")
	}
	if frame.PromptFeedback.BlockReason != "" {
		return nil, errors.New("factory Gemini blocked the prompt")
	}
	chunk := apicompat.ChatCompletionsChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model}
	for _, candidate := range frame.Candidates {
		if candidate.Index != 0 {
			continue
		}
		delta := apicompat.ChatDelta{Role: "assistant"}
		var text, thought strings.Builder
		for _, part := range candidate.Content.Parts {
			if part.Thought {
				_, _ = thought.WriteString(part.Text)
			} else {
				_, _ = text.WriteString(part.Text)
			}
			if fn := part.FunctionCall; fn != nil {
				index := s.toolIndex
				s.toolIndex++
				args := string(fn.Args)
				if args == "" {
					args = "{}"
				}
				delta.ToolCalls = append(delta.ToolCalls, apicompat.ChatToolCall{Index: &index, ID: droidGoogleCallID(fn.ID, part.ThoughtSignature), Type: "function", Function: apicompat.ChatFunctionCall{Name: fn.Name, Arguments: args}})
			}
		}
		if text.Len() > 0 {
			v := text.String()
			delta.Content = &v
		}
		if thought.Len() > 0 {
			v := thought.String()
			delta.ReasoningContent = &v
		}
		choice := apicompat.ChatChunkChoice{Index: 0, Delta: delta}
		if candidate.FinishReason != "" {
			finish := "stop"
			switch candidate.FinishReason {
			case "STOP":
				if s.toolIndex > 0 {
					finish = "tool_calls"
				}
			case "MAX_TOKENS":
				finish = "length"
			case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
				finish = "content_filter"
			default:
				return nil, fmt.Errorf("factory Gemini generation failed: %s", candidate.FinishReason)
			}
			choice.FinishReason = &finish
			s.finished = true
		}
		chunk.Choices = append(chunk.Choices, choice)
	}
	if u := frame.Usage; u != nil && (u.Prompt > 0 || u.Output > 0 || u.Thoughts > 0 || u.Total > 0) {
		s.usage = &apicompat.ChatUsage{PromptTokens: u.Prompt, CompletionTokens: u.Output + u.Thoughts, TotalTokens: u.Prompt + u.Output + u.Thoughts,
			PromptTokensDetails: &apicompat.ChatTokenDetails{CachedTokens: u.Cached}, CompletionTokensDetails: &apicompat.ChatTokenDetails{ReasoningTokens: u.Thoughts}}
		chunk.Usage = s.usage
	}
	if len(chunk.Choices) == 0 && chunk.Usage == nil {
		return nil, nil
	}
	return json.Marshal(chunk)
}
