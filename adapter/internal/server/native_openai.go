package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	pb "uipath-adapter/proto"
)

type openAINativeHandler struct{}

func (openAINativeHandler) Protocol() string    { return "openai" }
func (openAINativeHandler) Endpoints() []string { return []string{"chat_completions"} }

func (openAINativeHandler) Process(adapter *UiPathAdapter, req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	var in struct {
		Model     string `json:"model"`
		MaxTokens int32  `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		Messages  []struct {
			Role         string                   `json:"role"`
			Content      interface{}              `json:"content"`
			Name         string                   `json:"name"`
			ToolCallID   string                   `json:"tool_call_id"`
			FunctionCall map[string]interface{}   `json:"function_call"`
			ToolCalls    []map[string]interface{} `json:"tool_calls"`
		} `json:"messages"`
		Tools []map[string]interface{} `json:"tools"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return sendNativeInvalidRequest(req, stream, fmt.Errorf("invalid OpenAI request: %w", err))
	}

	univ := &pb.UniversalRequest{
		RequestId: requestID(req.RequestId),
		Model:     in.Model,
		Config:    &pb.GenerationConfig{MaxTokens: in.MaxTokens, Stream: req.Stream || in.Stream},
		Context:   req.Context,
		Tools:     openAINativeTools(in.Tools),
	}
	req.Stream = univ.Config.Stream
	for _, msg := range in.Messages {
		if msg.Role == "system" {
			univ.System = appendSystem(univ.System, nativeContentText(msg.Content))
			continue
		}
		univ.Messages = append(univ.Messages, contentMessage(msg.Role, openAINativeMessageParts(msg.Role, msg.Content, msg.ToolCallID, msg.Name, msg.FunctionCall, msg.ToolCalls)))
	}
	return runNative(adapter, req, stream, univ, openAICodec{})
}

func openAINativeTools(tools []map[string]interface{}) []*pb.Tool {
	out := make([]*pb.Tool, 0, len(tools))
	for _, tool := range tools {
		if stringField(tool, "type") != "function" {
			continue
		}
		function := firstMap(tool["function"])
		if function == nil {
			continue
		}
		name := stringField(function, "name")
		if name == "" {
			continue
		}
		out = append(out, &pb.Tool{
			Name:        name,
			Description: stringField(function, "description"),
			Parameters:  nativeJSON(function["parameters"]),
		})
	}
	return out
}

func openAINativeMessageParts(role string, content interface{}, toolCallID, name string, functionCall map[string]interface{}, toolCalls []map[string]interface{}) []*pb.ContentPart {
	switch role {
	case "tool":
		return []*pb.ContentPart{{Part: &pb.ContentPart_ToolResult{ToolResult: &pb.ToolResultPart{
			ToolCallId: toolCallID,
			Result:     nativeContentText(content),
		}}}}
	case "function":
		return []*pb.ContentPart{{Part: &pb.ContentPart_ToolResult{ToolResult: &pb.ToolResultPart{
			ToolCallId: firstNonEmpty(name, toolCallID),
			Result:     nativeContentText(content),
		}}}}
	}

	parts := nativeContentParts(content)
	if len(toolCalls) > 0 {
		parts = append(parts, openAIToolCallParts(toolCalls)...)
	}
	if functionCall != nil {
		parts = append(parts, openAIFunctionCallPart(functionCall))
	}
	return parts
}

func openAIToolCallParts(toolCalls []map[string]interface{}) []*pb.ContentPart {
	parts := make([]*pb.ContentPart, 0, len(toolCalls))
	for _, raw := range toolCalls {
		function := firstMap(raw["function"])
		if function == nil {
			continue
		}
		parts = append(parts, &pb.ContentPart{Part: &pb.ContentPart_ToolCall{ToolCall: &pb.ToolCallPart{
			Id:        stringField(raw, "id"),
			Name:      stringField(function, "name"),
			Arguments: nativeArgumentString(function["arguments"]),
		}}})
	}
	return parts
}

func openAIFunctionCallPart(functionCall map[string]interface{}) *pb.ContentPart {
	return &pb.ContentPart{Part: &pb.ContentPart_ToolCall{ToolCall: &pb.ToolCallPart{
		Id:        "call_" + stringField(functionCall, "name"),
		Name:      stringField(functionCall, "name"),
		Arguments: nativeArgumentString(functionCall["arguments"]),
	}}}
}

type openAICodec struct{}

func (openAICodec) NonStream(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, model string, responses []*pb.UniversalResponse) error {
	acc := collectText(responses)
	message := map[string]interface{}{"role": "assistant", "content": acc.text.String()}
	finishReason := openAIFinishReason(acc.finishReason)
	if acc.hasToolCalls() {
		message["content"] = nil
		message["tool_calls"] = openAIResponseToolCalls(acc.toolCalls)
		finishReason = "tool_calls"
	}
	return sendNativeJSON(stream, req.RequestId, http.StatusOK, map[string]interface{}{
		"id":      "chatcmpl-" + requestID(req.RequestId),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]interface{}{
			"prompt_tokens":     acc.inputTokens,
			"completion_tokens": acc.outputTokens,
			"total_tokens":      acc.inputTokens + acc.outputTokens,
		},
	})
}

func (openAICodec) NonStreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	return sendNativeJSON(stream, req.RequestId, http.StatusBadGateway, nativeErrorBody(err.Error(), "api_error"))
}

func (openAICodec) StreamStart(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	return sendOpenAIChunk(req, stream, map[string]interface{}{"role": "assistant"}, nil)
}

func (openAICodec) StreamResponse(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, resp *pb.UniversalResponse) error {
	switch r := resp.Response.(type) {
	case *pb.UniversalResponse_Chunk:
		if r.Chunk.Text == "" {
			return nil
		}
		return sendOpenAIChunk(req, stream, map[string]interface{}{"content": r.Chunk.Text}, nil)
	case *pb.UniversalResponse_ToolCall:
		return sendOpenAIChunk(req, stream, map[string]interface{}{
			"tool_calls": openAIResponseToolCalls([]*pb.ToolCall{r.ToolCall}),
		}, nil)
	case *pb.UniversalResponse_Error:
		return openAICodec{}.StreamError(req, stream, fmt.Errorf("upstream error: %s", r.Error.Message))
	}
	return nil
}

func (openAICodec) StreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	body, _ := json.Marshal(nativeErrorBody(err.Error(), "api_error"))
	return sendNativeSSEData(stream, req.RequestId, string(body))
}

func (openAICodec) StreamDone(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, acc nativeTextAccumulator) error {
	finish := openAIFinishReason(acc.finishReason)
	if acc.hasToolCalls() {
		finish = "tool_calls"
	}
	if err := sendOpenAIChunk(req, stream, map[string]interface{}{}, &finish); err != nil {
		return err
	}
	if err := sendNativeSSEData(stream, req.RequestId, "[DONE]"); err != nil {
		return err
	}
	return sendNativeEnd(stream, req.RequestId)
}

func openAIResponseToolCalls(toolCalls []*pb.ToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(toolCalls))
	for i, call := range toolCalls {
		if call == nil {
			continue
		}
		index := int32(i)
		if call.Index != 0 {
			index = call.Index
		}
		out = append(out, map[string]interface{}{
			"index": index,
			"id":    firstNonEmpty(call.Id, fmt.Sprintf("call_%d", index)),
			"type":  "function",
			"function": map[string]interface{}{
				"name":      call.Name,
				"arguments": nativeToolCallArguments(call),
			},
		})
	}
	return out
}

func sendOpenAIChunk(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, delta map[string]interface{}, finish *string) error {
	return sendNativeSSEData(stream, req.RequestId, mustJSON(map[string]interface{}{
		"id":      "chatcmpl-" + requestID(req.RequestId),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}))
}

func openAIFinishReason(reason string) string {
	switch reason {
	case "length", "max_tokens":
		return "length"
	case "tool_calls", "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func appendSystem(current, next string) string {
	if next == "" {
		return current
	}
	if current == "" {
		return next
	}
	return current + "\n" + next
}

func mustJSON(value interface{}) string {
	body, _ := json.Marshal(value)
	return string(body)
}
