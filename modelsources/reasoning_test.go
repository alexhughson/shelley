package modelsources

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/ant"
	"shelley.exe.dev/llm/oai"
	"shelley.exe.dev/models"
	"shelley.exe.dev/models/modelsdev"
)

// Exercise the actual discovery JSON boundary, both service construction paths,
// capability consumers, defaults, and the serialized request. The arbitrary
// hostname must not be needed to recognize an integration's reasoning metadata.
func TestIntegrationReasoningMetadata(t *testing.T) {
	for _, api := range []string{"openai_responses", "openai_chat", "anthropic_messages"} {
		for _, tc := range []struct {
			name, upstream        string
			known                 bool
			supported             bool
			levels                []string
			effort, defaultEffort string
		}{
			{name: "Muse 1.2 exact", upstream: `{"supports_reasoning":true,"reasoning_levels":["low","medium","high","xhigh"],"api_type":"openai-responses"}`, supported: true, levels: []string{"low", "medium", "high", "xhigh"}, effort: "xhigh", defaultEffort: "medium"},
			{name: "Muse 1.3 exact", upstream: `{"supports_reasoning":true,"reasoning_levels":["low","medium","high","xhigh"]}`, supported: true, levels: []string{"low", "medium", "high", "xhigh"}, effort: "xhigh", defaultEffort: "medium"},
			{name: "catalog conflict", upstream: `{"supports_reasoning":true,"reasoning_levels":["high","low","high"]}`, known: true, supported: true, levels: []string{"low", "high"}, effort: "high", defaultEffort: "low"},
			{name: "off-only", upstream: `{"supports_reasoning":true,"reasoning_levels":["off"]}`, supported: true, levels: []string{"off"}, effort: "none", defaultEffort: "off"},
			{name: "off", upstream: `{"supports_reasoning":true,"reasoning_levels":["off","low","medium","high","xhigh"]}`, supported: true, levels: []string{"off", "low", "medium", "high", "xhigh"}, effort: "xhigh", defaultEffort: "medium"},
			{name: "false", upstream: `{"supports_reasoning":false,"reasoning_levels":["low","high"]}`, known: true},
			{name: "invalid mixed", upstream: `{"supports_reasoning":true,"reasoning_levels":["high","ultra"]}`},
			{name: "toggle", upstream: `{"supports_reasoning":true,"reasoning_levels":["none","thinking"]}`},
			{name: "unknown", upstream: `{"supports_reasoning":true,"reasoning_levels":["ultra"]}`},
			{name: "empty", upstream: `{"supports_reasoning":true,"reasoning_levels":[]}`},
			{name: "absent", supported: true, effort: "xhigh", defaultEffort: "medium"},
			{name: "support only", upstream: `{"supports_reasoning":true}`, supported: true, effort: "xhigh", defaultEffort: "medium"},
		} {
			t.Run(api+"/"+tc.name, func(t *testing.T) {
				const base = "https://arbitrary-endpoint.example.test"
				native, provider := "meta/muse-spark-1.2-contributor", "opencode"
				if tc.name == "absent" || tc.name == "support only" {
					native = "unknown-model-not-in-catalog"
				}
				if tc.name == "Muse 1.3 exact" {
					native = "meta/muse-spark-1.3-contributor"
				}
				if tc.known {
					native, provider = "gpt-5.5", "openai"
					if api == "anthropic_messages" {
						native, provider = "claude-opus-4-8", "anthropic"
					}
				}
				model := map[string]any{"id": provider + "/" + native, "native_id": native, "provider": provider, "apis": []string{api}}
				if tc.upstream != "" {
					var upstream map[string]any
					if err := json.Unmarshal([]byte(tc.upstream), &upstream); err != nil {
						t.Fatal(err)
					}
					upstream["api_type"] = map[string]string{"openai_responses": "openai-responses", "openai_chat": "openai-chat-completions", "anthropic_messages": "anthropic-messages"}[api]
					model["upstream"] = upstream
					model["apis"] = []string{"openai_responses", "openai_chat", "anthropic_messages", "gemini"}
				}
				catalogJSON, err := json.Marshal(map[string]any{"schema_version": 1, "models": []any{model}})
				if err != nil {
					t.Fatal(err)
				}
				var captured map[string]any
				client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Host != "arbitrary-endpoint.example.test" {
						t.Fatalf("unexpected hostname: %s", req.URL)
					}
					body := string(catalogJSON)
					if req.Method == http.MethodPost {
						wantPath := map[string]string{"openai_responses": "/v1/responses", "openai_chat": "/v1/chat/completions", "anthropic_messages": "/v1/messages"}[api]
						if req.URL.Path != wantPath {
							t.Fatalf("wire path = %s, want %s", req.URL.Path, wantPath)
						}
						if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
							t.Fatal(err)
						}
						switch api {
						case "openai_chat":
							body = `{"id":"test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
						case "openai_responses":
							body = `{"id":"test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
						case "anthropic_messages":
							body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
						}
					} else if req.URL.Path != "/models.json" {
						t.Fatalf("unexpected discovery URL: %s", req.URL)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}
				var catalog llmIntegrationModelCatalog
				if !fetchJSON(t.Context(), client, base+"/models.json", &catalog) {
					t.Fatal("discovery failed")
				}
				integration := &LLMIntegrationConfig{Name: "custom", Host: "arbitrary-endpoint.example.test", URL: base, Models: integrationModelsFromCatalog(catalog)}
				built := Build(models.All(), []Source{LLMIntegration(integration, "")}, client, nil)
				if len(built) != 1 {
					t.Fatalf("built = %+v", built)
				}
				svc := built[0].Service
				if got := llm.SupportsReasoning(svc); got != tc.supported {
					t.Fatalf("supported = %v, want %v", got, tc.supported)
				}
				var levels []string
				for _, level := range llm.SupportedReasoningLevels(svc) {
					levels = append(levels, level.Name())
				}
				if !reflect.DeepEqual(levels, tc.levels) {
					t.Fatalf("levels = %v, want %v", levels, tc.levels)
				}
				wantDefault := tc.defaultEffort
				if api == "openai_chat" {
					wantDefault = ""
				}
				if got := llm.ServiceDefaultReasoningLevel(svc); got != wantDefault {
					t.Fatalf("default = %q, want %q", got, wantDefault)
				}
				requestLevels := []llm.ThinkingLevel{llm.ThinkingLevelDefault, llm.ThinkingLevelXHigh}
				if tc.name == "off" {
					requestLevels = append(requestLevels, llm.ThinkingLevelOff)
				}
				for _, level := range requestLevels {
					captured = nil
					_, err := svc.Do(t.Context(), &llm.Request{ThinkingLevel: level, Messages: []llm.Message{{Role: llm.MessageRoleUser, Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}}}}})
					if err != nil {
						t.Fatal(err)
					}
					if captured["model"] != native {
						t.Fatalf("wire model = %v, want %s", captured["model"], native)
					}
					want := tc.effort
					if level == llm.ThinkingLevelDefault {
						want = wantDefault
					}
					if tc.name == "off-only" && (level != llm.ThinkingLevelDefault || api != "openai_chat") {
						want = "none"
						if api == "anthropic_messages" {
							want = ""
						}
					}
					if level == llm.ThinkingLevelOff {
						want = "none" // Existing OpenAI wire spelling of canonical off.
						if api == "anthropic_messages" {
							want = ""
							if captured["thinking"] != nil {
								t.Fatalf("off emitted thinking: %v", captured)
							}
						}
					}
					if (tc.name == "absent" || tc.name == "support only") && api == "openai_chat" && level == llm.ThinkingLevelXHigh {
						want = "high"
					}
					var got string
					switch api {
					case "openai_chat":
						got, _ = captured["reasoning_effort"].(string)
					case "openai_responses":
						if r, ok := captured["reasoning"].(map[string]any); ok {
							got, _ = r["effort"].(string)
						}
					case "anthropic_messages":
						if tc.name == "absent" || tc.name == "support only" {
							if _, ok := captured["thinking"].(map[string]any); !ok {
								t.Fatalf("missing legacy budget thinking: %v", captured)
							}
							continue
						}
						if r, ok := captured["output_config"].(map[string]any); ok {
							got, _ = r["effort"].(string)
						}
						if !tc.supported && captured["thinking"] != nil {
							t.Fatalf("disabled thinking emitted: %v", captured)
						}
					}
					if got != want {
						t.Errorf("level %s: wire effort = %q, want %q; request %v", level.Name(), got, want, captured)
					}
				}
			})
		}
	}
}

func TestIntegrationAbsentReasoningMetadataPreservesCatalog(t *testing.T) {
	for _, native := range []string{"gpt-5.5", "claude-opus-4-8", "meta/muse-spark-1.2-contributor"} {
		for _, upstream := range []string{"null", `{}`, `{"api_type":"openai-responses"}`, `{"supports_reasoning":true}`, `{"supports_reasoning":true,"reasoning_levels":null}`} {
			t.Run(native+upstream, func(t *testing.T) {
				var model IntegrationModel
				if err := json.Unmarshal([]byte(`{"upstream":`+upstream+`}`), &model); err != nil {
					t.Fatal(err)
				}
				model.ID, model.NativeID = native, native
				model.APIs = []string{"openai_responses"}
				if native == "claude-opus-4-8" {
					model.APIs = []string{"anthropic_messages"}
					model.Provider = "anthropic"
				} else if native == "gpt-5.5" {
					model.Provider = "openai"
				}
				if override := model.reasoningOverride(); override != nil {
					t.Fatalf("unexpected override: %+v", override)
				}
				_, svc, ok := buildIntegrationService(models.All(), model, "https://arbitrary.example", &http.Client{})
				if !ok {
					t.Fatal("not built")
				}
				caps, found := modelsdev.LookupReasoningCapabilities("https://arbitrary.example", native)
				if !found {
					t.Fatal("test needs a known catalog entry")
				}
				if llm.SupportsReasoning(svc) != caps.Supported || !reflect.DeepEqual(llm.SupportedReasoningLevels(svc), caps.Levels) {
					t.Fatalf("service no longer follows catalog: %+v", caps)
				}
			})
		}
	}
}

func TestIntegrationReasoningMetadataValidation(t *testing.T) {
	for _, tc := range []struct {
		upstream  string
		supported bool
		levels    []llm.ThinkingLevel
	}{
		{`{"supports_reasoning":false}`, false, nil},
		{`{"reasoning_levels":["minimal","low","medium","high","xhigh","max"]}`, true, []llm.ThinkingLevel{llm.ThinkingLevelMinimal, llm.ThinkingLevelLow, llm.ThinkingLevelMedium, llm.ThinkingLevelHigh, llm.ThinkingLevelXHigh, llm.ThinkingLevelMax}},
		{`{"reasoning_levels":["default"]}`, false, nil},
		{`{"reasoning_levels":["off"]}`, true, []llm.ThinkingLevel{llm.ThinkingLevelOff}},
		{`{"reasoning_levels":["HIGH"]}`, false, nil},
		{`{"reasoning_levels":[" high "]}`, false, nil},
	} {
		t.Run(tc.upstream, func(t *testing.T) {
			var m IntegrationModel
			if err := json.Unmarshal([]byte(`{"id":"unknown-model","upstream":`+tc.upstream+`}`), &m); err != nil {
				t.Fatal(err)
			}
			caps := m.reasoningOverride()
			if caps == nil || caps.Supported != tc.supported || !reflect.DeepEqual(caps.Levels, tc.levels) {
				t.Fatalf("override = %+v, want supported %v, levels %v", caps, tc.supported, tc.levels)
			}
		})
	}
}

func TestIntegrationLevelsOverrideCatalogFalse(t *testing.T) {
	const native = "gpt-4.1-2025-04-14"
	old, found := modelsdev.LookupReasoningCapabilities("https://arbitrary.example", native)
	if !found || old.Supported {
		t.Fatalf("fixture must be catalog-known nonreasoning: %+v, %v", old, found)
	}
	var m IntegrationModel
	if err := json.Unmarshal([]byte(`{"native_id":"`+native+`","upstream":{"reasoning_levels":["low","high"]}}`), &m); err != nil {
		t.Fatal(err)
	}
	caps := m.reasoningOverride()
	if caps == nil || !caps.Supported || !reflect.DeepEqual(caps.Levels, []llm.ThinkingLevel{llm.ThinkingLevelLow, llm.ThinkingLevelHigh}) {
		t.Fatalf("explicit levels did not override catalog: %+v", caps)
	}
}

func TestMuseProxyEffortsOverrideBroaderCatalog(t *testing.T) {
	for _, endpoint := range []string{"https://api.opencode.ai/go/v1", "https://openrouter.ai/api/v1"} {
		t.Run(endpoint, func(t *testing.T) {
			const native = "meta/muse-spark-1.3-contributor"
			catalog, found := modelsdev.LookupReasoningCapabilities(endpoint, native)
			if !found || !slices.Contains(catalog.Levels, llm.ThinkingLevelMinimal) {
				t.Fatalf("fixture must advertise broader catalog efforts: %+v, %v", catalog, found)
			}
			if strings.Contains(endpoint, "openrouter") && !slices.Contains(catalog.Levels, llm.ThinkingLevelMax) {
				t.Fatalf("OpenRouter fixture must advertise max: %+v", catalog)
			}
			var m IntegrationModel
			if err := json.Unmarshal([]byte(`{"native_id":"`+native+`","upstream":{"supports_reasoning":true,"reasoning_levels":["low","medium","high","xhigh"]}}`), &m); err != nil {
				t.Fatal(err)
			}
			caps := m.reasoningOverride()
			if !caps.Supported || !reflect.DeepEqual(caps.Levels, []llm.ThinkingLevel{llm.ThinkingLevelLow, llm.ThinkingLevelMedium, llm.ThinkingLevelHigh, llm.ThinkingLevelXHigh}) {
				t.Fatalf("proxy efforts = %+v", caps)
			}
		})
	}
}

// Missing effort lists must not replace catalog factories or turn their
// models.dev capabilities into endpoint overrides (including support-only JSON).
func TestIntegrationUnspecifiedControlsUseCatalogBuild(t *testing.T) {
	const base = "https://arbitrary.example"
	client := &http.Client{}
	catalog := append(models.All(), models.Model{
		ID: "configured-chat", APIModelName: "unknown-configured-chat", Provider: models.ProviderOpenAI, APIType: models.APITypeOpenAIChat,
		Build: func(base, key string, httpc *http.Client) llm.Service {
			return &oai.Service{Model: oai.Model{ModelName: "unknown-configured-chat"}, ModelURL: base + "/v1", APIKey: key, HTTPC: httpc, ReasoningEffort: "none", ThinkingLevel: llm.ThinkingLevelXHigh, MaxTokens: 1234}
		},
	}, models.Model{
		ID: "configured-responses", APIModelName: "unknown-configured-responses", Provider: models.ProviderOpenAI, APIType: models.APITypeOpenAIResponses,
		Build: func(base, key string, httpc *http.Client) llm.Service {
			return &oai.ResponsesService{Model: oai.Model{ModelName: "unknown-configured-responses"}, ModelURL: base + "/v1", APIKey: key, HTTPC: httpc, ReasoningEffort: "custom-effort", ThinkingLevel: llm.ThinkingLevelHigh, MaxTokens: 4321}
		},
	})
	missingLevels := 0
	for _, entry := range catalog {
		if entry.APIType != models.APITypeAnthropicMessages && entry.APIType != models.APITypeOpenAIResponses && entry.APIType != models.APITypeOpenAIChat {
			continue
		}
		baseline := entry.Build(base, "implicit", client)
		if llm.SupportsReasoning(baseline) && llm.SupportedReasoningLevels(baseline) == nil {
			missingLevels++
		}
		for _, upstream := range []string{"null", `{}`, `{"supports_reasoning":true}`, `{"supports_reasoning":true,"reasoning_levels":null}`, `{"api_type":"` + string(entry.APIType) + `","supports_reasoning":true}`} {
			t.Run(entry.ID+upstream, func(t *testing.T) {
				model := IntegrationModel{ID: entry.ID, NativeID: entry.APIModelName, Provider: string(entry.Provider), APIs: []string{"openai_responses", "openai_chat", "anthropic_messages"}}
				if err := json.Unmarshal([]byte(upstream), &model.Upstream); err != nil {
					t.Fatal(err)
				}
				api, svc, ok := buildIntegrationService(catalog, model, base, client)
				if !ok || api != entry.APIType || !reflect.DeepEqual(svc, baseline) {
					t.Fatalf("catalog Build changed: %s, %T %+v; want %s, %T %+v", api, svc, svc, entry.APIType, baseline, baseline)
				}
				if got, want := llm.ServiceDefaultReasoningLevel(svc), llm.ServiceDefaultReasoningLevel(baseline); got != want {
					t.Fatalf("default = %q, want catalog %q", got, want)
				}
			})
		}
	}
	if missingLevels == 0 {
		t.Fatal("test must cover reasoning-capable built-in models without effort metadata")
	}
}

// A catalog host may advertise effort controls for a budget-thinking model.
// Support-only integration metadata must not promote those to an override.
func TestIntegrationSupportOnlyPreservesBudgetThinking(t *testing.T) {
	const base = "https://api.pioneer.ai"
	supported := true
	model := IntegrationModel{ID: "claude-sonnet-4-5", NativeID: "claude-sonnet-4-5", Provider: "anthropic", APIs: []string{"anthropic_messages"}, Upstream: &IntegrationModelUpstream{SupportsReasoning: &supported}}
	caps, found := modelsdev.LookupReasoningCapabilities(base, model.NativeID)
	if !found || len(caps.Levels) == 0 {
		t.Fatal("fixture must have endpoint effort metadata for this budget model")
	}
	_, svc, ok := buildIntegrationService(models.All(), model, base, &http.Client{})
	if !ok {
		t.Fatal("not built")
	}
	if got := svc.(*ant.Service).ReasoningOverride; got != nil {
		t.Fatalf("support-only metadata invented endpoint controls: %+v", got)
	}
}
