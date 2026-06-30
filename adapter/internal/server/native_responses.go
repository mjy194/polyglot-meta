package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	pb "uipath-adapter/proto"
)

type responsesNativeHandler struct{}

func (responsesNativeHandler) Protocol() string    { return "responses" }
func (responsesNativeHandler) Endpoints() []string { return []string{"responses"} }

func (responsesNativeHandler) Process(adapter *UiPathAdapter, req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	var in struct {
		Model        string                   `json:"model"`
		Instructions string                   `json:"instructions"`
		Input        interface{}              `json:"input"`
		Stream       bool                     `json:"stream"`
		MaxTokens    int32                    `json:"max_output_tokens"`
		Tools        []map[string]interface{} `json:"tools"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return sendNativeInvalidRequest(req, stream, fmt.Errorf("invalid Responses request: %w", err))
	}

	univ := &pb.UniversalRequest{
		RequestId: requestID(req.RequestId),
		Model:     in.Model,
		System:    in.Instructions,
		Config:    &pb.GenerationConfig{MaxTokens: in.MaxTokens, Stream: req.Stream || in.Stream},
		Context:   req.Context,
		Messages:  responsesInputMessages(in.Input),
		Tools:     responsesNativeTools(in.Tools),
	}
	req.Stream = univ.Config.Stream
	return runNative(adapter, req, stream, univ, responsesCodec{})
}

func responsesNativeTools(tools []map[string]interface{}) []*pb.Tool {
	out := make([]*pb.Tool, 0, len(tools))
	for _, tool := range tools {
		if stringField(tool, "type") != "function" {
			continue
		}
		name := stringField(tool, "name")
		if name == "" {
			continue
		}
		out = append(out, &pb.Tool{
			Name:        name,
			Description: stringField(tool, "description"),
			Parameters:  nativeJSON(tool["parameters"]),
		})
	}
	return out
}

type responsesCodec struct{}

func (responsesCodec) NonStream(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, model string, responses []*pb.UniversalResponse) error {
	acc := collectText(responses)
	return sendNativeJSON(stream, req.RequestId, http.StatusOK, responseObject(req.RequestId, model, "completed", acc.text.String(), acc))
}

func (responsesCodec) NonStreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	return sendNativeJSON(stream, req.RequestId, http.StatusBadGateway, nativeErrorBody(err.Error(), "server_error"))
}

func (responsesCodec) StreamStart(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	return sendNativeSSE(stream, req.RequestId, "response.created", map[string]interface{}{
		"type":     "response.created",
		"response": responseObject(req.RequestId, "", "in_progress", "", nativeTextAccumulator{}),
	})
}

func (responsesCodec) StreamResponse(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, resp *pb.UniversalResponse) error {
	switch r := resp.Response.(type) {
	case *pb.UniversalResponse_Chunk:
		if r.Chunk.Text == "" {
			return nil
		}
		return sendNativeSSE(stream, req.RequestId, "response.output_text.delta", map[string]interface{}{
			"type":  "response.output_text.delta",
			"delta": r.Chunk.Text,
		})
	case *pb.UniversalResponse_ToolCall:
		item := responsesFunctionCallItem(r.ToolCall)
		if err := sendNativeSSE(stream, req.RequestId, "response.output_item.added", map[string]interface{}{
			"type":         "response.output_item.added",
			"output_index": r.ToolCall.Index,
			"item":         item,
		}); err != nil {
			return err
		}
		return sendNativeSSE(stream, req.RequestId, "response.output_item.done", map[string]interface{}{
			"type":         "response.output_item.done",
			"output_index": r.ToolCall.Index,
			"item":         item,
		})
	case *pb.UniversalResponse_Error:
		return responsesCodec{}.StreamError(req, stream, fmt.Errorf("upstream error: %s", r.Error.Message))
	}
	return nil
}

func (responsesCodec) StreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	return sendNativeSSE(stream, req.RequestId, "error", map[string]interface{}{"code": "server_error", "message": err.Error()})
}

func (responsesCodec) StreamDone(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, acc nativeTextAccumulator) error {
	if err := sendNativeSSE(stream, req.RequestId, "response.completed", map[string]interface{}{
		"type":     "response.completed",
		"response": responseObject(req.RequestId, "", "completed", acc.text.String(), acc),
	}); err != nil {
		return err
	}
	return sendNativeEnd(stream, req.RequestId)
}

func responseObject(id, model, status, text string, acc nativeTextAccumulator) map[string]interface{} {
	itemID := "msg_" + requestID(id)
	output := []map[string]interface{}{}
	if text != "" || !acc.hasToolCalls() {
		output = append(output, map[string]interface{}{
			"type":   "message",
			"id":     itemID,
			"role":   "assistant",
			"status": status,
			"content": []map[string]interface{}{{
				"type":        "output_text",
				"text":        text,
				"annotations": []interface{}{},
			}},
		})
	}
	for _, call := range acc.toolCalls {
		output = append(output, responsesFunctionCallItem(call))
	}
	return map[string]interface{}{
		"id":         "resp_" + requestID(id),
		"object":     "response",
		"created_at": float64(time.Now().Unix()),
		"model":      model,
		"status":     status,
		"output":     output,
		"usage": map[string]interface{}{
			"input_tokens":  acc.inputTokens,
			"output_tokens": acc.outputTokens,
			"total_tokens":  acc.inputTokens + acc.outputTokens,
		},
	}
}

func responsesFunctionCallItem(call *pb.ToolCall) map[string]interface{} {
	return map[string]interface{}{
		"id":        firstNonEmpty(call.Id, "call_0"),
		"type":      "function_call",
		"call_id":   firstNonEmpty(call.Id, "call_0"),
		"name":      call.Name,
		"arguments": nativeToolCallArguments(call),
		"status":    "completed",
	}
}

func responsesInputMessages(input interface{}) []*pb.Message {
	switch value := input.(type) {
	case string:
		return []*pb.Message{textMessage("user", value)}
	case []interface{}:
		var messages []*pb.Message
		for _, raw := range value {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := item["role"].(string)
			messages = append(messages, contentMessage(role, nativeContentParts(item["content"])))
		}
		if len(messages) > 0 {
			return messages
		}
	}
	return []*pb.Message{textMessage("user", nativeContentText(input))}
}
