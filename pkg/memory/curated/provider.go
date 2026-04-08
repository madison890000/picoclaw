package curated

import "fmt"

// MemoryProvider is the abstract interface for pluggable memory providers.
//
// Memory providers give the agent persistent recall across sessions. One
// external provider is active at a time alongside the always-on built-in
// memory (MEMORY.md / USER.md). The MemoryManager enforces this limit.
//
// Lifecycle (called by MemoryManager):
//
//	Initialize()          -- connect, create resources, warm up
//	SystemPromptBlock()   -- static text for the system prompt
//	Prefetch(query)       -- background recall before each turn
//	SyncTurn(user, asst)  -- async write after each turn
//	GetToolSchemas()      -- tool schemas to expose to the model
//	HandleToolCall()      -- dispatch a tool call
//	Shutdown()            -- clean exit
type MemoryProvider interface {
	// Name returns a short identifier (e.g. "builtin", "honcho", "holographic").
	Name() string

	// IsAvailable returns true if this provider is configured and ready.
	// Should not make network calls -- just check config and installed deps.
	IsAvailable() bool

	// Initialize sets up the provider for a session.
	// Called once at agent startup.
	Initialize(sessionID string, opts map[string]any) error

	// SystemPromptBlock returns text to include in the system prompt.
	// Return empty string to skip.
	SystemPromptBlock() string

	// Prefetch recalls relevant context for the upcoming turn.
	// Return formatted text to inject, or empty string.
	Prefetch(query string, sessionID string) string

	// QueuePrefetch queues a background recall for the NEXT turn.
	QueuePrefetch(query string, sessionID string)

	// SyncTurn persists a completed turn to the backend.
	// Should be non-blocking.
	SyncTurn(userContent, assistantContent, sessionID string)

	// GetToolSchemas returns tool schemas this provider exposes.
	// Return empty slice if this provider has no tools (context-only).
	GetToolSchemas() []ToolSchema

	// HandleToolCall handles a tool call for one of this provider's tools.
	// Returns a JSON string result.
	HandleToolCall(toolName string, args map[string]any) (string, error)

	// Shutdown performs clean exit -- flush queues, close connections.
	Shutdown()

	// --- Optional lifecycle hooks ---

	// OnTurnStart is called at the start of each turn.
	OnTurnStart(turnNumber int, message string, kwargs map[string]any)

	// OnSessionEnd is called when a session ends.
	OnSessionEnd(messages []map[string]any)

	// OnPreCompress is called before context compression.
	// Return text to include in the compression summary prompt.
	OnPreCompress(messages []map[string]any) string

	// OnMemoryWrite is called when the built-in memory tool writes.
	OnMemoryWrite(action, target, content string)

	// OnDelegation is called when a subagent completes.
	OnDelegation(task, result, childSessionID string)
}

// ToolSchema describes an LLM function-calling tool in OpenAI format.
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// BaseProvider provides default (no-op) implementations for all optional
// MemoryProvider hooks. Embed this in concrete providers to only override
// the methods you need.
type BaseProvider struct{}

func (BaseProvider) QueuePrefetch(string, string)            {}
func (BaseProvider) SyncTurn(string, string, string)         {}
func (BaseProvider) Shutdown()                               {}
func (BaseProvider) OnTurnStart(int, string, map[string]any) {}
func (BaseProvider) OnSessionEnd([]map[string]any)           {}
func (BaseProvider) OnPreCompress([]map[string]any) string   { return "" }
func (BaseProvider) OnMemoryWrite(string, string, string)    {}
func (BaseProvider) OnDelegation(string, string, string)     {}
func (BaseProvider) Prefetch(string, string) string          { return "" }
func (BaseProvider) GetToolSchemas() []ToolSchema            { return nil }
func (BaseProvider) HandleToolCall(string, map[string]any) (string, error) {
	return "", fmt.Errorf("no tool handler")
}

// ensure BaseProvider does NOT satisfy MemoryProvider at compile time
// (it's missing Name/IsAvailable/Initialize/SystemPromptBlock)
