package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/skills"
)

// MaxSkillContentSize is the maximum size for SKILL.md (100K chars).
const MaxSkillContentSize = 100_000

// MaxSupportingFileSize is the maximum size for supporting files (1 MiB).
const MaxSupportingFileSize = 1 << 20

// SkillManagerTool allows the LLM agent to autonomously create, edit, patch,
// and delete skills. This is the core of the self-improving skill system.
//
// The agent is encouraged to create skills when:
//   - A complex task succeeded (5+ tool calls)
//   - Errors were overcome
//   - A user-corrected approach worked
//   - A non-trivial workflow was discovered
//
// The agent should patch skills when it uses one and hits issues not covered by it.
type SkillManagerTool struct {
	loader    *skills.SkillsLoader
	workspace string
	mu        sync.Mutex
}

// NewSkillManagerTool creates a new SkillManagerTool.
func NewSkillManagerTool(loader *skills.SkillsLoader, workspace string) *SkillManagerTool {
	return &SkillManagerTool{
		loader:    loader,
		workspace: workspace,
	}
}

func (t *SkillManagerTool) Name() string { return "skill_manage" }

func (t *SkillManagerTool) Description() string {
	return `Create, edit, patch, or delete skills. Skills are reusable procedures saved as SKILL.md files that persist across sessions.

WHEN TO CREATE (do this proactively):
- Complex task succeeded with 5+ tool calls
- You overcame errors worth documenting
- User corrected your approach and it worked
- You discovered a non-trivial workflow
- User asks to "remember how to do X"

WHEN TO PATCH (do this immediately):
- You used a skill and hit issues not covered by it
- Steps were wrong or missing
- Environment changed (new tool versions, different OS)

ACTIONS:
- create: Write a new SKILL.md with frontmatter and instructions
- edit: Full rewrite of an existing SKILL.md
- patch: Targeted find-and-replace edit (for small fixes)
- delete: Remove a skill entirely

FORMAT: Skills use YAML frontmatter (name, description required) followed by markdown instructions with numbered steps, verification commands, and pitfall warnings.`
}

func (t *SkillManagerTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"create", "edit", "patch", "delete"},
				"description": "The action to perform.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Skill name (lowercase, hyphens allowed, max 64 chars). E.g., 'deploy-docker', 'setup-postgres'.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full SKILL.md content for 'create' and 'edit'. Must include YAML frontmatter (---name/description---) and markdown body.",
			},
			"old_text": map[string]any{
				"type":        "string",
				"description": "Text to find for 'patch' action. Must be a unique substring in the current SKILL.md.",
			},
			"new_text": map[string]any{
				"type":        "string",
				"description": "Replacement text for 'patch' action.",
			},
			"category": map[string]any{
				"type":        "string",
				"description": "Optional category subdirectory (e.g., 'devops', 'research'). Skills are stored under {category}/{name}/.",
			},
		},
		"required": []string{"action", "name"},
	}
}

func (t *SkillManagerTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	action, _ := args["action"].(string)
	name, _ := args["name"].(string)
	content, _ := args["content"].(string)
	oldText, _ := args["old_text"].(string)
	newText, _ := args["new_text"].(string)
	category, _ := args["category"].(string)

	// Validate name.
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrorResult("name is required")
	}
	if len(name) > skills.MaxNameLength {
		return ErrorResult(fmt.Sprintf("name exceeds %d characters", skills.MaxNameLength))
	}
	if !skillNameValid(name) {
		return ErrorResult("name must be lowercase alphanumeric with hyphens (e.g., 'my-skill')")
	}

	// Validate category (prevent path traversal).
	category = strings.TrimSpace(category)
	if category != "" && !skillNameValid(category) {
		return ErrorResult("category must be lowercase alphanumeric with hyphens (e.g., 'devops')")
	}

	switch action {
	case "create":
		return t.doCreate(name, content, category)
	case "edit":
		return t.doEdit(name, content)
	case "patch":
		return t.doPatch(name, oldText, newText)
	case "delete":
		return t.doDelete(name)
	default:
		return ErrorResult(fmt.Sprintf("unknown action '%s'. Use: create, edit, patch, delete", action))
	}
}

func (t *SkillManagerTool) doCreate(name, content, category string) *ToolResult {
	if content == "" {
		return ErrorResult("content is required for 'create' action. Include YAML frontmatter and markdown body.")
	}
	if len(content) > MaxSkillContentSize {
		return ErrorResult(fmt.Sprintf("content exceeds %d character limit", MaxSkillContentSize))
	}

	// Validate frontmatter.
	if err := validateSkillContent(content); err != nil {
		return ErrorResult(err.Error())
	}

	// Security scan.
	scanResult := skills.ScanSkillContent(content, "SKILL.md", name, "agent-created")
	allowed, reason := skills.ShouldAllowInstall(scanResult)
	if !allowed {
		return ErrorResult(fmt.Sprintf("Security scan blocked skill creation: %s\n%s",
			reason, skills.FormatScanReport(scanResult)))
	}

	// Determine target directory.
	skillDir := t.resolveSkillDir(name, category)
	skillFile := filepath.Join(skillDir, "SKILL.md")

	// Check if already exists.
	if _, err := os.Stat(skillFile); err == nil {
		return ErrorResult(fmt.Sprintf("skill '%s' already exists at %s. Use 'edit' to modify it.", name, skillDir))
	}

	// Create directory and write file.
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return ErrorResult(fmt.Sprintf("failed to create skill directory: %v", err))
	}

	if err := fileutil.WriteFileAtomic(skillFile, []byte(content), 0o644); err != nil {
		return ErrorResult(fmt.Sprintf("failed to write skill: %v", err))
	}

	result := map[string]any{
		"status":  "created",
		"name":    name,
		"path":    skillFile,
		"message": fmt.Sprintf("Skill '%s' created successfully.", name),
	}
	if len(scanResult.Findings) > 0 {
		result["security_warnings"] = fmt.Sprintf("%d findings (verdict: %s)", len(scanResult.Findings), scanResult.Verdict)
	}
	return t.jsonResult(result)
}

func (t *SkillManagerTool) doEdit(name, content string) *ToolResult {
	if content == "" {
		return ErrorResult("content is required for 'edit' action. Provide the full updated SKILL.md.")
	}
	if len(content) > MaxSkillContentSize {
		return ErrorResult(fmt.Sprintf("content exceeds %d character limit", MaxSkillContentSize))
	}

	if err := validateSkillContent(content); err != nil {
		return ErrorResult(err.Error())
	}

	// Find existing skill.
	skillDir, err := t.findSkillDir(name)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Security scan.
	scanResult := skills.ScanSkillContent(content, "SKILL.md", name, "agent-created")
	allowed, reason := skills.ShouldAllowInstall(scanResult)
	if !allowed {
		return ErrorResult(fmt.Sprintf("Security scan blocked edit: %s\n%s",
			reason, skills.FormatScanReport(scanResult)))
	}

	skillFile := filepath.Join(skillDir, "SKILL.md")
	if err := fileutil.WriteFileAtomic(skillFile, []byte(content), 0o644); err != nil {
		return ErrorResult(fmt.Sprintf("failed to write skill: %v", err))
	}

	result := map[string]any{
		"status":  "edited",
		"name":    name,
		"path":    skillFile,
		"message": fmt.Sprintf("Skill '%s' updated successfully.", name),
	}
	return t.jsonResult(result)
}

func (t *SkillManagerTool) doPatch(name, oldText, newText string) *ToolResult {
	if oldText == "" {
		return ErrorResult("old_text is required for 'patch' action")
	}

	// Find existing skill.
	skillDir, err := t.findSkillDir(name)
	if err != nil {
		return ErrorResult(err.Error())
	}

	skillFile := filepath.Join(skillDir, "SKILL.md")
	data, err := os.ReadFile(skillFile)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to read skill: %v", err))
	}

	current := string(data)

	// Find the old text (exact match only — no case-insensitive fallback
	// to avoid UTF-8 byte-length mismatch issues).
	if !strings.Contains(current, oldText) {
		return ErrorResult(fmt.Sprintf("old_text not found in skill '%s'. Provide an exact substring from the current SKILL.md.", name))
	}

	// Check for multiple matches.
	count := strings.Count(current, oldText)
	if count > 1 {
		return ErrorResult(fmt.Sprintf("old_text matches %d locations. Provide a more specific substring.", count))
	}

	patched := strings.Replace(current, oldText, newText, 1)

	if len(patched) > MaxSkillContentSize {
		return ErrorResult(fmt.Sprintf("patched content would exceed %d character limit", MaxSkillContentSize))
	}

	// Security scan the patched result.
	scanResult := skills.ScanSkillContent(patched, "SKILL.md", name, "agent-created")
	allowed, reason := skills.ShouldAllowInstall(scanResult)
	if !allowed {
		return ErrorResult(fmt.Sprintf("Security scan blocked patch: %s\n%s",
			reason, skills.FormatScanReport(scanResult)))
	}

	if err := fileutil.WriteFileAtomic(skillFile, []byte(patched), 0o644); err != nil {
		return ErrorResult(fmt.Sprintf("failed to write patched skill: %v", err))
	}

	result := map[string]any{
		"status":  "patched",
		"name":    name,
		"path":    skillFile,
		"message": fmt.Sprintf("Skill '%s' patched successfully.", name),
	}
	return t.jsonResult(result)
}

func (t *SkillManagerTool) doDelete(name string) *ToolResult {
	skillDir, err := t.findSkillDir(name)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Safety: only delete from workspace skills dir, not builtin.
	workspaceSkills := filepath.Join(t.workspace, "skills")
	absSkillDir, _ := filepath.Abs(skillDir)
	absWorkspace, _ := filepath.Abs(workspaceSkills)

	if !strings.HasPrefix(absSkillDir, absWorkspace) {
		return ErrorResult(fmt.Sprintf("cannot delete skill '%s': it's a builtin/global skill at %s. Only workspace skills can be deleted.", name, skillDir))
	}

	// Create backup before deletion.
	backupDir := skillDir + ".deleted-" + time.Now().Format("20060102-150405")
	if err := os.Rename(skillDir, backupDir); err != nil {
		return ErrorResult(fmt.Sprintf("failed to delete skill: %v", err))
	}

	result := map[string]any{
		"status":  "deleted",
		"name":    name,
		"backup":  backupDir,
		"message": fmt.Sprintf("Skill '%s' deleted (backup at %s).", name, filepath.Base(backupDir)),
	}
	return t.jsonResult(result)
}

// --- Helpers ---

func (t *SkillManagerTool) resolveSkillDir(name, category string) string {
	base := filepath.Join(t.workspace, "skills")
	if category != "" {
		return filepath.Join(base, category, name)
	}
	return filepath.Join(base, name)
}

func (t *SkillManagerTool) findSkillDir(name string) (string, error) {
	// Search through all skill roots.
	for _, root := range t.loader.SkillRoots() {
		// Direct match.
		dir := filepath.Join(root, name)
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
			return dir, nil
		}
		// Check one level of category subdirectories.
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, e.Name(), name)
			if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("skill '%s' not found in any skill directory", name)
}

func (t *SkillManagerTool) jsonResult(data map[string]any) *ToolResult {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to marshal result: %v", err))
	}
	return NewToolResult(string(jsonData))
}

// skillNameValid checks that a skill name is lowercase alphanumeric with hyphens.
func skillNameValid(name string) bool {
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasSuffix(name, "-")
}

// validateSkillContent checks that content has valid YAML frontmatter with name and description.
func validateSkillContent(content string) error {
	// Must start with ---.
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return fmt.Errorf("SKILL.md must start with YAML frontmatter (---). Example:\n---\nname: my-skill\ndescription: What it does\n---\n\n# Instructions...")
	}

	// Extract frontmatter.
	lines := strings.Split(content, "\n")
	endIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			endIdx = i
			break
		}
	}
	if endIdx == -1 {
		return fmt.Errorf("YAML frontmatter not closed (missing second ---)")
	}

	frontmatter := strings.Join(lines[1:endIdx], "\n")

	// Check for name and description.
	if !strings.Contains(frontmatter, "name:") {
		return fmt.Errorf("frontmatter must include 'name' field")
	}
	if !strings.Contains(frontmatter, "description:") {
		return fmt.Errorf("frontmatter must include 'description' field")
	}

	// Body must be non-empty.
	body := strings.TrimSpace(strings.Join(lines[endIdx+1:], "\n"))
	if body == "" {
		return fmt.Errorf("SKILL.md body cannot be empty — include instructions, steps, and examples")
	}

	return nil
}
