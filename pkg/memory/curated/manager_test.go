package curated

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mockProvider is a test MemoryProvider.
type mockProvider struct {
	BaseProvider
	name      string
	available bool
	tools     []ToolSchema
	prompt    string
	prefetch  string
}

func (m *mockProvider) Name() string                                    { return m.name }
func (m *mockProvider) IsAvailable() bool                               { return m.available }
func (m *mockProvider) Initialize(string, map[string]any) error         { return nil }
func (m *mockProvider) SystemPromptBlock() string                       { return m.prompt }
func (m *mockProvider) Prefetch(query string, sessionID string) string  { return m.prefetch }
func (m *mockProvider) GetToolSchemas() []ToolSchema                    { return m.tools }
func (m *mockProvider) HandleToolCall(name string, args map[string]any) (string, error) {
	result := map[string]string{"provider": m.name, "tool": name}
	data, _ := json.Marshal(result)
	return string(data), nil
}

// panicProvider always panics to test recovery.
type panicProvider struct {
	BaseProvider
}

func (p *panicProvider) Name() string                            { return "panicker" }
func (p *panicProvider) IsAvailable() bool                       { return true }
func (p *panicProvider) Initialize(string, map[string]any) error { return nil }
func (p *panicProvider) SystemPromptBlock() string               { panic("boom") }
func (p *panicProvider) Prefetch(string, string) string          { panic("boom") }

func TestManagerAddProvider(t *testing.T) {
	mgr := NewManager()

	builtin := &mockProvider{name: "builtin", available: true}
	ext1 := &mockProvider{name: "ext1", available: true}
	ext2 := &mockProvider{name: "ext2", available: true}

	mgr.AddProvider(builtin)
	mgr.AddProvider(ext1)
	mgr.AddProvider(ext2) // should be rejected

	if len(mgr.Providers()) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(mgr.Providers()))
	}
	if mgr.GetProvider("ext2") != nil {
		t.Fatal("ext2 should have been rejected")
	}
}

func TestManagerBuildSystemPrompt(t *testing.T) {
	mgr := NewManager()
	mgr.AddProvider(&mockProvider{name: "builtin", available: true, prompt: "MEMORY block"})
	mgr.AddProvider(&mockProvider{name: "ext", available: true, prompt: "EXT block"})

	prompt := mgr.BuildSystemPrompt()
	if !strings.Contains(prompt, "MEMORY block") {
		t.Fatal("should contain builtin block")
	}
	if !strings.Contains(prompt, "EXT block") {
		t.Fatal("should contain external block")
	}
}

func TestManagerToolRouting(t *testing.T) {
	mgr := NewManager()
	mgr.AddProvider(&mockProvider{
		name:      "builtin",
		available: true,
	})
	mgr.AddProvider(&mockProvider{
		name:      "ext",
		available: true,
		tools: []ToolSchema{
			{Name: "ext_search", Description: "search"},
		},
	})

	if !mgr.HasTool("ext_search") {
		t.Fatal("should have ext_search tool")
	}
	if mgr.HasTool("nonexistent") {
		t.Fatal("should not have nonexistent tool")
	}

	result, err := mgr.HandleToolCall("ext_search", nil)
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}
	if !strings.Contains(result, "ext") {
		t.Fatalf("should route to ext provider: %s", result)
	}

	_, err = mgr.HandleToolCall("nonexistent", nil)
	if err == nil {
		t.Fatal("should fail for unknown tool")
	}
}

func TestManagerPanicRecovery(t *testing.T) {
	mgr := NewManager()
	mgr.AddProvider(&mockProvider{name: "builtin", available: true, prompt: "ok"})

	// Force add panicker (bypass external check by naming it differently).
	mgr.providers = append(mgr.providers, &panicProvider{})

	// These should NOT panic.
	prompt := mgr.BuildSystemPrompt()
	if !strings.Contains(prompt, "ok") {
		t.Fatal("should still get builtin prompt despite panicker")
	}

	// Prefetch with panicker should not crash.
	result := mgr.PrefetchAll("test", "")
	_ = result // just verify no panic
}

func TestManagerPrefetchAll(t *testing.T) {
	mgr := NewManager()
	mgr.AddProvider(&mockProvider{name: "builtin", available: true, prefetch: ""})
	mgr.AddProvider(&mockProvider{name: "ext", available: true, prefetch: "recalled context"})

	result := mgr.PrefetchAll("query", "")
	if !strings.Contains(result, "recalled context") {
		t.Fatalf("should contain prefetched context: %s", result)
	}
}

func TestManagerToolSchemaDedupe(t *testing.T) {
	mgr := NewManager()
	mgr.AddProvider(&mockProvider{
		name:      "builtin",
		available: true,
		tools:     []ToolSchema{{Name: "memory", Description: "builtin memory"}},
	})
	mgr.AddProvider(&mockProvider{
		name:      "ext",
		available: true,
		tools:     []ToolSchema{{Name: "memory", Description: "ext memory"}},
	})

	schemas := mgr.GetAllToolSchemas()
	count := 0
	for _, s := range schemas {
		if s.Name == "memory" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("tool 'memory' should appear once, got %d", count)
	}
}

func TestManagerShutdownOrder(t *testing.T) {
	var order []string
	mgr := NewManager()

	for _, name := range []string{"a", "b", "c"} {
		n := name
		p := &mockProvider{name: n, available: true}
		p.BaseProvider = BaseProvider{}
		mgr.providers = append(mgr.providers, &shutdownTracker{
			mockProvider: p,
			order:        &order,
		})
	}

	mgr.ShutdownAll()

	if len(order) != 3 {
		t.Fatalf("expected 3 shutdowns, got %d", len(order))
	}
	// Should be reverse order.
	expected := []string{"c", "b", "a"}
	for i, name := range expected {
		if order[i] != name {
			t.Fatalf("expected shutdown order %v, got %v", expected, order)
		}
	}
}

type shutdownTracker struct {
	*mockProvider
	order *[]string
}

func (s *shutdownTracker) Shutdown() {
	*s.order = append(*s.order, s.name)
}

func TestManagerOnMemoryWriteSkipsBuiltin(t *testing.T) {
	var called []string
	mgr := NewManager()

	builtin := &writeTracker{mockProvider: &mockProvider{name: "builtin", available: true}, called: &called}
	ext := &writeTracker{mockProvider: &mockProvider{name: "ext", available: true}, called: &called}

	mgr.providers = append(mgr.providers, builtin, ext)

	mgr.OnMemoryWrite("add", "user", "test")

	if len(called) != 1 || called[0] != "ext" {
		t.Fatalf("OnMemoryWrite should skip builtin, got: %v", called)
	}
}

type writeTracker struct {
	*mockProvider
	called *[]string
}

func (w *writeTracker) OnMemoryWrite(action, target, content string) {
	*w.called = append(*w.called, w.name)
}

func (w *writeTracker) Name() string { return w.mockProvider.name }

func TestManagerGetAllToolNames(t *testing.T) {
	mgr := NewManager()
	mgr.AddProvider(&mockProvider{
		name:      "builtin",
		available: true,
		tools:     []ToolSchema{{Name: "memory"}},
	})
	mgr.AddProvider(&mockProvider{
		name:      "ext",
		available: true,
		tools:     []ToolSchema{{Name: "fact_store"}, {Name: "fact_feedback"}},
	})

	names := mgr.GetAllToolNames()
	for _, expected := range []string{"fact_store", "fact_feedback"} {
		if _, ok := names[expected]; !ok {
			t.Fatalf("missing tool name: %s, got: %v", expected, names)
		}
	}
	_ = fmt.Sprintf("%v", names) // use fmt to avoid import error
}
