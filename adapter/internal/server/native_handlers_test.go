package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	pb "uipath-adapter/proto"
)

type captureNativeStream struct {
	ctx       context.Context
	responses []*pb.NativeResponse
}

func (s *captureNativeStream) Send(resp *pb.NativeResponse) error {
	s.responses = append(s.responses, resp)
	return nil
}

func (s *captureNativeStream) SetHeader(metadata.MD) error  { return nil }
func (s *captureNativeStream) SendHeader(metadata.MD) error { return nil }
func (s *captureNativeStream) SetTrailer(metadata.MD)       {}
func (s *captureNativeStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *captureNativeStream) SendMsg(interface{}) error { return nil }
func (s *captureNativeStream) RecvMsg(interface{}) error { return nil }

func TestNativeProtocolSupportAdvertisesProtocolHandlers(t *testing.T) {
	support := nativeProtocolSupport()
	got := map[string]bool{}
	for _, item := range support {
		got[item.Protocol] = len(item.Endpoints) > 0 && item.Streaming
	}
	for _, protocol := range []string{"anthropic", "openai", "responses", "gemini"} {
		if !got[protocol] {
			t.Fatalf("missing native support for %s: %+v", protocol, support)
		}
	}
}

func TestProcessNativeOpenAIMockMode(t *testing.T) {
	adapter := &UiPathAdapter{}
	stream := &captureNativeStream{}
	req := &pb.NativeRequest{
		RequestId: "req_native_openai",
		Protocol:  "openai",
		Endpoint:  "chat_completions",
		Body:      []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
	}

	if err := adapter.ProcessNative(req, stream); err != nil {
		t.Fatalf("ProcessNative: %v", err)
	}
	if len(stream.responses) != 1 {
		t.Fatalf("responses=%d, want 1", len(stream.responses))
	}
	body := string(stream.responses[0].Body)
	if !strings.Contains(body, `"object":"chat.completion"`) || !strings.Contains(body, "Mock response from UiPath Adapter") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestProcessNativeNonStreamProtocolMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		endpoint string
		path     string
		body     string
		markers  []string
	}{
		{
			name:     "anthropic",
			protocol: "anthropic",
			endpoint: "messages",
			path:     "/v1/messages",
			body:     `{"model":"claude","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
			markers:  []string{`"type":"message"`, `"model":"claude"`, `"stop_reason":"end_turn"`, "Mock response from UiPath Adapter"},
		},
		{
			name:     "openai",
			protocol: "openai",
			endpoint: "chat_completions",
			path:     "/v1/chat/completions",
			body:     `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			markers:  []string{`"object":"chat.completion"`, `"model":"gpt-4"`, `"finish_reason":"stop"`, "Mock response from UiPath Adapter"},
		},
		{
			name:     "responses",
			protocol: "responses",
			endpoint: "responses",
			path:     "/v1/responses",
			body:     `{"model":"gpt-4o","input":"hi"}`,
			markers:  []string{`"object":"response"`, `"model":"gpt-4o"`, `"status":"completed"`, "Mock response from UiPath Adapter"},
		},
		{
			name:     "gemini",
			protocol: "gemini",
			endpoint: "generate_content",
			path:     "/v1beta/models/gemini-pro:generateContent",
			body:     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			markers:  []string{`"candidates"`, `"finishReason":"STOP"`, "Mock response from UiPath Adapter"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &UiPathAdapter{}
			stream := &captureNativeStream{}
			req := &pb.NativeRequest{
				RequestId: "req_native_" + tc.name,
				Protocol:  tc.protocol,
				Endpoint:  tc.endpoint,
				Path:      tc.path,
				Body:      []byte(tc.body),
			}

			if err := adapter.ProcessNative(req, stream); err != nil {
				t.Fatalf("ProcessNative: %v", err)
			}
			if len(stream.responses) != 1 {
				t.Fatalf("responses=%d, want 1", len(stream.responses))
			}
			resp := stream.responses[0]
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, string(resp.Body))
			}
			body := string(resp.Body)
			for _, marker := range tc.markers {
				if !strings.Contains(body, marker) {
					t.Fatalf("missing marker %q\n--- body ---\n%s", marker, body)
				}
			}
		})
	}
}

func TestProcessNativeStreamProtocolMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		endpoint string
		path     string
		body     string
		markers  []string
	}{
		{
			name:     "anthropic",
			protocol: "anthropic",
			endpoint: "messages",
			path:     "/v1/messages",
			body:     `{"model":"claude","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			markers:  []string{"event: message_start", "event: content_block_delta", "event: message_stop", `"input_tokens":10`, `"output_tokens":20`, "Mock response from UiPath Adapter"},
		},
		{
			name:     "openai",
			protocol: "openai",
			endpoint: "chat_completions",
			path:     "/v1/chat/completions",
			body:     `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			markers:  []string{"data: ", `"role":"assistant"`, `"content":"Mock response from UiPath Adapter"`, "data: [DONE]"},
		},
		{
			name:     "responses",
			protocol: "responses",
			endpoint: "responses",
			path:     "/v1/responses",
			body:     `{"model":"gpt-4o","input":"hi","stream":true}`,
			markers:  []string{"event: response.created", "event: response.output_text.delta", "event: response.completed", `"total_tokens":30`, "Mock response from UiPath Adapter"},
		},
		{
			name:     "gemini",
			protocol: "gemini",
			endpoint: "generate_content",
			path:     "/v1beta/models/gemini-pro:generateContent",
			body:     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			markers:  []string{"data: ", `"text":"Mock response from UiPath Adapter"`, `"finishReason":"STOP"`, `"totalTokenCount":30`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &UiPathAdapter{}
			stream := &captureNativeStream{}
			req := &pb.NativeRequest{
				RequestId: "req_native_stream_" + tc.name,
				Protocol:  tc.protocol,
				Endpoint:  tc.endpoint,
				Path:      tc.path,
				Body:      []byte(tc.body),
				Stream:    true,
			}

			if err := adapter.ProcessNative(req, stream); err != nil {
				t.Fatalf("ProcessNative: %v", err)
			}
			if len(stream.responses) < 2 {
				t.Fatalf("responses=%d, want stream chunks", len(stream.responses))
			}
			out := joinNativeBodies(stream.responses)
			for _, marker := range tc.markers {
				if !strings.Contains(out, marker) {
					t.Fatalf("missing marker %q\n--- stream ---\n%s", marker, out)
				}
			}
			if !stream.responses[len(stream.responses)-1].EndStream {
				t.Fatalf("last response should end stream")
			}
		})
	}
}

func TestProcessNativeInvalidAndUnsupportedMatrix(t *testing.T) {
	adapter := &UiPathAdapter{}

	for _, tc := range []struct {
		name     string
		protocol string
		endpoint string
		body     string
		status   int32
		markers  []string
	}{
		{
			name:     "invalid openai json",
			protocol: "openai",
			endpoint: "chat_completions",
			body:     `{`,
			status:   http.StatusBadRequest,
			markers:  []string{`"type":"invalid_request_error"`, "invalid OpenAI request"},
		},
		{
			name:     "unsupported endpoint",
			protocol: "openai",
			endpoint: "responses",
			body:     `{}`,
			status:   http.StatusBadRequest,
			markers:  []string{`"type":"invalid_request_error"`, "unsupported native protocol endpoint"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &captureNativeStream{}
			req := &pb.NativeRequest{
				RequestId: "req_" + strings.ReplaceAll(tc.name, " ", "_"),
				Protocol:  tc.protocol,
				Endpoint:  tc.endpoint,
				Body:      []byte(tc.body),
			}

			if err := adapter.ProcessNative(req, stream); err != nil {
				t.Fatalf("ProcessNative: %v", err)
			}
			if len(stream.responses) != 1 {
				t.Fatalf("responses=%d, want 1", len(stream.responses))
			}
			resp := stream.responses[0]
			if resp.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.status, string(resp.Body))
			}
			body := string(resp.Body)
			for _, marker := range tc.markers {
				if !strings.Contains(body, marker) {
					t.Fatalf("missing marker %q\n--- body ---\n%s", marker, body)
				}
			}
		})
	}
}

func TestNativeContentPartsPreserveImagesAndToolResults(t *testing.T) {
	parts := nativeContentParts([]interface{}{
		map[string]interface{}{"type": "text", "text": "look"},
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.test/img.png", "detail": "high"}},
		map[string]interface{}{"type": "tool_result", "tool_use_id": "call_1", "content": map[string]interface{}{"ok": true}, "is_error": true},
	})

	if len(parts) != 3 {
		t.Fatalf("parts=%d want=3", len(parts))
	}
	if text := parts[0].GetText(); text == nil || text.Text != "look" {
		t.Fatalf("text part not preserved: %+v", parts[0])
	}
	if image := parts[1].GetImage(); image == nil || image.GetUrl() != "https://example.test/img.png" || image.Detail != "high" {
		t.Fatalf("image part not preserved: %+v", parts[1])
	}
	if result := parts[2].GetToolResult(); result == nil || result.ToolCallId != "call_1" || result.Result != `{"ok":true}` || !result.IsError {
		t.Fatalf("tool result not preserved: %+v", parts[2])
	}
}

func TestGeminiNativePartsPreserveFileDataAndFunctionResponse(t *testing.T) {
	filePart := geminiNativePart(map[string]interface{}{
		"fileData": map[string]interface{}{"fileUri": "gs://bucket/img.png", "mimeType": "image/png"},
	})
	if image := filePart.GetImage(); image == nil || image.GetUrl() != "gs://bucket/img.png" || image.MimeType != "image/png" {
		t.Fatalf("gemini fileData not preserved: %+v", filePart)
	}

	resultPart := geminiNativePart(map[string]interface{}{
		"functionResponse": map[string]interface{}{"name": "lookup", "response": map[string]interface{}{"ok": true}},
	})
	if result := resultPart.GetToolResult(); result == nil || result.ToolCallId != "gemini_lookup" || result.Result != `{"ok":true}` {
		t.Fatalf("gemini functionResponse not preserved: %+v", resultPart)
	}
}

func TestOpenAINativeMessagePartsPreserveToolCallsAndResults(t *testing.T) {
	toolParts := openAINativeMessageParts("tool", "42", "call_1", "", nil, nil)
	if result := toolParts[0].GetToolResult(); result == nil || result.ToolCallId != "call_1" || result.Result != "42" {
		t.Fatalf("tool result not preserved: %+v", toolParts)
	}

	assistantParts := openAINativeMessageParts("assistant", "", "", "", nil, []map[string]interface{}{{
		"id": "call_1",
		"function": map[string]interface{}{
			"name":      "lookup",
			"arguments": `{"city":"SF"}`,
		},
	}})
	if call := assistantParts[1].GetToolCall(); call == nil || call.Id != "call_1" || call.Name != "lookup" || call.Arguments != `{"city":"SF"}` {
		t.Fatalf("tool call not preserved: %+v", assistantParts)
	}
}

func TestNativeToolSchemasMapToUniversalTools(t *testing.T) {
	anthropicTools := anthropicNativeTools([]map[string]interface{}{{
		"name": "ReadFile", "description": "read", "input_schema": map[string]interface{}{"type": "object"},
	}})
	if len(anthropicTools) != 1 || anthropicTools[0].Name != "ReadFile" || !strings.Contains(anthropicTools[0].Parameters, `"type":"object"`) {
		t.Fatalf("anthropic tools not mapped: %+v", anthropicTools)
	}

	openAITools := openAINativeTools([]map[string]interface{}{{
		"type": "function",
		"function": map[string]interface{}{
			"name": "Lookup", "description": "lookup", "parameters": map[string]interface{}{"type": "object"},
		},
	}})
	if len(openAITools) != 1 || openAITools[0].Name != "Lookup" || openAITools[0].Description != "lookup" {
		t.Fatalf("openai tools not mapped: %+v", openAITools)
	}

	responsesTools := responsesNativeTools([]map[string]interface{}{{
		"type": "function", "name": "Search", "description": "search", "parameters": map[string]interface{}{"type": "object"},
	}})
	if len(responsesTools) != 1 || responsesTools[0].Name != "Search" {
		t.Fatalf("responses tools not mapped: %+v", responsesTools)
	}

	geminiTools := geminiNativeTools([]map[string]interface{}{{
		"functionDeclarations": []interface{}{
			map[string]interface{}{"name": "Plan", "description": "plan", "parameters": map[string]interface{}{"type": "object"}},
		},
	}})
	if len(geminiTools) != 1 || geminiTools[0].Name != "Plan" {
		t.Fatalf("gemini tools not mapped: %+v", geminiTools)
	}
}

func TestNativeCodecsEncodeToolCallsNonStream(t *testing.T) {
	responses := []*pb.UniversalResponse{
		toolCallResponse("req_tool", "call_1", "ReadFile", `{"path":"a.txt"}`),
		completionResponse("req_tool", "tool_calls"),
	}
	for _, tc := range []struct {
		name    string
		codec   nativeCodec
		model   string
		markers []string
	}{
		{
			name:    "anthropic",
			codec:   anthropicCodec{},
			model:   "claude",
			markers: []string{`"type":"tool_use"`, `"id":"call_1"`, `"name":"ReadFile"`, `"path":"a.txt"`, `"stop_reason":"tool_use"`},
		},
		{
			name:    "openai",
			codec:   openAICodec{},
			model:   "gpt-4",
			markers: []string{`"tool_calls"`, `"id":"call_1"`, `"name":"ReadFile"`, `"arguments":"{\"path\":\"a.txt\"}"`, `"finish_reason":"tool_calls"`},
		},
		{
			name:    "responses",
			codec:   responsesCodec{},
			model:   "gpt-4o",
			markers: []string{`"type":"function_call"`, `"call_id":"call_1"`, `"name":"ReadFile"`, `"arguments":"{\"path\":\"a.txt\"}"`},
		},
		{
			name:    "gemini",
			codec:   geminiCodec{},
			model:   "gemini-pro",
			markers: []string{`"functionCall"`, `"name":"ReadFile"`, `"path":"a.txt"`, `"finishReason":"STOP"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &captureNativeStream{}
			req := &pb.NativeRequest{RequestId: "req_tool_" + tc.name}
			if err := tc.codec.NonStream(req, stream, tc.model, responses); err != nil {
				t.Fatalf("NonStream: %v", err)
			}
			if len(stream.responses) != 1 {
				t.Fatalf("responses=%d want 1", len(stream.responses))
			}
			body := string(stream.responses[0].Body)
			for _, marker := range tc.markers {
				if !strings.Contains(body, marker) {
					t.Fatalf("missing marker %q\n--- body ---\n%s", marker, body)
				}
			}
		})
	}
}

func TestNativeCodecsEncodeToolCallsStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		codec   nativeCodec
		markers []string
	}{
		{
			name:    "anthropic",
			codec:   anthropicCodec{},
			markers: []string{"event: content_block_start", `"type":"tool_use"`, `"partial_json":"{\"path\":\"a.txt\"}"`, `"stop_reason":"tool_use"`},
		},
		{
			name:    "openai",
			codec:   openAICodec{},
			markers: []string{`"tool_calls"`, `"id":"call_1"`, `"name":"ReadFile"`, `"finish_reason":"tool_calls"`},
		},
		{
			name:    "responses",
			codec:   responsesCodec{},
			markers: []string{"event: response.output_item.added", `"type":"function_call"`, `"call_id":"call_1"`, `"arguments":"{\"path\":\"a.txt\"}"`},
		},
		{
			name:    "gemini",
			codec:   geminiCodec{},
			markers: []string{`"functionCall"`, `"name":"ReadFile"`, `"path":"a.txt"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &captureNativeStream{}
			req := &pb.NativeRequest{RequestId: "req_tool_stream_" + tc.name}
			resp := toolCallResponse(req.RequestId, "call_1", "ReadFile", `{"path":"a.txt"}`)
			var acc nativeTextAccumulator
			if err := tc.codec.StreamStart(req, stream); err != nil {
				t.Fatalf("StreamStart: %v", err)
			}
			acc.Add(resp)
			if err := tc.codec.StreamResponse(req, stream, resp); err != nil {
				t.Fatalf("StreamResponse: %v", err)
			}
			acc.Add(completionResponse(req.RequestId, "tool_calls"))
			if err := tc.codec.StreamDone(req, stream, acc); err != nil {
				t.Fatalf("StreamDone: %v", err)
			}
			out := joinNativeBodies(stream.responses)
			for _, marker := range tc.markers {
				if !strings.Contains(out, marker) {
					t.Fatalf("missing marker %q\n--- stream ---\n%s", marker, out)
				}
			}
			if !stream.responses[len(stream.responses)-1].EndStream {
				t.Fatalf("last response should end stream")
			}
		})
	}
}

func TestProcessNativeAnthropicStreamMockMode(t *testing.T) {
	adapter := &UiPathAdapter{}
	stream := &captureNativeStream{}
	req := &pb.NativeRequest{
		RequestId: "req_native_anthropic",
		Protocol:  "anthropic",
		Endpoint:  "messages",
		Body:      []byte(`{"model":"claude","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}

	if err := adapter.ProcessNative(req, stream); err != nil {
		t.Fatalf("ProcessNative: %v", err)
	}
	if len(stream.responses) < 3 {
		t.Fatalf("responses=%d, want stream chunks", len(stream.responses))
	}
	var joined strings.Builder
	for _, resp := range stream.responses {
		joined.Write(resp.Body)
	}
	out := joined.String()
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: content_block_delta") {
		t.Fatalf("unexpected stream output:\n%s", out)
	}
}

func toolCallResponse(requestID, id, name, arguments string) *pb.UniversalResponse {
	return &pb.UniversalResponse{
		RequestId: requestID,
		Response: &pb.UniversalResponse_ToolCall{ToolCall: &pb.ToolCall{
			Id:        id,
			Name:      name,
			Arguments: arguments,
		}},
	}
}

func completionResponse(requestID, finishReason string) *pb.UniversalResponse {
	return &pb.UniversalResponse{
		RequestId: requestID,
		Response: &pb.UniversalResponse_Completion{Completion: &pb.CompletionInfo{
			FinishReason: finishReason,
			InputTokens:  10,
			OutputTokens: 20,
		}},
	}
}

func joinNativeBodies(responses []*pb.NativeResponse) string {
	var joined strings.Builder
	for _, resp := range responses {
		joined.Write(resp.Body)
	}
	return joined.String()
}
