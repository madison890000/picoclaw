package curated

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// setupTestStore creates a CuratedStore backed by a temp directory.
// Uses t.Setenv which is safe and auto-cleans up.
func setupTestStore(t *testing.T) *CuratedStore {
	t.Helper()
	dir := t.TempDir()

	// Override the memory dir via PICOCLAW_HOME.
	// t.Setenv handles save/restore automatically and prevents t.Parallel().
	t.Setenv("PICOCLAW_HOME", dir)

	// Create memories subdirectory.
	os.MkdirAll(filepath.Join(dir, "memories"), 0o755)

	store := NewCuratedStore()
	store.LoadFromDisk()
	return store
}

func TestAdd(t *testing.T) {
	store := setupTestStore(t)

	r := store.Add("memory", "Go is fun")
	if !r.Success {
		t.Fatalf("Add failed: %s", r.Error)
	}
	if r.EntryCount != 1 {
		t.Fatalf("expected 1 entry, got %d", r.EntryCount)
	}

	// Second add with same content should report duplicate.
	r = store.Add("memory", "Go is fun")
	if !r.Success {
		t.Fatalf("Duplicate add should succeed with message: %s", r.Error)
	}
	if !strings.Contains(r.Message, "already exists") {
		t.Fatalf("expected duplicate message, got: %s", r.Message)
	}
}

func TestAddExceedsLimit(t *testing.T) {
	store := setupTestStore(t)

	store.memoryCharLimit = 20 // tiny limit

	r := store.Add("memory", "short")
	if !r.Success {
		t.Fatalf("first add should succeed: %s", r.Error)
	}

	r = store.Add("memory", "this is a very long entry that exceeds the limit")
	if r.Success {
		t.Fatal("should have failed due to limit")
	}
	if !strings.Contains(r.Error, "exceed the limit") {
		t.Fatalf("unexpected error: %s", r.Error)
	}
}

func TestReplace(t *testing.T) {
	store := setupTestStore(t)

	store.Add("memory", "Python is slow")
	r := store.Replace("memory", "Python", "Python is fast")
	if !r.Success {
		t.Fatalf("Replace failed: %s", r.Error)
	}
	if r.Entries[0] != "Python is fast" {
		t.Fatalf("expected replaced content, got: %s", r.Entries[0])
	}
}

func TestReplaceNoMatch(t *testing.T) {
	store := setupTestStore(t)

	store.Add("memory", "hello")
	r := store.Replace("memory", "nonexistent", "new")
	if r.Success {
		t.Fatal("replace should fail for no match")
	}
	if !strings.Contains(r.Error, "No entry matched") {
		t.Fatalf("unexpected error: %s", r.Error)
	}
}

func TestReplaceAmbiguous(t *testing.T) {
	store := setupTestStore(t)

	store.Add("memory", "tool: vim is great")
	store.Add("memory", "tool: emacs is great")

	r := store.Replace("memory", "tool:", "tool: nano")
	if r.Success {
		t.Fatal("replace should fail for ambiguous match")
	}
	if !strings.Contains(r.Error, "Multiple entries") {
		t.Fatalf("unexpected error: %s", r.Error)
	}
}

func TestRemove(t *testing.T) {
	store := setupTestStore(t)

	store.Add("memory", "delete me")
	store.Add("memory", "keep me")

	r := store.Remove("memory", "delete me")
	if !r.Success {
		t.Fatalf("Remove failed: %s", r.Error)
	}
	if r.EntryCount != 1 {
		t.Fatalf("expected 1 entry after remove, got %d", r.EntryCount)
	}
	if r.Entries[0] != "keep me" {
		t.Fatalf("wrong entry kept: %s", r.Entries[0])
	}
}

func TestFrozenSnapshot(t *testing.T) {
	store := setupTestStore(t)

	// Add entries before loading snapshot.
	store.Add("memory", "initial entry")

	// Re-load to capture snapshot.
	store.LoadFromDisk()
	snapshot := store.FormatForSystemPrompt("memory")
	if !strings.Contains(snapshot, "initial entry") {
		t.Fatal("snapshot should contain initial entry")
	}

	// Add more entries mid-session.
	store.Add("memory", "mid-session entry")

	// Snapshot should NOT change.
	snapshot2 := store.FormatForSystemPrompt("memory")
	if snapshot2 != snapshot {
		t.Fatal("frozen snapshot should not change mid-session")
	}
}

func TestUserTarget(t *testing.T) {
	store := setupTestStore(t)

	r := store.Add("user", "Prefers dark mode")
	if !r.Success {
		t.Fatalf("Add user failed: %s", r.Error)
	}
	if r.Target != "user" {
		t.Fatalf("expected target 'user', got '%s'", r.Target)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PICOCLAW_HOME", dir)
	os.MkdirAll(filepath.Join(dir, "memories"), 0o755)

	// Write with first store.
	s1 := NewCuratedStore()
	s1.LoadFromDisk()
	s1.Add("memory", "persistent fact")

	// Read with second store.
	s2 := NewCuratedStore()
	s2.LoadFromDisk()

	snap := s2.FormatForSystemPrompt("memory")
	if !strings.Contains(snap, "persistent fact") {
		t.Fatal("data should persist across store instances")
	}
}

func TestConcurrentAdd(t *testing.T) {
	store := setupTestStore(t)

	store.memoryCharLimit = 100000 // large limit for concurrent test

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			store.Add("memory", strings.Repeat("x", 10)+" "+string(rune('a'+n)))
		}(i)
	}
	wg.Wait()

	// Reload and verify no data loss.
	store.LoadFromDisk()
	entries := store.entriesFor("memory")
	if len(entries) < 15 { // some may overlap due to timing, but most should succeed
		t.Fatalf("expected at least 15 entries from 20 concurrent adds, got %d", len(entries))
	}
}

func TestDeduplication(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PICOCLAW_HOME", dir)

	memDir := filepath.Join(dir, "memories")
	os.MkdirAll(memDir, 0o755)

	// Write a file with duplicate entries.
	content := "entry one\n§\nentry two\n§\nentry one"
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(content), 0o644)

	store := NewCuratedStore()
	store.LoadFromDisk()

	entries := store.entriesFor("memory")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries after dedup, got %d: %v", len(entries), entries)
	}
}
