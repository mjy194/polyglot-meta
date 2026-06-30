package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	pb "uipath-adapter/proto"
)

type nativeHandler interface {
	Protocol() string
	Endpoints() []string
	Process(adapter *UiPathAdapter, req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error
}

var nativeHandlers = []nativeHandler{
	anthropicNativeHandler{},
	openAINativeHandler{},
	responsesNativeHandler{},
	geminiNativeHandler{},
}

func nativeProtocolSupport() []*pb.NativeProtocolSupport {
	out := make([]*pb.NativeProtocolSupport, 0, len(nativeHandlers))
	for _, handler := range nativeHandlers {
		out = append(out, &pb.NativeProtocolSupport{
			Protocol:  handler.Protocol(),
			Endpoints: handler.Endpoints(),
			Streaming: true,
		})
	}
	return out
}

func nativeHandlerFor(protocol, endpoint string) (nativeHandler, bool) {
	for _, handler := range nativeHandlers {
		if handler.Protocol() != protocol {
			continue
		}
		for _, candidate := range handler.Endpoints() {
			if candidate == "*" || candidate == endpoint {
				return handler, true
			}
		}
	}
	return nil, false
}

func runNative(adapter *UiPathAdapter, req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, univ *pb.UniversalRequest, codec nativeCodec) error {
	if req.Stream {
		if err := codec.StreamStart(req, stream); err != nil {
			return err
		}
		var acc nativeTextAccumulator
		err := adapter.processUniversalRequest(univ, stream.Context(), func(resp *pb.UniversalResponse) error {
			acc.Add(resp)
			return codec.StreamResponse(req, stream, resp)
		})
		if err != nil {
			return codec.StreamError(req, stream, err)
		}
		return codec.StreamDone(req, stream, acc)
	}

	var responses []*pb.UniversalResponse
	err := adapter.processUniversalRequest(univ, stream.Context(), func(resp *pb.UniversalResponse) error {
		responses = append(responses, resp)
		return nil
	})
	if err != nil {
		return codec.NonStreamError(req, stream, err)
	}
	return codec.NonStream(req, stream, univ.Model, responses)
}

type nativeCodec interface {
	NonStream(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, model string, responses []*pb.UniversalResponse) error
	NonStreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error
	StreamStart(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error
	StreamResponse(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, resp *pb.UniversalResponse) error
	StreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error
	StreamDone(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, acc nativeTextAccumulator) error
}

type nativeTextAccumulator struct {
	text         strings.Builder
	toolCalls    []*pb.ToolCall
	inputTokens  uint32
	outputTokens uint32
	finishReason string
}

func (a *nativeTextAccumulator) Add(resp *pb.UniversalResponse) {
	switch r := resp.Response.(type) {
	case *pb.UniversalResponse_Chunk:
		a.text.WriteString(r.Chunk.Text)
	case *pb.UniversalResponse_ToolCall:
		a.toolCalls = append(a.toolCalls, r.ToolCall)
		if a.finishReason == "" {
			a.finishReason = "tool_calls"
		}
	case *pb.UniversalResponse_Completion:
		a.inputTokens = r.Completion.InputTokens
		a.outputTokens = r.Completion.OutputTokens
		a.finishReason = r.Completion.FinishReason
	}
}

func (a nativeTextAccumulator) hasToolCalls() bool {
	return len(a.toolCalls) > 0
}

func collectText(responses []*pb.UniversalResponse) nativeTextAccumulator {
	var acc nativeTextAccumulator
	for _, resp := range responses {
		acc.Add(resp)
	}
	return acc
}

func sendNativeJSON(stream pb.AdapterService_ProcessNativeServer, reqID string, status int, value interface{}) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return stream.Send(&pb.NativeResponse{
		RequestId:  reqID,
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
		EndStream:  true,
	})
}

func sendNativeSSE(stream pb.AdapterService_ProcessNativeServer, reqID string, event string, value interface{}) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var payload string
	if event != "" {
		payload = "event: " + event + "\n"
	}
	payload += "data: " + string(body) + "\n\n"
	return stream.Send(&pb.NativeResponse{
		RequestId:  reqID,
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache"},
		Body:       []byte(payload),
	})
}

func sendNativeSSEData(stream pb.AdapterService_ProcessNativeServer, reqID string, data string) error {
	return stream.Send(&pb.NativeResponse{
		RequestId:  reqID,
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache"},
		Body:       []byte("data: " + data + "\n\n"),
	})
}

func sendNativeEnd(stream pb.AdapterService_ProcessNativeServer, reqID string) error {
	return stream.Send(&pb.NativeResponse{RequestId: reqID, StatusCode: http.StatusOK, EndStream: true})
}

func nativeErrorBody(message, typ string) map[string]interface{} {
	return map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    typ,
		},
	}
}

func sendNativeInvalidRequest(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	return sendNativeJSON(stream, req.RequestId, http.StatusBadRequest, nativeErrorBody(err.Error(), "invalid_request_error"))
}

func textMessage(role, text string) *pb.Message {
	return contentMessage(role, []*pb.ContentPart{textPart(text)})
}

func contentMessage(role string, parts []*pb.ContentPart) *pb.Message {
	if len(parts) == 0 {
		parts = []*pb.ContentPart{textPart("")}
	}
	return &pb.Message{
		Role:    roleFromString(role),
		Content: parts,
	}
}

func textPart(text string) *pb.ContentPart {
	return &pb.ContentPart{Part: &pb.ContentPart_Text{Text: &pb.TextPart{Text: text}}}
}

func nativeContentParts(content interface{}) []*pb.ContentPart {
	switch value := content.(type) {
	case string:
		return []*pb.ContentPart{textPart(value)}
	case []interface{}:
		parts := make([]*pb.ContentPart, 0, len(value))
		for _, raw := range value {
			partMap, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if part := nativeContentPartFromMap(partMap); part != nil {
				parts = append(parts, part)
			}
		}
		if len(parts) > 0 {
			return parts
		}
	}
	return []*pb.ContentPart{textPart(nativeContentText(content))}
}

func nativeContentPartFromMap(part map[string]interface{}) *pb.ContentPart {
	switch partType, _ := part["type"].(string); partType {
	case "text", "input_text":
		return textPart(stringField(part, "text"))
	case "image":
		if image := imageFromAnthropicSource(part["source"]); image != nil {
			return &pb.ContentPart{Part: &pb.ContentPart_Image{Image: image}}
		}
	case "image_url", "input_image":
		if image := imageFromURLValue(part["image_url"]); image != nil {
			return &pb.ContentPart{Part: &pb.ContentPart_Image{Image: image}}
		}
	case "tool_result":
		return &pb.ContentPart{Part: &pb.ContentPart_ToolResult{ToolResult: &pb.ToolResultPart{
			ToolCallId: stringField(part, "tool_use_id"),
			Result:     nativeContentText(part["content"]),
			IsError:    boolField(part, "is_error"),
		}}}
	case "tool_use", "function_call":
		return &pb.ContentPart{Part: &pb.ContentPart_ToolCall{ToolCall: &pb.ToolCallPart{
			Id:        firstNonEmpty(stringField(part, "id"), stringField(part, "call_id")),
			Name:      stringField(part, "name"),
			Arguments: nativeJSON(part["input"]),
		}}}
	default:
		if text := stringField(part, "text"); text != "" {
			return textPart(text)
		}
	}
	return nil
}

func imageFromAnthropicSource(raw interface{}) *pb.ImagePart {
	source, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	image := &pb.ImagePart{MimeType: stringField(source, "media_type")}
	switch stringField(source, "type") {
	case "url":
		url := stringField(source, "url")
		if url == "" {
			return nil
		}
		image.Source = &pb.ImagePart_Url{Url: url}
	case "base64":
		data, err := base64.StdEncoding.DecodeString(stringField(source, "data"))
		if err != nil || len(data) == 0 {
			return nil
		}
		image.Source = &pb.ImagePart_Data{Data: data}
	default:
		return nil
	}
	return image
}

func imageFromURLValue(raw interface{}) *pb.ImagePart {
	image := &pb.ImagePart{}
	switch value := raw.(type) {
	case string:
		if value == "" {
			return nil
		}
		image.Source = &pb.ImagePart_Url{Url: value}
	case map[string]interface{}:
		url := stringField(value, "url")
		if url == "" {
			return nil
		}
		image.Source = &pb.ImagePart_Url{Url: url}
		image.Detail = stringField(value, "detail")
	default:
		return nil
	}
	return image
}

func nativeContentText(content interface{}) string {
	switch value := content.(type) {
	case string:
		return value
	case []interface{}:
		var out strings.Builder
		for _, raw := range value {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				out.WriteString(text)
			}
		}
		if out.Len() > 0 {
			return out.String()
		}
		return nativeJSON(value)
	case map[string]interface{}:
		if text := stringField(value, "text"); text != "" {
			return text
		}
		return nativeJSON(value)
	default:
		return nativeJSON(value)
	}
}

func nativeJSON(value interface{}) string {
	if value == nil {
		return ""
	}
	b, _ := json.Marshal(value)
	return string(b)
}

func nativeArgumentString(value interface{}) string {
	if raw, ok := value.(string); ok {
		return raw
	}
	return nativeJSON(value)
}

func nativeToolCallArguments(call *pb.ToolCall) string {
	if call == nil || strings.TrimSpace(call.Arguments) == "" {
		return "{}"
	}
	return call.Arguments
}

func nativeToolCallJSONValue(call *pb.ToolCall) interface{} {
	args := nativeToolCallArguments(call)
	var out interface{}
	if err := json.Unmarshal([]byte(args), &out); err == nil && out != nil {
		return out
	}
	return map[string]interface{}{"value": args}
}

func stringField(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return value
}

func boolField(values map[string]interface{}, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonNil(values ...interface{}) interface{} {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func roleFromString(role string) pb.Message_Role {
	switch role {
	case "assistant", "model":
		return pb.Message_ASSISTANT
	case "system":
		return pb.Message_SYSTEM
	case "tool", "function":
		return pb.Message_TOOL
	default:
		return pb.Message_USER
	}
}

func requestID(raw string) string {
	if raw != "" {
		return raw
	}
	return fmt.Sprintf("native_%d", time.Now().UnixNano())
}
