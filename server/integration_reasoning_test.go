package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"shelley.exe.dev/models"
	"shelley.exe.dev/modelsources"
)

func TestIntegrationReasoningAPI(t *testing.T) {
	var integration modelsources.IntegrationModel
	if err := json.Unmarshal([]byte(`{"id":"opencode/meta/muse-spark-1.3-contributor","provider":"opencode","native_id":"meta/muse-spark-1.3-contributor","apis":["openai_responses"],"upstream":{"supports_reasoning":true,"reasoning_levels":["low","medium","high","xhigh"]}}`), &integration); err != nil {
		t.Fatal(err)
	}
	built := modelsources.Build(models.All(), []modelsources.Source{modelsources.LLMIntegration(&modelsources.LLMIntegrationConfig{Name: "proxy", Host: "arbitrary.example", URL: "https://arbitrary.example", Models: []modelsources.IntegrationModel{integration}}, "")}, &http.Client{}, nil)
	mgr, err := models.NewManager(&models.Config{Models: built})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{llmManager: mgr, logger: slog.Default()}
	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	var got []ModelInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("models = %+v", got)
	}
	m := &got[0]
	if !m.SupportsReasoning || !reflect.DeepEqual(m.ReasoningLevels, []string{"low", "medium", "high", "xhigh"}) || m.DefaultReasoningLevel != "medium" {
		t.Fatalf("model = %+v", m)
	}
	if msg := validateModelReasoningLevel(m, "xhigh"); msg != "" {
		t.Fatal(msg)
	}
	for _, level := range []string{"minimal", "max", "off", "ultra", "thinking", "none"} {
		if msg := validateModelReasoningLevel(m, level); msg == "" {
			t.Errorf("accepted unadvertised level %q", level)
		}
	}
	if got, changed := roundModelReasoningLevel(m, "max"); got != "xhigh" || !changed {
		t.Fatalf("round max = %q, %v", got, changed)
	}
}
