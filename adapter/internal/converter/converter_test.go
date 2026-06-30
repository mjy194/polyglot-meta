package converter

import (
	"encoding/json"
	"strings"
	"testing"

	"uipath-adapter/internal/client"
	pb "uipath-adapter/proto"
)

func sseEvent(event string, data string) *client.UiPathStreamEvent {
	return &client.UiPathStreamEvent{Event: event, Data: json.RawMessage(data)}
}

func TestUiPathToUniversalMessageDelta(t *testing.T) {
	resp, err := UiPathToUniversal("req1", sseEvent("message_delta", `{"delta":{"content":"Hello","type":"Response"},"finish_reason":null}`))
	if err != nil || resp == nil {
		t.Fatalf("expected chunk, got resp=%v err=%v", resp, err)
	}
	c := resp.GetChunk()
	if c == nil || c.Text != "Hello" {
		t.Fatalf("chunk text wrong: %v", resp)
	}
}

func TestUiPathToUniversalEmptyDeltaIsNil(t *testing.T) {
	// 空 content 的 message_delta 不应产生 chunk（否则下游会收到空文本块）
	resp, _ := UiPathToUniversal("req1", sseEvent("message_delta", `{"delta":{"content":"","type":"Response"}}`))
	if resp != nil {
		t.Fatalf("empty content should yield nil, got %v", resp)
	}
}

func TestUiPathToUniversalUsage(t *testing.T) {
	resp, _ := UiPathToUniversal("req1", sseEvent("usage", `{"usage_by_model":{},"turn_totals":{"prompt_tokens":11,"completion_tokens":22}}`))
	if resp == nil {
		t.Fatalf("expected completion")
	}
	comp := resp.GetCompletion()
	if comp == nil || comp.InputTokens != 11 || comp.OutputTokens != 22 {
		t.Fatalf("usage wrong: %v", resp)
	}
	if comp.FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop", comp.FinishReason)
	}
}

func TestUiPathToUniversalError(t *testing.T) {
	// 上游 error 事件 → ErrorResponse(code 500)
	resp, _ := UiPathToUniversal("req1", sseEvent("error", `upstream blew up`))
	if resp == nil {
		t.Fatalf("expected error resp")
	}
	e := resp.GetError()
	if e == nil || e.Code != 500 || e.Message != "upstream blew up" {
		t.Fatalf("error wrong: %v", resp)
	}
}

func TestUiPathToUniversalScannerErrorSurfaces(t *testing.T) {
	// client/uipath.go 在 scanner.Err() 时会发 event=error, data="stream read error: ..."
	// 必须能被 converter 转成 ErrorResponse 并透传给客户端，而不是静默中断。
	resp, _ := UiPathToUniversal("req1", sseEvent("error", `stream read error: unexpected EOF`))
	if resp == nil {
		t.Fatalf("scanner error must surface, got nil")
	}
	e := resp.GetError()
	if e == nil || e.Message != "stream read error: unexpected EOF" {
		t.Fatalf("scanner error not surfaced correctly: %v", resp)
	}
}

func TestUiPathToUniversalIgnoredEvents(t *testing.T) {
	for _, ev := range []string{"message_start", "content_block_start", "content_block_stop", "update_loading_message", "update_title", "totally_unknown_event"} {
		resp, _ := UiPathToUniversal("req1", sseEvent(ev, `{}`))
		if resp != nil {
			t.Fatalf("event %q should be ignored (nil), got %v", ev, resp)
		}
	}
}

func TestUniversalToUiPathRegistersToolsAndAttachesBinaryImages(t *testing.T) {
	req := &pb.UniversalRequest{
		System: "sys",
		Messages: []*pb.Message{{
			Role: pb.Message_USER,
			Content: []*pb.ContentPart{
				{Part: &pb.ContentPart_Text{Text: &pb.TextPart{Text: "look"}}},
				{Part: &pb.ContentPart_Image{Image: &pb.ImagePart{
					Source:   &pb.ImagePart_Data{Data: []byte{1, 2, 3}},
					Detail:   "high",
					MimeType: "image/png",
				}}},
				{Part: &pb.ContentPart_ToolCall{ToolCall: &pb.ToolCallPart{
					Id:        "call_ignored",
					Name:      "ReadFile",
					Arguments: `{"path":"a.txt"}`,
				}}},
				{Part: &pb.ContentPart_ToolResult{ToolResult: &pb.ToolResultPart{
					ToolCallId: "call_1",
					Result:     `{"ok":true}`,
					IsError:    true,
				}}},
			},
		}},
		Tools: []*pb.Tool{{
			Name:        "WebFetch",
			Description: "fetch a URL",
			Parameters:  `{"type":"object"}`,
		}, {
			Name:        "AskUserQuestion",
			Description: "unsupported upstream",
			Parameters:  `{}`,
		}},
	}

	out, err := UniversalToUiPath(req)
	if err != nil {
		t.Fatalf("UniversalToUiPath: %v", err)
	}
	for _, marker := range []string{
		"System: sys",
		"User: look",
		"[Image attachment=image-1.png media_type=image/png detail=high]",
		"[ToolResult id=call_1 is_error=true]\n{\"ok\":true}",
	} {
		if !strings.Contains(out.Content, marker) {
			t.Fatalf("missing marker %q\n--- content ---\n%s", marker, out.Content)
		}
	}
	if strings.Contains(out.Content, "call_ignored") {
		t.Fatalf("tool call metadata should not be replayed as prompt text:\n%s", out.Content)
	}
	if len(out.Attachments) != 1 {
		t.Fatalf("attachments=%d want 1", len(out.Attachments))
	}
	if got := out.Attachments[0]; got.Name != "image-1.png" || got.Type != "image/png" || got.AttachmentSource != "Screenshot" || string(got.Content) != string([]byte{1, 2, 3}) {
		t.Fatalf("image attachment not preserved: %+v", got)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools=%d want 1: %+v", len(out.Tools), out.Tools)
	}
	tool := out.Tools[0].(map[string]interface{})
	fn := tool["function"].(map[string]interface{})
	if tool["type"] != "function" || fn["name"] != "FetchUrl" || fn["description"] != "fetch a URL" {
		t.Fatalf("tool not converted to UiPath function shape: %+v", tool)
	}
	params := fn["parameters"].(map[string]interface{})
	if _, ok := params["properties"]; !ok {
		t.Fatalf("parameters missing properties: %+v", params)
	}
}

func TestUiPathToUniversalToolCalls(t *testing.T) {
	responses, err := UiPathToUniversalResponses("req1", sseEvent("tool_calls", `{"tool_calls":[{"id":"call_1","name":"ReadFile","args":{"path":"a.txt"}},{"name":"List","args":{}}]}`))
	if err != nil {
		t.Fatalf("UiPathToUniversalResponses: %v", err)
	}
	if len(responses) != 2 {
		t.Fatalf("responses=%d want 2", len(responses))
	}
	first := responses[0].GetToolCall()
	if first == nil || first.Id != "call_1" || first.Name != "ReadFile" || first.Arguments != `{"path":"a.txt"}` || first.Index != 0 {
		t.Fatalf("first tool call wrong: %+v", responses[0])
	}
	second := responses[1].GetToolCall()
	if second == nil || second.Id == "" || second.Name != "List" || second.Arguments != `{}` || second.Index != 1 {
		t.Fatalf("second tool call wrong: %+v", responses[1])
	}
}
