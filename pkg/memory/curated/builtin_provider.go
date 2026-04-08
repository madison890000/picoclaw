package curated

import "context"

// BuiltinMemoryProvider wraps MEMORY.md / USER.md as a MemoryProvider.
//
// Always registered as the first provider. Cannot be disabled or removed.
// The actual storage logic lives in CuratedStore. This provider is a thin
// adapter that delegates to CuratedStore and exposes the memory tool schema.
type BuiltinMemoryProvider struct {
	BaseProvider

	store              *CuratedStore
	memoryEnabled      bool
	userProfileEnabled bool
}

// NewBuiltinProvider creates a BuiltinMemoryProvider.
func NewBuiltinProvider(store *CuratedStore, memoryEnabled, userProfileEnabled bool) *BuiltinMemoryProvider {
	return &BuiltinMemoryProvider{
		store:              store,
		memoryEnabled:      memoryEnabled,
		userProfileEnabled: userProfileEnabled,
	}
}

func (p *BuiltinMemoryProvider) Name() string { return "builtin" }

func (p *BuiltinMemoryProvider) IsAvailable() bool { return true }

func (p *BuiltinMemoryProvider) Initialize(sessionID string, opts map[string]any) error {
	if p.store != nil {
		return p.store.LoadFromDisk()
	}
	return nil
}

func (p *BuiltinMemoryProvider) SystemPromptBlock() string {
	if p.store == nil {
		return ""
	}
	var parts []string
	if p.memoryEnabled {
		if block := p.store.FormatForSystemPrompt("memory"); block != "" {
			parts = append(parts, block)
		}
	}
	if p.userProfileEnabled {
		if block := p.store.FormatForSystemPrompt("user"); block != "" {
			parts = append(parts, block)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		result += "\n\n" + parts[i]
	}
	return result
}

// GetToolSchemas exposes the "memory" tool through the provider interface.
func (p *BuiltinMemoryProvider) GetToolSchemas() []ToolSchema {
	if p.store == nil {
		return nil
	}
	tool := NewCuratedMemoryTool(p.store)
	return []ToolSchema{{
		Name:        tool.Name(),
		Description: tool.Description(),
		Parameters:  tool.Parameters(),
	}}
}

// HandleToolCall dispatches a memory tool call to the CuratedMemoryTool.
func (p *BuiltinMemoryProvider) HandleToolCall(toolName string, args map[string]any) (string, error) {
	if p.store == nil {
		return `{"success":false,"error":"memory store not initialized"}`, nil
	}
	tool := NewCuratedMemoryTool(p.store)
	result := tool.Execute(context.Background(), args)
	return result.ForLLM, nil
}

// Store returns the underlying CuratedStore for legacy code paths.
func (p *BuiltinMemoryProvider) Store() *CuratedStore {
	return p.store
}

// MemoryEnabled returns whether MEMORY.md is enabled.
func (p *BuiltinMemoryProvider) MemoryEnabled() bool {
	return p.memoryEnabled
}

// UserProfileEnabled returns whether USER.md is enabled.
func (p *BuiltinMemoryProvider) UserProfileEnabled() bool {
	return p.userProfileEnabled
}
