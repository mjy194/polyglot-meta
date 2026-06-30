package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	pb "uipath-adapter/proto"
)

type anthropicNativeHandler struct{}

func (anthropicNativeHandler) Protocol() string    { return "anthropic" }
func (anthropicNativeHandler) Endpoints() []string { return []string{"messages"} }

func (anthropicNativeHandler) Process(adapter *UiPathAdapter, req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	var in struct {
		Model     string      `json:"model"`
		MaxTokens int32       `json:"max_tokens"`
		Stream    bool        `json:"stream"`
		System    interface{} `json:"system"` // string or []SystemBlock (Anthropic allows both)
		Messages  []struct {
			Role    string      `json:"role"`
			Content interface{} `json:"content"`
		} `json:"messages"`
		Tools []map[string]interface{} `json:"tools"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return sendNativeInvalidRequest(req, stream, fmt.Errorf("invalid Anthropic request: %w", err))
	}

	univ := &pb.UniversalRequest{
		RequestId: requestID(req.RequestId),
		Model:     in.Model,
		System:    nativeContentText(in.System),
		Config:    &pb.GenerationConfig{MaxTokens: in.MaxTokens, Stream: req.Stream || in.Stream},
		Context:   req.Context,
		Tools:     anthropicNativeTools(in.Tools),
	}
	req.Stream = univ.Config.Stream
	for _, msg := range in.Messages {
		univ.Messages = append(univ.Messages, contentMessage(msg.Role, nativeContentParts(msg.Content)))
	}
	return runNative(adapter, req, stream, univ, anthropicCodec{})
}

func anthropicNativeTools(tools []map[string]interface{}) []*pb.Tool {
	out := make([]*pb.Tool, 0, len(tools))
	for _, tool := range tools {
		name := stringField(tool, "name")
		if name == "" {
			continue
		}
		out = append(out, &pb.Tool{
			Name:        name,
			Description: stringField(tool, "description"),
			Parameters:  nativeJSON(tool["input_schema"]),
		})
	}
	return out
}

type anthropicCodec struct{}

func (anthropicCodec) NonStream(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, model string, responses []*pb.UniversalResponse) error {
	acc := collectText(responses)
	content := []map[string]interface{}{}
	if text := acc.text.String(); text != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": text})
	}
	for _, call := range acc.toolCalls {
		content = append(content, anthropicToolUseBlock(call))
	}
	if len(content) == 0 {
		content = append(content, map[string]interface{}{"type": "text", "text": ""})
	}
	stopReason := anthropicStopReason(acc.finishReason)
	if acc.hasToolCalls() {
		stopReason = "tool_use"
	}
	return sendNativeJSON(stream, req.RequestId, http.StatusOK, map[string]interface{}{
		"id":            "msg_" + requestID(req.RequestId),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  acc.inputTokens,
			"output_tokens": acc.outputTokens,
		},
	})
}

func (anthropicCodec) NonStreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	return sendNativeJSON(stream, req.RequestId, http.StatusBadGateway, map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": err.Error(),
		},
	})
}

func (anthropicCodec) StreamStart(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	if err := sendNativeSSE(stream, req.RequestId, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": "msg_" + requestID(req.RequestId), "type": "message", "role": "assistant",
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}
	return sendNativeSSE(stream, req.RequestId, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
}

func (anthropicCodec) StreamResponse(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, resp *pb.UniversalResponse) error {
	switch r := resp.Response.(type) {
	case *pb.UniversalResponse_Chunk:
		if r.Chunk.Text == "" {
			return nil
		}
		return sendNativeSSE(stream, req.RequestId, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": r.Chunk.Text},
		})
	case *pb.UniversalResponse_ToolCall:
		index := int(r.ToolCall.Index)
		if index == 0 {
			index = 1
		}
		if err := sendNativeSSE(stream, req.RequestId, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		}); err != nil {
			return err
		}
		if err := sendNativeSSE(stream, req.RequestId, "content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         index,
			"content_block": anthropicToolUseBlock(r.ToolCall),
		}); err != nil {
			return err
		}
		if err := sendNativeSSE(stream, req.RequestId, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": nativeToolCallArguments(r.ToolCall),
			},
		}); err != nil {
			return err
		}
		return sendNativeSSE(stream, req.RequestId, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": index,
		})
	case *pb.UniversalResponse_Error:
		return anthropicCodec{}.StreamError(req, stream, fmt.Errorf("upstream error: %s", r.Error.Message))
	}
	return nil
}

func (anthropicCodec) StreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	return sendNativeSSE(stream, req.RequestId, "error", map[string]interface{}{
		"type":  "error",
		"error": map[string]interface{}{"type": "api_error", "message": err.Error()},
	})
}

func (anthropicCodec) StreamDone(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, acc nativeTextAccumulator) error {
	if !acc.hasToolCalls() {
		if err := sendNativeSSE(stream, req.RequestId, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		}); err != nil {
			return err
		}
	}
	stopReason := anthropicStopReason(acc.finishReason)
	if acc.hasToolCalls() {
		stopReason = "tool_use"
	}
	if err := sendNativeSSE(stream, req.RequestId, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]interface{}{"input_tokens": acc.inputTokens, "output_tokens": acc.outputTokens},
	}); err != nil {
		return err
	}
	if err := sendNativeSSE(stream, req.RequestId, "message_stop", map[string]interface{}{"type": "message_stop"}); err != nil {
		return err
	}
	return sendNativeEnd(stream, req.RequestId)
}

func anthropicStopReason(reason string) string {
	switch reason {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "tool_use":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func anthropicToolUseBlock(call *pb.ToolCall) map[string]interface{} {
	return map[string]interface{}{
		"type":  "tool_use",
		"id":    firstNonEmpty(call.Id, "tool_use"),
		"name":  call.Name,
		"input": nativeToolCallJSONValue(call),
	}
}
