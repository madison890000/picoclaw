// Package memory provides persistent curated memory that survives across sessions.
//
// CuratedStore manages two bounded, file-backed memory stores:
//   - MEMORY.md: agent's personal notes (environment facts, project conventions, tool quirks)
//   - USER.md: user profile (preferences, communication style, habits)
//
// Both are injected into the system prompt as a frozen snapshot at session start.
// Mid-session writes update files on disk immediately (durable) but do NOT change
// the system prompt -- this preserves the prefix cache for the entire session.
//
// Entry delimiter: § (section sign). Entries can be multiline.
// Character limits (not tokens) because char counts are model-independent.
package curated

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/fileutil"
)

const (
	// EntryDelimiter separates entries in memory files.
	EntryDelimiter = "\n§\n"

	// DefaultMemoryCharLimit is the default character budget for MEMORY.md.
	DefaultMemoryCharLimit = 2200

	// DefaultUserCharLimit is the default character budget for USER.md.
	DefaultUserCharLimit = 1375
)

// CuratedStore is a bounded curated memory with file persistence.
//
// It maintains two parallel states:
//   - snapshot: frozen at load time, used for system prompt injection.
//     Never mutated mid-session. Keeps prefix cache stable.
//   - memoryEntries / userEntries: live state, mutated by tool calls,
//     persisted to disk. Tool responses always reflect this live state.
type CuratedStore struct {
	mu sync.Mutex

	memoryEntries []string
	userEntries   []string

	memoryCharLimit int
	userCharLimit   int

	// Frozen snapshot for system prompt -- set once at LoadFromDisk().
	snapshot map[string]string // "memory" / "user" -> rendered block
}

// CuratedStoreOption configures a CuratedStore.
type CuratedStoreOption func(*CuratedStore)

// WithMemoryCharLimit sets the character budget for MEMORY.md.
func WithMemoryCharLimit(limit int) CuratedStoreOption {
	return func(s *CuratedStore) { s.memoryCharLimit = limit }
}

// WithUserCharLimit sets the character budget for USER.md.
func WithUserCharLimit(limit int) CuratedStoreOption {
	return func(s *CuratedStore) { s.userCharLimit = limit }
}

// NewCuratedStore creates a new CuratedStore with the given options.
func NewCuratedStore(opts ...CuratedStoreOption) *CuratedStore {
	s := &CuratedStore{
		memoryCharLimit: DefaultMemoryCharLimit,
		userCharLimit:   DefaultUserCharLimit,
		snapshot:        map[string]string{"memory": "", "user": ""},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// memoryDir returns the profile-scoped memories directory.
func memoryDir() string {
	return filepath.Join(config.GetHome(), "memories")
}

// pathFor returns the file path for a given target.
func pathFor(target string) string {
	if target == "user" {
		return filepath.Join(memoryDir(), "USER.md")
	}
	return filepath.Join(memoryDir(), "MEMORY.md")
}

// LoadFromDisk reads entries from MEMORY.md and USER.md, deduplicates them,
// and captures a frozen snapshot for system prompt injection.
func (s *CuratedStore) LoadFromDisk() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := memoryDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("curated memory: create dir: %w", err)
	}

	s.memoryEntries = deduplicate(readEntries(pathFor("memory")))
	s.userEntries = deduplicate(readEntries(pathFor("user")))

	s.snapshot = map[string]string{
		"memory": s.renderBlock("memory", s.memoryEntries),
		"user":   s.renderBlock("user", s.userEntries),
	}
	return nil
}

// FormatForSystemPrompt returns the frozen snapshot for system prompt injection.
// This returns the state captured at LoadFromDisk() time, NOT the live state.
// Returns empty string if no entries existed at load time.
func (s *CuratedStore) FormatForSystemPrompt(target string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot[target]
}

// CuratedResult is the structured result of a curated memory operation.
type CuratedResult struct {
	Success    bool     `json:"success"`
	Message    string   `json:"message,omitempty"`
	Error      string   `json:"error,omitempty"`
	Target     string   `json:"target,omitempty"`
	Entries    []string `json:"entries,omitempty"`
	EntryCount int      `json:"entry_count,omitempty"`
	Usage      string   `json:"usage,omitempty"`
	Matches    []string `json:"matches,omitempty"` // for ambiguous match errors
}

// Add appends a new entry. Returns error if it would exceed the char limit.
func (s *CuratedStore) Add(target, content string) CuratedResult {
	content = strings.TrimSpace(content)
	if content == "" {
		return CuratedResult{Success: false, Error: "Content cannot be empty."}
	}

	if err := ScanMemoryContent(content); err != nil {
		return CuratedResult{Success: false, Error: err.Error()}
	}

	path := pathFor(target)
	return s.withFileLock(path, func() CuratedResult {
		s.reloadTarget(target)
		entries := s.entriesFor(target)
		limit := s.charLimit(target)

		// Reject exact duplicates.
		for _, e := range entries {
			if e == content {
				return s.successResult(target, "Entry already exists (no duplicate added).")
			}
		}

		// Check capacity.
		newEntries := append(append([]string{}, entries...), content)
		newTotal := len(strings.Join(newEntries, EntryDelimiter))
		if newTotal > limit {
			current := s.charCount(target)
			return CuratedResult{
				Success: false,
				Error: fmt.Sprintf(
					"Memory at %d/%d chars. Adding this entry (%d chars) would exceed the limit. Replace or remove existing entries first.",
					current, limit, len(content),
				),
				Entries: entries,
				Usage:   fmt.Sprintf("%d/%d", current, limit),
			}
		}

		s.setEntries(target, newEntries)
		if err := s.saveToDisk(target); err != nil {
			return CuratedResult{Success: false, Error: err.Error()}
		}
		return s.successResult(target, "Entry added.")
	})
}

// Replace finds the entry containing oldText substring and replaces it with newContent.
func (s *CuratedStore) Replace(target, oldText, newContent string) CuratedResult {
	oldText = strings.TrimSpace(oldText)
	newContent = strings.TrimSpace(newContent)
	if oldText == "" {
		return CuratedResult{Success: false, Error: "old_text cannot be empty."}
	}
	if newContent == "" {
		return CuratedResult{Success: false, Error: "new_content cannot be empty. Use 'remove' to delete entries."}
	}

	if err := ScanMemoryContent(newContent); err != nil {
		return CuratedResult{Success: false, Error: err.Error()}
	}

	path := pathFor(target)
	return s.withFileLock(path, func() CuratedResult {
		s.reloadTarget(target)
		entries := s.entriesFor(target)

		matches := findMatches(entries, oldText)
		if len(matches) == 0 {
			return CuratedResult{Success: false, Error: fmt.Sprintf("No entry matched '%s'.", oldText)}
		}
		if ambiguous, previews := isAmbiguous(entries, matches); ambiguous {
			return CuratedResult{
				Success: false,
				Error:   fmt.Sprintf("Multiple entries matched '%s'. Be more specific.", oldText),
				Matches: previews,
			}
		}

		idx := matches[0]
		limit := s.charLimit(target)

		// Check that replacement doesn't blow the budget.
		test := make([]string, len(entries))
		copy(test, entries)
		test[idx] = newContent
		newTotal := len(strings.Join(test, EntryDelimiter))
		if newTotal > limit {
			return CuratedResult{
				Success: false,
				Error: fmt.Sprintf(
					"Replacement would put memory at %d/%d chars. Shorten the new content or remove other entries first.",
					newTotal, limit,
				),
			}
		}

		entries[idx] = newContent
		s.setEntries(target, entries)
		if err := s.saveToDisk(target); err != nil {
			return CuratedResult{Success: false, Error: err.Error()}
		}
		return s.successResult(target, "Entry replaced.")
	})
}

// Remove deletes the entry containing oldText substring.
func (s *CuratedStore) Remove(target, oldText string) CuratedResult {
	oldText = strings.TrimSpace(oldText)
	if oldText == "" {
		return CuratedResult{Success: false, Error: "old_text cannot be empty."}
	}

	path := pathFor(target)
	return s.withFileLock(path, func() CuratedResult {
		s.reloadTarget(target)
		entries := s.entriesFor(target)

		matches := findMatches(entries, oldText)
		if len(matches) == 0 {
			return CuratedResult{Success: false, Error: fmt.Sprintf("No entry matched '%s'.", oldText)}
		}
		if ambiguous, previews := isAmbiguous(entries, matches); ambiguous {
			return CuratedResult{
				Success: false,
				Error:   fmt.Sprintf("Multiple entries matched '%s'. Be more specific.", oldText),
				Matches: previews,
			}
		}

		idx := matches[0]
		newEntries := append(entries[:idx], entries[idx+1:]...)
		s.setEntries(target, newEntries)
		if err := s.saveToDisk(target); err != nil {
			return CuratedResult{Success: false, Error: err.Error()}
		}
		return s.successResult(target, "Entry removed.")
	})
}

// --- Internal helpers ---

func (s *CuratedStore) entriesFor(target string) []string {
	if target == "user" {
		return s.userEntries
	}
	return s.memoryEntries
}

func (s *CuratedStore) setEntries(target string, entries []string) {
	if target == "user" {
		s.userEntries = entries
	} else {
		s.memoryEntries = entries
	}
}

func (s *CuratedStore) charCount(target string) int {
	entries := s.entriesFor(target)
	if len(entries) == 0 {
		return 0
	}
	return len(strings.Join(entries, EntryDelimiter))
}

func (s *CuratedStore) charLimit(target string) int {
	if target == "user" {
		return s.userCharLimit
	}
	return s.memoryCharLimit
}

func (s *CuratedStore) reloadTarget(target string) {
	fresh := deduplicate(readEntries(pathFor(target)))
	s.setEntries(target, fresh)
}

func (s *CuratedStore) saveToDisk(target string) error {
	dir := memoryDir()
	os.MkdirAll(dir, 0o755) //nolint:errcheck
	entries := s.entriesFor(target)
	content := strings.Join(entries, EntryDelimiter)
	if err := fileutil.WriteFileAtomic(pathFor(target), []byte(content), 0o644); err != nil {
		return fmt.Errorf("curated memory: write %s failed: %w", target, err)
	}
	return nil
}

func (s *CuratedStore) renderBlock(target string, entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	limit := s.charLimit(target)
	content := strings.Join(entries, EntryDelimiter)
	current := len(content)
	pct := current * 100 / limit
	if pct > 100 {
		pct = 100
	}

	var header string
	if target == "user" {
		header = fmt.Sprintf("USER PROFILE (who the user is) [%d%% \u2014 %d/%d chars]", pct, current, limit)
	} else {
		header = fmt.Sprintf("MEMORY (your personal notes) [%d%% \u2014 %d/%d chars]", pct, current, limit)
	}
	separator := strings.Repeat("\u2550", 46)
	return fmt.Sprintf("%s\n%s\n%s\n%s", separator, header, separator, content)
}

func (s *CuratedStore) successResult(target, message string) CuratedResult {
	entries := s.entriesFor(target)
	current := s.charCount(target)
	limit := s.charLimit(target)
	pct := 0
	if limit > 0 {
		pct = current * 100 / limit
		if pct > 100 {
			pct = 100
		}
	}
	return CuratedResult{
		Success:    true,
		Target:     target,
		Message:    message,
		Entries:    entries,
		EntryCount: len(entries),
		Usage:      fmt.Sprintf("%d%% \u2014 %d/%d chars", pct, current, limit),
	}
}

// withFileLock acquires an exclusive file lock on path.lock, runs fn, and releases.
func (s *CuratedStore) withFileLock(path string, fn func() CuratedResult) CuratedResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	lockPath := path + ".lock"
	os.MkdirAll(filepath.Dir(lockPath), 0o755) //nolint:errcheck

	f, err := os.Create(lockPath)
	if err != nil {
		return CuratedResult{Success: false, Error: fmt.Sprintf("lock create failed: %v", err)}
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return CuratedResult{Success: false, Error: fmt.Sprintf("file lock failed: %v", err)}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	return fn()
}

// --- File I/O helpers ---

// readEntries reads a memory file and splits into entries.
// Returns empty slice if file doesn't exist or is empty.
func readEntries(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, EntryDelimiter)
	var entries []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			entries = append(entries, p)
		}
	}
	return entries
}

// deduplicate removes duplicate entries, preserving first occurrence order.
func deduplicate(entries []string) []string {
	seen := make(map[string]struct{}, len(entries))
	var result []string
	for _, e := range entries {
		if _, ok := seen[e]; !ok {
			seen[e] = struct{}{}
			result = append(result, e)
		}
	}
	return result
}

// findMatches returns indices of entries containing oldText as a substring.
func findMatches(entries []string, oldText string) []int {
	var matches []int
	for i, e := range entries {
		if strings.Contains(e, oldText) {
			matches = append(matches, i)
		}
	}
	return matches
}

// isAmbiguous returns true if matches refer to distinct entries.
// If all matches are identical text, it's safe to operate on the first.
func isAmbiguous(entries []string, matches []int) (bool, []string) {
	if len(matches) <= 1 {
		return false, nil
	}
	unique := make(map[string]struct{})
	for _, idx := range matches {
		unique[entries[idx]] = struct{}{}
	}
	if len(unique) <= 1 {
		return false, nil // all identical, safe to operate on first
	}
	var previews []string
	for _, idx := range matches {
		e := entries[idx]
		if len(e) > 80 {
			e = e[:80] + "..."
		}
		previews = append(previews, e)
	}
	return true, previews
}

// MarshalResult serializes a CuratedResult to JSON string.
func MarshalResult(r CuratedResult) string {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":"marshal failed: %s"}`, err)
	}
	return string(data)
}
