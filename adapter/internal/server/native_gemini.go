package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	pb "uipath-adapter/proto"
)

type geminiNativeHandler struct{}

func (geminiNativeHandler) Protocol() string    { return "gemini" }
func (geminiNativeHandler) Endpoints() []string { return []string{"generate_content"} }

func (geminiNativeHandler) Process(adapter *UiPathAdapter, req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	var in struct {
		Contents []struct {
			Role  string                   `json:"role"`
			Parts []map[string]interface{} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			MaxOutputTokens int32   `json:"maxOutputTokens"`
			Temperature     float64 `json:"temperature"`
			TopP            float64 `json:"topP"`
			TopK            int32   `json:"topK"`
		} `json:"generationConfig"`
		Tools []map[string]interface{} `json:"tools"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return sendNativeInvalidRequest(req, stream, fmt.Errorf("invalid Gemini request: %w", err))
	}

	univ := &pb.UniversalRequest{
		RequestId: requestID(req.RequestId),
		Model:     geminiModelFromNativePath(req.Path),
		Config: &pb.GenerationConfig{
			MaxTokens:   in.GenerationConfig.MaxOutputTokens,
			Temperature: in.GenerationConfig.Temperature,
			TopP:        in.GenerationConfig.TopP,
			TopK:        in.GenerationConfig.TopK,
			Stream:      req.Stream,
		},
		Context: req.Context,
		Tools:   geminiNativeTools(in.Tools),
	}
	req.Stream = univ.Config.Stream
	for _, content := range in.Contents {
		parts := make([]*pb.ContentPart, 0, len(content.Parts))
		for _, part := range content.Parts {
			if contentPart := geminiNativePart(part); contentPart != nil {
				parts = append(parts, contentPart)
			}
		}
		univ.Messages = append(univ.Messages, contentMessage(content.Role, parts))
	}
	return runNative(adapter, req, stream, univ, geminiCodec{})
}

func geminiNativeTools(tools []map[string]interface{}) []*pb.Tool {
	var out []*pb.Tool
	for _, tool := range tools {
		declarations, _ := firstNonNil(tool["functionDeclarations"], tool["function_declarations"]).([]interface{})
		for _, raw := range declarations {
			declaration, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			name := stringField(declaration, "name")
			if name == "" {
				continue
			}
			out = append(out, &pb.Tool{
				Name:        name,
				Description: stringField(declaration, "description"),
				Parameters:  nativeJSON(declaration["parameters"]),
			})
		}
	}
	return out
}

func geminiNativePart(part map[string]interface{}) *pb.ContentPart {
	if text := stringField(part, "text"); text != "" {
		return textPart(text)
	}
	if image := geminiImagePart(firstMap(part["fileData"], part["file_data"])); image != nil {
		return &pb.ContentPart{Part: &pb.ContentPart_Image{Image: image}}
	}
	if image := geminiInlineImagePart(firstMap(part["inlineData"], part["inline_data"])); image != nil {
		return &pb.ContentPart{Part: &pb.ContentPart_Image{Image: image}}
	}
	if call := firstMap(part["functionCall"], part["function_call"]); call != nil {
		return &pb.ContentPart{Part: &pb.ContentPart_ToolCall{ToolCall: &pb.ToolCallPart{
			Id:        "gemini_" + stringField(call, "name"),
			Name:      stringField(call, "name"),
			Arguments: nativeJSON(call["args"]),
		}}}
	}
	if response := firstMap(part["functionResponse"], part["function_response"]); response != nil {
		return &pb.ContentPart{Part: &pb.ContentPart_ToolResult{ToolResult: &pb.ToolResultPart{
			ToolCallId: "gemini_" + stringField(response, "name"),
			Result:     nativeJSON(response["response"]),
		}}}
	}
	return nil
}

func geminiImagePart(data map[string]interface{}) *pb.ImagePart {
	if data == nil {
		return nil
	}
	uri := firstNonEmpty(stringField(data, "fileUri"), stringField(data, "file_uri"))
	if uri == "" {
		return nil
	}
	return &pb.ImagePart{
		Source:   &pb.ImagePart_Url{Url: uri},
		MimeType: firstNonEmpty(stringField(data, "mimeType"), stringField(data, "mime_type")),
	}
}

func geminiInlineImagePart(data map[string]interface{}) *pb.ImagePart {
	if data == nil {
		return nil
	}
	raw := stringField(data, "data")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(decoded) == 0 {
		return nil
	}
	return &pb.ImagePart{
		Source:   &pb.ImagePart_Data{Data: decoded},
		MimeType: firstNonEmpty(stringField(data, "mimeType"), stringField(data, "mime_type")),
	}
}

func firstMap(values ...interface{}) map[string]interface{} {
	for _, value := range values {
		if mapped, ok := value.(map[string]interface{}); ok {
			return mapped
		}
	}
	return nil
}

type geminiCodec struct{}

func (geminiCodec) NonStream(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, model string, responses []*pb.UniversalResponse) error {
	acc := collectText(responses)
	if acc.hasToolCalls() {
		return sendNativeJSON(stream, req.RequestId, http.StatusOK, geminiToolCallResponse(acc.toolCalls, "STOP", acc))
	}
	return sendNativeJSON(stream, req.RequestId, http.StatusOK, geminiResponse(acc.text.String(), "STOP", acc))
}

func (geminiCodec) NonStreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	return sendNativeJSON(stream, req.RequestId, http.StatusBadGateway, map[string]interface{}{
		"error": map[string]interface{}{"code": http.StatusBadGateway, "message": err.Error(), "status": "UNAVAILABLE"},
	})
}

func (geminiCodec) StreamStart(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer) error {
	return nil
}

func (geminiCodec) StreamResponse(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, resp *pb.UniversalResponse) error {
	switch r := resp.Response.(type) {
	case *pb.UniversalResponse_Chunk:
		if r.Chunk.Text == "" {
			return nil
		}
		return sendNativeSSE(stream, req.RequestId, "", geminiResponse(r.Chunk.Text, "", nativeTextAccumulator{}))
	case *pb.UniversalResponse_ToolCall:
		return sendNativeSSE(stream, req.RequestId, "", geminiToolCallResponse([]*pb.ToolCall{r.ToolCall}, "STOP", nativeTextAccumulator{}))
	case *pb.UniversalResponse_Error:
		return geminiCodec{}.StreamError(req, stream, fmt.Errorf("upstream error: %s", r.Error.Message))
	}
	return nil
}

func (geminiCodec) StreamError(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, err error) error {
	return sendNativeSSE(stream, req.RequestId, "", map[string]interface{}{
		"error": map[string]interface{}{"code": http.StatusInternalServerError, "message": err.Error(), "status": "INTERNAL"},
	})
}

func (geminiCodec) StreamDone(req *pb.NativeRequest, stream pb.AdapterService_ProcessNativeServer, acc nativeTextAccumulator) error {
	if acc.hasToolCalls() {
		return sendNativeEnd(stream, req.RequestId)
	}
	if err := sendNativeSSE(stream, req.RequestId, "", geminiResponse(" ", geminiFinishReason(acc.finishReason), acc)); err != nil {
		return err
	}
	return sendNativeEnd(stream, req.RequestId)
}

func geminiFinishReason(reason string) string {
	switch reason {
	case "length", "max_tokens":
		return "MAX_TOKENS"
	case "safety", "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}

func geminiResponse(text, finish string, acc nativeTextAccumulator) map[string]interface{} {
	candidate := map[string]interface{}{
		"content": map[string]interface{}{
			"role":  "model",
			"parts": []map[string]interface{}{{"text": text}},
		},
		"index": 0,
	}
	if finish != "" {
		candidate["finishReason"] = finish
	}
	return map[string]interface{}{
		"candidates": []map[string]interface{}{candidate},
		"usageMetadata": map[string]interface{}{
			"promptTokenCount":     acc.inputTokens,
			"candidatesTokenCount": acc.outputTokens,
			"totalTokenCount":      acc.inputTokens + acc.outputTokens,
		},
	}
}

func geminiToolCallResponse(toolCalls []*pb.ToolCall, finish string, acc nativeTextAccumulator) map[string]interface{} {
	parts := make([]map[string]interface{}, 0, len(toolCalls))
	for _, call := range toolCalls {
		if call == nil {
			continue
		}
		parts = append(parts, map[string]interface{}{
			"functionCall": map[string]interface{}{
				"name": call.Name,
				"args": nativeToolCallJSONValue(call),
			},
		})
	}
	candidate := map[string]interface{}{
		"content": map[string]interface{}{
			"role":  "model",
			"parts": parts,
		},
		"index": 0,
	}
	if finish != "" {
		candidate["finishReason"] = finish
	}
	return map[string]interface{}{
		"candidates": []map[string]interface{}{candidate},
		"usageMetadata": map[string]interface{}{
			"promptTokenCount":     acc.inputTokens,
			"candidatesTokenCount": acc.outputTokens,
			"totalTokenCount":      acc.inputTokens + acc.outputTokens,
		},
	}
}

func geminiModelFromNativePath(path string) string {
	if idx := strings.Index(path, "/models/"); idx >= 0 {
		model := strings.TrimPrefix(path[idx:], "/models/")
		if cut := strings.Index(model, ":"); cut >= 0 {
			return model[:cut]
		}
		if model != "" {
			return model
		}
	}
	return "gemini-pro"
}
