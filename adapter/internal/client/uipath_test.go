package client

import (
	"io"
	"mime/multipart"
	"strings"
	"testing"
)

func TestEncodeUiPathRequestWithAttachmentUsesMultipart(t *testing.T) {
	req := &UiPathRequest{
		Agent:            "aria-autopilot",
		Content:          "look",
		Context:          map[string]interface{}{},
		Resources:        map[string]interface{}{},
		Tools:            []interface{}{},
		UnavailableTools: []interface{}{},
		UserConfig:       map[string]interface{}{},
		Attachments: []UiPathAttachment{{
			Name:             "image-1.png",
			Type:             "image/png",
			Preamble:         "User provided image attachment.",
			AttachmentSource: "Screenshot",
			Content:          []byte{1, 2, 3},
		}},
	}

	body, contentType, debugPayload, err := encodeUiPathRequest(req)
	if err != nil {
		t.Fatalf("encodeUiPathRequest: %v", err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
		t.Fatalf("contentType=%q, want multipart", contentType)
	}
	if !strings.Contains(debugPayload, `"attachments_metadata"`) || !strings.Contains(debugPayload, `"attachmentSource":"Screenshot"`) {
		t.Fatalf("debug payload missing attachment metadata: %s", debugPayload)
	}

	reader := multipart.NewReader(body, strings.TrimPrefix(contentType, "multipart/form-data; boundary="))
	requestPart, err := reader.NextPart()
	if err != nil {
		t.Fatalf("request part: %v", err)
	}
	if requestPart.FormName() != "request" {
		t.Fatalf("first form name=%q, want request", requestPart.FormName())
	}
	requestBody, _ := io.ReadAll(requestPart)
	if !strings.Contains(string(requestBody), `"attachments_metadata"`) {
		t.Fatalf("request body missing attachments_metadata: %s", string(requestBody))
	}

	filePart, err := reader.NextPart()
	if err != nil {
		t.Fatalf("file part: %v", err)
	}
	if filePart.FormName() != "files" || filePart.FileName() != "image-1.png" {
		t.Fatalf("file part name=%q filename=%q", filePart.FormName(), filePart.FileName())
	}
	fileBody, _ := io.ReadAll(filePart)
	if string(fileBody) != string([]byte{1, 2, 3}) {
		t.Fatalf("file body=%v", fileBody)
	}
}
