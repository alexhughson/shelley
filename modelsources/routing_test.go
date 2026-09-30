package modelsources

import (
	"encoding/json"
	"net/http"
	"testing"

	"shelley.exe.dev/llm/ant"
	"shelley.exe.dev/llm/oai"
	"shelley.exe.dev/models"
)

func TestIntegrationUpstreamAPIRouting(t *testing.T) {
	for _, tc := range []struct {
		name, native, provider, upstream string
		apis                             []string
		want                             models.APIType
		catalogMatch                     bool
	}{
		{name: "responses", native: "gpt-5.5", provider: "openai", upstream: "openai-responses", want: models.APITypeOpenAIResponses, catalogMatch: true},
		{name: "chat overrides responses catalog", native: "gpt-5.5", provider: "openai", upstream: "openai-chat-completions", want: models.APITypeOpenAIChat},
		{name: "messages overrides responses catalog", native: "gpt-5.5", provider: "openai", upstream: "anthropic-messages", want: models.APITypeAnthropicMessages},
		{name: "messages catalog", native: "claude-opus-4-8", provider: "anthropic", upstream: "anthropic-messages", want: models.APITypeAnthropicMessages, catalogMatch: true},
		{name: "responses overrides messages catalog", native: "claude-opus-4-8", provider: "anthropic", upstream: "openai-responses", want: models.APITypeOpenAIResponses},
		{name: "absent preserves catalog preference", native: "claude-opus-4-8", provider: "anthropic", want: models.APITypeAnthropicMessages, catalogMatch: true},
		{name: "invalid preserves catalog preference", native: "claude-opus-4-8", provider: "anthropic", upstream: "invented", want: models.APITypeAnthropicMessages, catalogMatch: true},
		{name: "unadvertised ignored", native: "unknown", upstream: "anthropic-messages", apis: []string{"openai_chat"}, want: models.APITypeOpenAIChat},
		{name: "unknown preferred chat", native: "unknown", upstream: "openai-chat-completions", want: models.APITypeOpenAIChat},
		{name: "unknown no preference", native: "unknown", want: models.APITypeOpenAIResponses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apis := tc.apis
			if apis == nil {
				apis = []string{"openai_responses", "openai_chat", "anthropic_messages", "gemini"}
			}
			body, err := json.Marshal(map[string]any{"id": tc.provider + "/" + tc.native, "native_id": tc.native, "provider": tc.provider, "apis": apis, "upstream": map[string]string{"api_type": tc.upstream}})
			if err != nil {
				t.Fatal(err)
			}
			var m IntegrationModel
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			catalogModel, matched := compatibleCatalogModel(models.All(), m)
			if matched != tc.catalogMatch {
				t.Fatalf("catalog match = %v, want %v", matched, tc.catalogMatch)
			}
			if matched && catalogModel.APIType != tc.want {
				t.Fatalf("catalog API = %s, want %s", catalogModel.APIType, tc.want)
			}
			api, svc, ok := buildIntegrationService(models.All(), m, "https://arbitrary.example", &http.Client{})
			if !ok || api != tc.want {
				t.Fatalf("built API = %s, %v, want %s", api, ok, tc.want)
			}
			switch tc.want {
			case models.APITypeOpenAIResponses:
				if _, ok := svc.(*oai.ResponsesService); !ok {
					t.Fatalf("wrong service %T", svc)
				}
			case models.APITypeOpenAIChat:
				if _, ok := svc.(*oai.Service); !ok {
					t.Fatalf("wrong service %T", svc)
				}
			case models.APITypeAnthropicMessages:
				if _, ok := svc.(*ant.Service); !ok {
					t.Fatalf("wrong service %T", svc)
				}
			}
		})
	}
}
