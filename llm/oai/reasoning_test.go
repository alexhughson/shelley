package oai

import (
	"testing"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/models/modelsdev"
)

func TestExplicitReasoningPolicy(t *testing.T) {
	exact := &modelsdev.ReasoningCapabilities{Supported: true, Levels: []llm.ThinkingLevel{llm.ThinkingLevelLow, llm.ThinkingLevelHigh}}
	offOnly := &modelsdev.ReasoningCapabilities{Supported: true, Levels: []llm.ThinkingLevel{llm.ThinkingLevelOff}}
	disabled := &modelsdev.ReasoningCapabilities{}
	for _, tc := range []struct {
		name           string
		caps           *modelsdev.ReasoningCapabilities
		req            llm.Request
		service        llm.ThinkingLevel
		verbatim, want string
		handled        bool
	}{
		{name: "absent leaves legacy path", service: llm.ThinkingLevelMax},
		{name: "support alone leaves legacy path", caps: &modelsdev.ReasoningCapabilities{Supported: true}, service: llm.ThinkingLevelMax},
		{name: "default rounds lower", caps: exact, service: llm.ThinkingLevelMedium, want: "low", handled: true},
		{name: "request wins", caps: exact, req: llm.Request{ThinkingLevel: llm.ThinkingLevelMax}, service: llm.ThinkingLevelLow, verbatim: "custom", want: "high", handled: true},
		{name: "off when unadvertised", caps: exact, req: llm.Request{ThinkingLevel: llm.ThinkingLevelOff}, verbatim: "custom", want: "low", handled: true},
		{name: "off-only default", caps: offOnly, service: llm.ThinkingLevelMedium, want: "none", handled: true},
		{name: "off-only request", caps: offOnly, req: llm.Request{ThinkingLevel: llm.ThinkingLevelHigh}, want: "none", handled: true},
		{name: "unset stays unset", caps: exact, handled: true},
		{name: "service off stays unset", caps: exact, service: llm.ThinkingLevelOff, handled: true},
		{name: "disabled default", caps: disabled, service: llm.ThinkingLevelHigh, handled: true},
		{name: "disabled request", caps: disabled, req: llm.Request{ThinkingLevel: llm.ThinkingLevelHigh}, handled: true},
		{name: "service verbatim", caps: exact, service: llm.ThinkingLevelLow, verbatim: "custom", want: "custom", handled: true},
		{name: "disabled preserves service verbatim", caps: disabled, req: llm.Request{ThinkingLevel: llm.ThinkingLevelHigh}, verbatim: "custom", want: "custom", handled: true},
		{name: "request verbatim", caps: disabled, req: llm.Request{ReasoningEffort: "request-custom"}, verbatim: "service-custom", want: "request-custom", handled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, handled := overrideReasoningEffort(tc.caps, &tc.req, tc.service, tc.verbatim)
			if got != tc.want || handled != tc.handled {
				t.Fatalf("effort = %q, %v; want %q, %v", got, handled, tc.want, tc.handled)
			}
		})
	}
}
