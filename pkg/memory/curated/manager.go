package curated

import (
	"fmt"
	"log"
	"strings"
)

// Manager orchestrates the built-in memory provider plus at most ONE
// external plugin memory provider.
//
// The BuiltinMemoryProvider is always registered first and cannot be removed.
// Only ONE external (non-builtin) provider is allowed at a time -- attempting
// to register a second external provider is rejected with a warning.
//
// Usage:
//
//	mgr := memory.NewManager()
//	mgr.AddProvider(builtinProvider)
//	mgr.AddProvider(pluginProvider) // at most one
//	mgr.InitializeAll(sessionID, nil)
//
//	prompt := mgr.BuildSystemPrompt()
//	context := mgr.PrefetchAll(userMessage, "")
//	mgr.SyncAll(userMsg, assistantMsg, "")
type Manager struct {
	providers      []MemoryProvider
	toolToProvider map[string]MemoryProvider
	hasExternal    bool
}

// NewManager creates a new Manager.
func NewManager() *Manager {
	return &Manager{
		toolToProvider: make(map[string]MemoryProvider),
	}
}

// AddProvider registers a memory provider.
// Built-in provider (name "builtin") is always accepted.
// Only one external (non-builtin) provider is allowed.
func (m *Manager) AddProvider(p MemoryProvider) {
	isBuiltin := p.Name() == "builtin"

	if !isBuiltin {
		if m.hasExternal {
			existing := ""
			for _, ep := range m.providers {
				if ep.Name() != "builtin" {
					existing = ep.Name()
					break
				}
			}
			log.Printf("memory: rejected provider '%s' -- external provider '%s' is already registered. Only one external memory provider is allowed at a time.",
				p.Name(), existing)
			return
		}
		m.hasExternal = true
	}

	m.providers = append(m.providers, p)

	// Index tool names -> provider for routing.
	for _, schema := range p.GetToolSchemas() {
		if schema.Name == "" {
			continue
		}
		if _, exists := m.toolToProvider[schema.Name]; exists {
			log.Printf("memory: tool name conflict: '%s' already registered, ignoring from %s",
				schema.Name, p.Name())
			continue
		}
		m.toolToProvider[schema.Name] = p
	}

	log.Printf("memory: provider '%s' registered (%d tools)", p.Name(), len(p.GetToolSchemas()))
}

// Providers returns all registered providers.
func (m *Manager) Providers() []MemoryProvider {
	return m.providers
}

// GetProvider returns a provider by name, or nil if not found.
func (m *Manager) GetProvider(name string) MemoryProvider {
	for _, p := range m.providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// --- System prompt ---

// BuildSystemPrompt collects system prompt blocks from all providers.
func (m *Manager) BuildSystemPrompt() string {
	var blocks []string
	for _, p := range m.providers {
		func() {
			defer recoverLog("system_prompt_block", p.Name())
			if block := p.SystemPromptBlock(); strings.TrimSpace(block) != "" {
				blocks = append(blocks, block)
			}
		}()
	}
	return strings.Join(blocks, "\n\n")
}

// --- Prefetch / recall ---

// PrefetchAll collects prefetch context from all providers.
func (m *Manager) PrefetchAll(query, sessionID string) string {
	var parts []string
	for _, p := range m.providers {
		func() {
			defer recoverLog("prefetch", p.Name())
			if result := p.Prefetch(query, sessionID); strings.TrimSpace(result) != "" {
				parts = append(parts, result)
			}
		}()
	}
	return strings.Join(parts, "\n\n")
}

// QueuePrefetchAll queues background prefetch on all providers.
func (m *Manager) QueuePrefetchAll(query, sessionID string) {
	for _, p := range m.providers {
		func() {
			defer recoverLog("queue_prefetch", p.Name())
			p.QueuePrefetch(query, sessionID)
		}()
	}
}

// --- Sync ---

// SyncAll syncs a completed turn to all providers.
func (m *Manager) SyncAll(userContent, assistantContent, sessionID string) {
	for _, p := range m.providers {
		func() {
			defer recoverLog("sync_turn", p.Name())
			p.SyncTurn(userContent, assistantContent, sessionID)
		}()
	}
}

// --- Tools ---

// GetAllToolSchemas collects tool schemas from all providers (deduplicated).
func (m *Manager) GetAllToolSchemas() []ToolSchema {
	seen := make(map[string]struct{})
	var schemas []ToolSchema
	for _, p := range m.providers {
		func() {
			defer recoverLog("get_tool_schemas", p.Name())
			for _, schema := range p.GetToolSchemas() {
				if schema.Name == "" {
					continue
				}
				if _, ok := seen[schema.Name]; ok {
					continue
				}
				seen[schema.Name] = struct{}{}
				schemas = append(schemas, schema)
			}
		}()
	}
	return schemas
}

// GetAllToolNames returns all tool names across all providers.
func (m *Manager) GetAllToolNames() map[string]struct{} {
	result := make(map[string]struct{}, len(m.toolToProvider))
	for name := range m.toolToProvider {
		result[name] = struct{}{}
	}
	return result
}

// HasTool returns true if any provider handles this tool.
func (m *Manager) HasTool(toolName string) bool {
	_, ok := m.toolToProvider[toolName]
	return ok
}

// HandleToolCall routes a tool call to the correct provider.
func (m *Manager) HandleToolCall(toolName string, args map[string]any) (string, error) {
	p, ok := m.toolToProvider[toolName]
	if !ok {
		return "", fmt.Errorf("no memory provider handles tool '%s'", toolName)
	}
	return p.HandleToolCall(toolName, args)
}

// --- Lifecycle hooks ---

// InitializeAll initializes all providers.
func (m *Manager) InitializeAll(sessionID string, opts map[string]any) {
	for _, p := range m.providers {
		func() {
			defer recoverLog("initialize", p.Name())
			if err := p.Initialize(sessionID, opts); err != nil {
				log.Printf("memory: provider '%s' initialize failed: %v", p.Name(), err)
			}
		}()
	}
}

// OnTurnStart notifies all providers of a new turn.
func (m *Manager) OnTurnStart(turnNumber int, message string, kwargs map[string]any) {
	for _, p := range m.providers {
		func() {
			defer recoverLog("on_turn_start", p.Name())
			p.OnTurnStart(turnNumber, message, kwargs)
		}()
	}
}

// OnSessionEnd notifies all providers of session end.
func (m *Manager) OnSessionEnd(messages []map[string]any) {
	for _, p := range m.providers {
		func() {
			defer recoverLog("on_session_end", p.Name())
			p.OnSessionEnd(messages)
		}()
	}
}

// OnPreCompress notifies all providers before context compression.
// Returns combined text to include in the compression summary prompt.
func (m *Manager) OnPreCompress(messages []map[string]any) string {
	var parts []string
	for _, p := range m.providers {
		func() {
			defer recoverLog("on_pre_compress", p.Name())
			if result := p.OnPreCompress(messages); strings.TrimSpace(result) != "" {
				parts = append(parts, result)
			}
		}()
	}
	return strings.Join(parts, "\n\n")
}

// OnMemoryWrite notifies external providers when the built-in memory writes.
// Skips the builtin provider itself (it's the source of the write).
func (m *Manager) OnMemoryWrite(action, target, content string) {
	for _, p := range m.providers {
		if p.Name() == "builtin" {
			continue
		}
		func() {
			defer recoverLog("on_memory_write", p.Name())
			p.OnMemoryWrite(action, target, content)
		}()
	}
}

// OnDelegation notifies all providers that a subagent completed.
func (m *Manager) OnDelegation(task, result, childSessionID string) {
	for _, p := range m.providers {
		func() {
			defer recoverLog("on_delegation", p.Name())
			p.OnDelegation(task, result, childSessionID)
		}()
	}
}

// ShutdownAll shuts down all providers (reverse order for clean teardown).
func (m *Manager) ShutdownAll() {
	for i := len(m.providers) - 1; i >= 0; i-- {
		p := m.providers[i]
		func() {
			defer recoverLog("shutdown", p.Name())
			p.Shutdown()
		}()
	}
}

// recoverLog catches panics from provider calls and logs them.
func recoverLog(method, providerName string) {
	if r := recover(); r != nil {
		log.Printf("memory: provider '%s' %s panicked: %v", providerName, method, r)
	}
}
