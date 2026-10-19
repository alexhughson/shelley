package claudetool

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"shelley.exe.dev/llm"
)

// SubagentRunner is the interface for running a subagent conversation.
// This is implemented by the server package to avoid import cycles.
type SubagentRunner interface {
	// RunSubagent sends prompt to the subagent conversation and returns an
	// acknowledgement immediately. The subagent reports back with
	// message_parent.
	// modelID is the requested model; empty keeps an existing subagent's
	// model, or gives a new subagent the parent's.
	// reasoning is the user-facing reasoning/thinking level for the subagent
	// (one of "off", "minimal", "low", "medium", "high", "xhigh", "max");
	// an empty string means "use the service/conversation default".
	RunSubagent(ctx context.Context, conversationID, prompt, modelID, reasoning string) (string, error)
}

// subagentReasoningLevels are the user-facing reasoning/thinking levels a
// subagent may be given via the "reasoning" parameter.
var subagentReasoningLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// isValidReasoningLevel reports whether s is a recognized reasoning level.
func isValidReasoningLevel(s string) bool {
	for _, l := range subagentReasoningLevels {
		if s == l {
			return true
		}
	}
	return false
}

// AvailableModel describes a model that subagent and llm_one_shot can use.
type AvailableModel struct {
	Name string // Value the agent passes as the "model" parameter
	ID   string // Model ID to run; may name a specific route (e.g. "gpt-6-sol@sub")
}

// resolveModel maps the agent-facing model name to the model ID to run.
func resolveModel(available []AvailableModel, name string) (string, error) {
	var names []string
	for _, m := range available {
		if m.Name == name {
			return m.ID, nil
		}
		names = append(names, m.Name)
	}
	return "", fmt.Errorf("unknown model %q; available: %s", name, strings.Join(names, ", "))
}

// SubagentDB is the database interface for subagent operations.
// This is implemented by the db package.
type SubagentDB interface {
	// GetOrCreateSubagentConversation retrieves or creates a subagent conversation.
	// Returns the conversation ID and the actual slug used (may differ from requested
	// slug if a suffix was added for uniqueness).
	GetOrCreateSubagentConversation(ctx context.Context, slug, parentID, cwd string) (conversationID, actualSlug string, err error)
}

// SubagentTool provides the ability to spawn and interact with subagent conversations.
type SubagentTool struct {
	DB                   SubagentDB
	ParentConversationID string
	WorkingDir           *MutableWorkingDir
	Runner               SubagentRunner
	AvailableModels      []AvailableModel // Models the agent can choose from
	// ParentReasoning is the parent conversation's user-facing reasoning level
	// (one of "off", "minimal", "low", "medium", "high", "xhigh", "max",
	// or "" for the service default). Subagents inherit this when the "reasoning"
	// parameter is not specified.
	ParentReasoning string
	// DBPath is the Shelley database, named in the description's query
	// for listing subagents.
	DBPath    string
	slugLocks sync.Map // map[string]*sync.Mutex
}

func (s *SubagentTool) lockSlug(slug string) func() {
	lockValue, _ := s.slugLocks.LoadOrStore(slug, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

const subagentName = "subagent"

// subagentListQuery lists the conversation's delegated subagents with their
// working state and latest reply. Workers and /btw readers have a kind; a
// distilled conversation's source is a user-initiated child.
const subagentListQuery = `SELECT slug, agent_working, (SELECT json_extract(c.value, '$.Text') FROM messages m, json_each(m.llm_data, '$.Content') c WHERE m.conversation_id = s.conversation_id AND m.type = 'agent' AND json_extract(c.value, '$.Type') = 2 ORDER BY m.sequence_id DESC, c.key DESC LIMIT 1) AS last_reply FROM conversations s WHERE parent_conversation_id = '$SHELLEY_CONVERSATION_ID' AND NOT user_initiated AND json_extract(conversation_options, '$.kind') IS NULL`

func (s *SubagentTool) description() string {
	return `Delegate tasks to independent conversations, including parallel or
output-heavy work whose details should not fill your context.

Use a new slug to start a subagent; reuse its slug to continue that conversation.
A busy subagent receives the message during its current turn.

The tool returns immediately; the subagent works in the background and reports
back with messages. Its messages will wake you, so end your turn rather than
checking on subagents.

To list your subagents, whether each is working, and its latest reply:
sqlite3 -json "` + s.DBPath + `" "` + subagentListQuery + `"

Subagents do not inherit your conversation. When writing prompts for subagents,
convey intent, nuance, and operational details — not just prescriptive instructions.
Explain how the task serves the user's broader goals, the rationale and constraints,
and what success looks like. Distinguish hard requirements from suggested approaches.
Give subagents enough context and autonomy to make good decisions and adapt as they learn.

Keep short context inline; put substantial context in reusable files, splitting
out shared material. Have subagents read primary sources directly.`
}

// subagentInputSchema builds the JSON schema, including model enum when models are available.
func (s *SubagentTool) subagentInputSchema() string {
	modelProp := ""
	if len(s.AvailableModels) > 0 {
		// Build the enum array
		var enumItems []string
		for _, m := range s.AvailableModels {
			enumItems = append(enumItems, fmt.Sprintf("%q", m.Name))
		}
		modelProp = fmt.Sprintf(`,
    "model": {
      "type": "string",
      "description": "Optional. LLM model for the subagent. Omit to keep an existing subagent's model, or to give a new subagent the parent conversation's model; set only when the user asks for a specific model.",
      "enum": [%s]
    }`, strings.Join(enumItems, ", "))
	}

	// reasoning is always available, regardless of the model catalog.
	var reasoningEnum []string
	for _, l := range subagentReasoningLevels {
		reasoningEnum = append(reasoningEnum, fmt.Sprintf("%q", l))
	}
	reasoningProp := fmt.Sprintf(`,
    "reasoning": {
      "type": "string",
      "description": "Reasoning/thinking effort level for the subagent. If omitted, the subagent inherits the parent conversation's reasoning level.",
      "enum": [%s]
    }`, strings.Join(reasoningEnum, ", "))

	return fmt.Sprintf(`{
  "type": "object",
  "required": ["slug", "prompt"],
  "properties": {
    "slug": {
      "type": "string",
      "description": "A short identifier for this subagent (e.g., 'research-api', 'test-runner')"
    },
    "prompt": {
      "type": "string",
      "description": "The message to send to the subagent. If it is still working, it receives the message during its current turn without being interrupted."
    }%s%s
  }
}`, modelProp, reasoningProp)
}

type subagentInput struct {
	Slug      string `json:"slug"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

// Tool returns an llm.Tool for the subagent functionality.
func (s *SubagentTool) Tool() *llm.Tool {
	return &llm.Tool{
		Name:        subagentName,
		Description: s.description(),
		InputSchema: llm.MustSchema(s.subagentInputSchema()),
		Run:         llm.RunJSON(s.run),
	}
}

func (s *SubagentTool) run(ctx context.Context, req subagentInput) llm.ToolOut {
	// Validate slug
	if req.Slug == "" {
		return llm.ErrorfToolOut("slug is required")
	}
	req.Slug = sanitizeSlug(req.Slug)
	if req.Slug == "" {
		return llm.ErrorfToolOut("slug must contain alphanumeric characters")
	}

	if req.Prompt == "" {
		return llm.ErrorfToolOut("prompt is required")
	}

	unlockSlug := s.lockSlug(req.Slug)
	defer unlockSlug()

	// An omitted model is resolved by the runner: an existing subagent keeps
	// its own model, and a new one inherits the parent's.
	var modelID string
	if req.Model != "" {
		var err error
		if modelID, err = resolveModel(s.AvailableModels, req.Model); err != nil {
			return llm.ErrorToolOut(err)
		}
	}

	// Determine reasoning level: explicit choice > parent's reasoning level.
	reasoning := s.ParentReasoning
	if req.Reasoning != "" {
		if !isValidReasoningLevel(req.Reasoning) {
			return llm.ErrorfToolOut("unknown reasoning level %q; available: %s", req.Reasoning, strings.Join(subagentReasoningLevels, ", "))
		}
		reasoning = req.Reasoning
	}

	// Get or create the subagent conversation
	conversationID, actualSlug, err := s.DB.GetOrCreateSubagentConversation(ctx, req.Slug, s.ParentConversationID, s.WorkingDir.Get())
	if err != nil {
		return llm.ErrorfToolOut("failed to get/create subagent conversation: %w", err)
	}

	ack, err := s.Runner.RunSubagent(ctx, conversationID, req.Prompt, modelID, reasoning)
	if err != nil {
		return llm.ErrorfToolOut("subagent error: %w", err)
	}

	// Include actual slug in response if it differs from requested
	slugNote := ""
	if actualSlug != req.Slug {
		slugNote = fmt.Sprintf(" (Note: slug was changed to '%s' for uniqueness. Use '%s' for future messages to this subagent.)", actualSlug, actualSlug)
	}

	return llm.ToolOut{
		LLMContent: llm.TextContent(fmt.Sprintf("Subagent '%s':%s %s", actualSlug, slugNote, ack)),
		Display: SubagentDisplayData{
			Slug:           actualSlug,
			ConversationID: conversationID,
		},
	}
}

// SubagentDisplayData is the display data sent to the UI for subagent tool results.
type SubagentDisplayData struct {
	Slug           string `json:"slug"`
	ConversationID string `json:"conversation_id"`
}

func sanitizeSlug(slug string) string {
	// Lowercase, keep alphanumeric and hyphens
	var result strings.Builder
	for _, r := range strings.ToLower(slug) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			result.WriteRune(r)
		} else if r == ' ' || r == '_' {
			result.WriteRune('-')
		}
	}
	// Remove consecutive hyphens and trim
	s := result.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}
