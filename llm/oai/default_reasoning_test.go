package oai

import (
	"testing"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/models/modelsdev"
)

func TestServiceDefaultReasoningLevel(t *testing.T) {
	tests := []struct {
		name string
		svc  llm.Service
		want string
	}{
		{"chat none stays verbatim", &Service{ReasoningEffort: "none"}, "none"},
		{"responses none stays verbatim", &ResponsesService{ReasoningEffort: "none"}, "none"},
		{"chat verbatim wins", &Service{ProviderName: "fireworks", ReasoningEffort: "high", ThinkingLevel: llm.ThinkingLevelMedium}, "high"},
		{"chat service default", &Service{ProviderName: "fireworks", ThinkingLevel: llm.ThinkingLevelMedium}, "medium"},
		// Unset: provider picks its own default, which Shelley can't name.
		{"chat unset -> unknown", &Service{ProviderName: "fireworks"}, ""},
		{"responses verbatim wins", &ResponsesService{ProviderName: "openai", ReasoningEffort: "xhigh", ThinkingLevel: llm.ThinkingLevelMedium}, "xhigh"},
		{"responses service default", &ResponsesService{ProviderName: "openai", ThinkingLevel: llm.ThinkingLevelMedium}, "medium"},
		{"responses unset -> unknown", &ResponsesService{ProviderName: "openai"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := llm.ServiceDefaultReasoningLevel(tc.svc); got != tc.want {
				t.Fatalf("ServiceDefaultReasoningLevel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReasoningOverrideDefaultMatchesClamping(t *testing.T) {
	for _, tc := range []struct {
		name  string
		caps  modelsdev.ReasoningCapabilities
		level llm.ThinkingLevel
		want  string
	}{
		{"round to advertised", modelsdev.ReasoningCapabilities{Supported: true, Levels: []llm.ThinkingLevel{llm.ThinkingLevelLow, llm.ThinkingLevelHigh}}, llm.ThinkingLevelMedium, "low"},
		{"retain xhigh", modelsdev.ReasoningCapabilities{Supported: true, Levels: []llm.ThinkingLevel{llm.ThinkingLevelHigh, llm.ThinkingLevelXHigh}}, llm.ThinkingLevelXHigh, "xhigh"},
		{"off-only canonical default", modelsdev.ReasoningCapabilities{Supported: true, Levels: []llm.ThinkingLevel{llm.ThinkingLevelOff}}, llm.ThinkingLevelMedium, "off"},
		{"disabled", modelsdev.ReasoningCapabilities{}, llm.ThinkingLevelHigh, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, svc := range []llm.Service{&Service{ReasoningOverride: &tc.caps, ThinkingLevel: tc.level}, &ResponsesService{ReasoningOverride: &tc.caps, ThinkingLevel: tc.level}} {
				if got := llm.ServiceDefaultReasoningLevel(svc); got != tc.want {
					t.Errorf("%T default = %q, want %q", svc, got, tc.want)
				}
			}
		})
	}
}
