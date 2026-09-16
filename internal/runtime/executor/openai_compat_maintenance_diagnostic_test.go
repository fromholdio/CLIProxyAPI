package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestMaintenanceToolImageDelivery(t *testing.T) {
	tests := []struct {
		name            string
		stream          bool
		inputModalities []string
		wantString      bool
		directImage     bool
	}{
		{name: "non-stream text-only", stream: false, inputModalities: []string{"text"}, wantString: true},
		{name: "stream text-only", stream: true, inputModalities: []string{"text"}, wantString: true},
		{name: "stream multimodal", stream: true, inputModalities: []string{"text", "image"}, wantString: false},
		{name: "stream unspecified", stream: true, inputModalities: nil, wantString: false},
		{name: "non-stream multimodal", stream: false, inputModalities: []string{"text", "image"}, wantString: false},
		{name: "non-stream unspecified", stream: false, inputModalities: nil, wantString: false},
		{name: "non-stream text-only with direct image", inputModalities: []string{"text"}, wantString: true, directImage: true},
		{name: "stream text-only with direct image", stream: true, inputModalities: []string{"text"}, wantString: true, directImage: true},
		{name: "non-stream multimodal with direct image", inputModalities: []string{"text", "image"}, directImage: true},
		{name: "stream multimodal with direct image", stream: true, inputModalities: []string{"text", "image"}, directImage: true},
		{name: "non-stream unspecified with direct image", directImage: true},
		{name: "stream unspecified with direct image", stream: true, directImage: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				if tt.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer server.Close()

			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{{
					Name: "compat",
					Models: []config.OpenAICompatibilityModel{{
						Name:            "mapped-model",
						Alias:           "claude-client",
						InputModalities: tt.inputModalities,
					}},
				}},
			})
			auth := &cliproxyauth.Auth{
				Provider: "openai-compatibility",
				Attributes: map[string]string{
					"base_url":     server.URL + "/v1",
					"api_key":      "test",
					"compat_name":  "compat",
					"provider_key": "compat",
				},
			}
			payload := []byte(`{"model":"claude-client","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"inspect_image","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"image inspected"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}]}`)
			if tt.directImage {
				// Identical image bytes make URL-based subtraction insufficient.
				payload, _ = sjson.SetRawBytes(payload, "messages.1.content.-1", []byte(`{"type":"text","text":"Images returned by the preceding tool call(s):"}`))
				payload, _ = sjson.SetRawBytes(payload, "messages.1.content.-1", []byte(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}`))
			}

			if tt.directImage && gjson.GetBytes(payload, "messages.1.content.#").Int() != 3 {
				t.Fatal("direct-image fixture must use sibling content")
			}
			if !gjson.ValidBytes(payload) {
				t.Fatal("invalid test fixture")
			}

			req := cliproxyexecutor.Request{Model: "mapped-model", Payload: payload}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatClaude,
				ResponseFormat: sdktranslator.FormatOpenAI,
				Stream:         tt.stream,
			}

			if tt.stream {
				result, errExecute := executor.ExecuteStream(context.Background(), auth, req, opts)
				if errExecute != nil {
					t.Fatalf("ExecuteStream error: %v", errExecute)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error: %v", chunk.Err)
					}
				}
			} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
				t.Fatalf("Execute error: %v", errExecute)
			}

			if tt.wantString {
				if got := gjson.GetBytes(gotBody, "messages.1.content").String(); got != "image inspected\n\n[image omitted: unsupported by upstream]" {
					t.Errorf("tool text/omission marker lost: %q", got)
				}
				wantMessages := int64(2)
				if tt.directImage {
					wantMessages = 3
					if got := gjson.GetBytes(gotBody, "messages.2.content.0.text").String(); got != "Images returned by the preceding tool call(s):" {
						t.Errorf("literal user text changed: %q", got)
					}
					if got := gjson.GetBytes(gotBody, "messages.2.content.1.image_url.url").String(); got != "data:image/png;base64,AA==" {
						t.Errorf("direct image changed: %q", got)
					}
				}
				if got := gjson.GetBytes(gotBody, "messages.#").Int(); got != wantMessages {
					t.Errorf("unexpected relay/message count: %d", got)
				}
			}

			images, userImages, toolImages := 0, 0, 0
			gjson.GetBytes(gotBody, "messages").ForEach(func(_, message gjson.Result) bool {
				role := message.Get("role").String()
				message.Get("content").ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").String() == "image_url" {
						images++
						if role == "user" {
							userImages++
						}
						if role == "tool" {
							toolImages++
						}
					}
					return true
				})
				return true
			})
			t.Logf("captured outgoing request: images=%d user_images=%d tool_images=%d", images, userImages, toolImages)
			if gjson.GetBytes(gotBody, "messages.1.tool_call_id").String() != "call_1" {
				t.Error("tool-result pairing lost")
			}
			wantImages := 1
			if tt.wantString {
				wantImages = 0
			}
			if tt.directImage {
				wantImages++
			}
			if tt.wantString && images != wantImages {
				t.Errorf("text-only destination received %d image parts", images)
			}
			if !tt.wantString && images != wantImages {
				t.Errorf("image-capable or unspecified destination received %d images, want %d", images, wantImages)
			}

		})
	}
}
