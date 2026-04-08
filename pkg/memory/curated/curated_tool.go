package curated

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sipeed/picoclaw/pkg/tools"
)

// CuratedMemoryTool is the LLM-facing tool for reading/writing curated memory.
// It dispatches to a CuratedStore for the actual operations.
type CuratedMemoryTool struct {
	store *CuratedStore
}

// NewCuratedMemoryTool creates a new memory tool backed by the given store.
func NewCuratedMemoryTool(store *CuratedStore) *CuratedMemoryTool {
	return &CuratedMemoryTool{store: store}
}

func (t *CuratedMemoryTool) Name() string { return "memory" }

func (t *CuratedMemoryTool) Description() string {
	return `Save durable information to persistent memory that survives across sessions. ` +
		`Memory is injected into future turns, so keep it compact and focused on facts ` +
		`that will still matter later.

WHEN TO SAVE (do this proactively, don't wait to be asked):
- User corrects you or says 'remember this' / 'don't do that again'
- User shares a preference, habit, or personal detail (name, role, timezone, coding style)
- You discover something about the environment (OS, installed tools, project structure)
- You learn a convention, API quirk, or workflow specific to this user's setup
- You identify a stable fact that will be useful again in future sessions

PRIORITY: User preferences and corrections > environment facts > procedural knowledge. ` +
		`The most valuable memory prevents the user from having to repeat themselves.

Do NOT save task progress, session outcomes, completed-work logs, or temporary TODO ` +
		`state to memory; use session_search to recall those from past transcripts.

TWO TARGETS:
- 'user': who the user is -- name, role, preferences, communication style, pet peeves
- 'memory': your notes -- environment facts, project conventions, tool quirks, lessons learned

ACTIONS: add (new entry), replace (update existing -- old_text identifies it), ` +
		`remove (delete -- old_text identifies it).

SKIP: trivial/obvious info, things easily re-discovered, raw data dumps, and temporary task state.`
}

func (t *CuratedMemoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "replace", "remove"},
				"description": "The action to perform.",
			},
			"target": map[string]any{
				"type":        "string",
				"enum":        []string{"memory", "user"},
				"description": "Which memory store: 'memory' for personal notes, 'user' for user profile.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The entry content. Required for 'add' and 'replace'.",
			},
			"old_text": map[string]any{
				"type":        "string",
				"description": "Short unique substring identifying the entry to replace or remove.",
			},
		},
		"required": []string{"action", "target"},
	}
}

func (t *CuratedMemoryTool) Execute(_ context.Context, args map[string]any) *tools.ToolResult {
	if t.store == nil {
		return tools.ErrorResult("Memory is not available. It may be disabled in config or this environment.")
	}

	action, _ := args["action"].(string)
	target, _ := args["target"].(string)
	content, _ := args["content"].(string)
	oldText, _ := args["old_text"].(string)

	if target != "memory" && target != "user" {
		return tools.ErrorResult(fmt.Sprintf("Invalid target '%s'. Use 'memory' or 'user'.", target))
	}

	var result CuratedResult

	switch action {
	case "add":
		if content == "" {
			return tools.ErrorResult("Content is required for 'add' action.")
		}
		result = t.store.Add(target, content)
	case "replace":
		if oldText == "" {
			return tools.ErrorResult("old_text is required for 'replace' action.")
		}
		if content == "" {
			return tools.ErrorResult("content is required for 'replace' action.")
		}
		result = t.store.Replace(target, oldText, content)
	case "remove":
		if oldText == "" {
			return tools.ErrorResult("old_text is required for 'remove' action.")
		}
		result = t.store.Remove(target, oldText)
	default:
		return tools.ErrorResult(fmt.Sprintf("Unknown action '%s'. Use: add, replace, remove", action))
	}

	data, err := json.Marshal(result)
	if err != nil {
		return tools.ErrorResult(fmt.Sprintf("Failed to marshal result: %v", err))
	}

	tr := tools.NewToolResult(string(data))
	if !result.Success {
		tr.IsError = true
	}
	return tr
}
