package converter

import (
	"encoding/json"
	"fmt"
	"mime"
	"strings"

	"uipath-adapter/internal/client"
	pb "uipath-adapter/proto"
)

// platformField 构造 UiPath context 判别字段格式
func platformField(value string) map[string]interface{} {
	return map[string]interface{}{
		"Description": "",
		"Value":       value,
	}
}

// UniversalToUiPath 将 Universal 请求转换为 UiPath 格式
func UniversalToUiPath(req *pb.UniversalRequest) (*client.UiPathRequest, error) {
	// 构建 prompt（拼接所有消息）
	var prompt strings.Builder
	var attachments []client.UiPathAttachment

	// 添加 system
	if req.System != "" {
		prompt.WriteString("System: ")
		prompt.WriteString(req.System)
		prompt.WriteString("\n\n")
	}

	// 添加消息
	for _, msg := range req.Messages {
		role := roleToString(msg.Role)
		prompt.WriteString(role)
		prompt.WriteString(": ")

		// 提取文本内容
		for _, part := range msg.Content {
			fragment, attachment := contentPartPrompt(part, len(attachments)+1)
			prompt.WriteString(fragment)
			if attachment != nil {
				attachments = append(attachments, *attachment)
			}
		}
		prompt.WriteString("\n\n")
	}

	// 构建 context — PlatformType 等字段必须是对象格式供 context_discriminator() 读取
	context := map[string]interface{}{
		"PlatformType":           platformField("Studio Desktop"),
		"PlatformContextType":    platformField("Studio Project"),
		"PlatformSubContextType": platformField("Workflow Designer"),
		"OperatingModel":         "",
		"UserLocalTime":          "",
		"ContextData": map[string]interface{}{
			"CurrentFile": map[string]interface{}{
				"FileExtension":       "",
				"FilePath":            "",
				"FileType":            "Unknown",
				"IsCurrentFileEmpty":  "Unknown",
				"VariableDefinitions": []interface{}{},
			},
			"CurrentProject": map[string]interface{}{
				"OrchestratorFolderId": "",
				"ProjectDescription":   "",
				"ProjectFramework":     "Windows",
				"ProjectId":            "",
				"ProjectName":          "",
				"ProjectType":          "Process",
			},
		},
	}

	// 构建 user_config
	userConfig := map[string]interface{}{}

	if req.Config != nil {
		if req.Config.Temperature > 0 {
			userConfig["temperature"] = req.Config.Temperature
		}
		if req.Config.MaxTokens > 0 {
			userConfig["maxTokens"] = req.Config.MaxTokens
		}
	}

	return &client.UiPathRequest{
		Agent:            "aria-autopilot",
		Content:          strings.TrimSpace(prompt.String()),
		Context:          context,
		Resources:        make(map[string]interface{}),
		Tools:            uiPathTools(req.Tools),
		UnavailableTools: []interface{}{},
		UserConfig:       userConfig,
		Attachments:      attachments,
	}, nil
}

func contentPartPrompt(part *pb.ContentPart, attachmentIndex int) (string, *client.UiPathAttachment) {
	if part == nil {
		return "", nil
	}
	if textPart := part.GetText(); textPart != nil {
		return textPart.Text, nil
	}
	if image := part.GetImage(); image != nil {
		if url := image.GetUrl(); url != "" {
			return fmt.Sprintf("[Image media_type=%s detail=%s source_url=%s]", imageMimeType(image.MimeType), imageDetail(image.Detail), url), nil
		}
		if data := image.GetData(); len(data) > 0 {
			mimeType := imageMimeType(image.MimeType)
			name := fmt.Sprintf("image-%d%s", attachmentIndex, extensionForMime(mimeType))
			return fmt.Sprintf("[Image attachment=%s media_type=%s detail=%s]", name, mimeType, imageDetail(image.Detail)), &client.UiPathAttachment{
				Name:             name,
				Type:             mimeType,
				Preamble:         "User provided image attachment.",
				AttachmentSource: "Screenshot",
				Content:          data,
			}
		}
		return "[Image]", nil
	}
	if toolCall := part.GetToolCall(); toolCall != nil {
		return "", nil
	}
	if toolResult := part.GetToolResult(); toolResult != nil {
		if toolResult.IsError {
			return fmt.Sprintf("[ToolResult id=%s is_error=true]\n%s", toolResult.ToolCallId, toolResult.Result), nil
		}
		return fmt.Sprintf("[ToolResult id=%s]\n%s", toolResult.ToolCallId, toolResult.Result), nil
	}
	return "", nil
}

func uiPathTools(tools []*pb.Tool) []interface{} {
	if len(tools) == 0 {
		return []interface{}{}
	}
	out := make([]interface{}, 0, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Name == "" || isUiPathUnsupportedTool(tool.Name) {
			continue
		}
		parameters := toolParameters(tool.Parameters)
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        uiPathUpstreamToolName(tool.Name),
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return out
}

func toolParameters(raw string) map[string]interface{} {
	if strings.TrimSpace(raw) == "" {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil || parsed == nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	if _, ok := parsed["properties"]; !ok {
		parsed["properties"] = map[string]interface{}{}
	}
	return parsed
}

func isUiPathUnsupportedTool(name string) bool {
	switch name {
	case "AskUserQuestion", "EnterWorktree", "ExitWorktree", "NotebookEdit", "WaitForMcpServers", "Workflow":
		return true
	default:
		return false
	}
}

func uiPathUpstreamToolName(name string) string {
	if name == "WebFetch" {
		return "FetchUrl"
	}
	return name
}

func imageMimeType(value string) string {
	if strings.TrimSpace(value) == "" {
		return "image/png"
	}
	return value
}

func imageDetail(value string) string {
	if strings.TrimSpace(value) == "" {
		return "auto"
	}
	return value
}

func extensionForMime(mimeType string) string {
	extensions, err := mime.ExtensionsByType(mimeType)
	if err == nil && len(extensions) > 0 {
		return extensions[0]
	}
	switch mimeType {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// UiPathToUniversal 将 UiPath 事件转换为 Universal 响应
// 真实 API 事件格式:
//   - message_delta: {"delta": {"content": "...", "type": "Response"}, "finish_reason": null}
//   - usage: {"usage_by_model": {...}, "turn_totals": {"prompt_tokens": N, "completion_tokens": N}}
//   - update_loading_message: 加载状态文字（忽略）
//   - update_title: 对话标题更新（忽略）
func UiPathToUniversal(requestID string, event *client.UiPathStreamEvent) (*pb.UniversalResponse, error) {
	responses, err := UiPathToUniversalResponses(requestID, event)
	if err != nil || len(responses) == 0 {
		return nil, err
	}
	return responses[0], nil
}

func UiPathToUniversalResponses(requestID string, event *client.UiPathStreamEvent) ([]*pb.UniversalResponse, error) {
	switch event.Event {
	case "message_start", "content_block_start", "content_block_stop", "message_stop",
		"update_loading_message", "update_title":
		// 这些事件不需要转发给客户端
		return nil, nil

	case "message_delta":
		// 真实 API 格式: {"delta": {"content": "...", "type": "Response"}, "finish_reason": null}
		var msgDelta struct {
			Delta struct {
				Content string `json:"content"`
				Type    string `json:"type"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		}
		if err := json.Unmarshal(event.Data, &msgDelta); err != nil {
			return nil, fmt.Errorf("failed to parse message_delta: %w", err)
		}

		if msgDelta.Delta.Content == "" {
			return nil, nil
		}

		return []*pb.UniversalResponse{{
			RequestId: requestID,
			Response: &pb.UniversalResponse_Chunk{
				Chunk: &pb.ContentChunk{
					Text: msgDelta.Delta.Content,
				},
			},
		}}, nil

	case "content_block_delta":
		// 旧格式兼容（mock 模式使用）
		var delta client.UiPathMessageData
		if err := json.Unmarshal(event.Data, &delta); err != nil {
			return nil, fmt.Errorf("failed to parse delta: %w", err)
		}
		text := delta.Content
		if text == "" {
			text = delta.Text
		}
		if text == "" {
			return nil, nil
		}
		return []*pb.UniversalResponse{{
			RequestId: requestID,
			Response: &pb.UniversalResponse_Chunk{
				Chunk: &pb.ContentChunk{
					Text: text,
				},
			},
		}}, nil

	case "tool_calls":
		var eventBody struct {
			ToolCalls []struct {
				ID   string          `json:"id"`
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(event.Data, &eventBody); err != nil {
			return nil, fmt.Errorf("failed to parse tool_calls: %w", err)
		}
		responses := make([]*pb.UniversalResponse, 0, len(eventBody.ToolCalls))
		for i, call := range eventBody.ToolCalls {
			if call.Name == "" {
				continue
			}
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("call_%s_%d", call.Name, i)
			}
			args := string(call.Args)
			if args == "" {
				args = "{}"
			}
			responses = append(responses, &pb.UniversalResponse{
				RequestId: requestID,
				Response: &pb.UniversalResponse_ToolCall{ToolCall: &pb.ToolCall{
					Id:        id,
					Name:      call.Name,
					Arguments: args,
					Index:     int32(i),
				}},
			})
		}
		return responses, nil

	case "usage":
		// 真实 API 格式: {"usage_by_model": {...}, "turn_totals": {"prompt_tokens": N, "completion_tokens": N}}
		var usageEvent struct {
			TurnTotals struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"turn_totals"`
		}
		if err := json.Unmarshal(event.Data, &usageEvent); err != nil {
			// 如果解析失败，返回 completion 但不含 token 信息
		}
		return []*pb.UniversalResponse{{
			RequestId: requestID,
			Response: &pb.UniversalResponse_Completion{
				Completion: &pb.CompletionInfo{
					FinishReason: "stop",
					InputTokens:  uint32(usageEvent.TurnTotals.PromptTokens),
					OutputTokens: uint32(usageEvent.TurnTotals.CompletionTokens),
				},
			},
		}}, nil

	case "error":
		// 错误
		return []*pb.UniversalResponse{{
			RequestId: requestID,
			Response: &pb.UniversalResponse_Error{
				Error: &pb.ErrorResponse{
					Code:    500,
					Message: string(event.Data),
					Type:    "uipath_error",
				},
			},
		}}, nil
	}

	return nil, nil
}

func roleToString(role pb.Message_Role) string {
	switch role {
	case pb.Message_USER:
		return "User"
	case pb.Message_ASSISTANT:
		return "Assistant"
	case pb.Message_SYSTEM:
		return "System"
	default:
		return "User"
	}
}
