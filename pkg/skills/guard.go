// Package skills provides the security scanner for skill content.
//
// Every agent-created or edited skill passes through this scanner before
// being written to disk. It uses regex-based static analysis to detect
// known-bad patterns (data exfiltration, prompt injection, destructive
// commands, persistence, obfuscation, network abuse).
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Severity levels for findings.
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
)

// Categories for findings.
const (
	CategoryExfiltration = "exfiltration"
	CategoryInjection    = "injection"
	CategoryDestructive  = "destructive"
	CategoryPersistence  = "persistence"
	CategoryNetwork      = "network"
	CategoryObfuscation  = "obfuscation"
)

// Trust levels for skill sources.
const (
	TrustBuiltin      = "builtin"
	TrustTrusted      = "trusted"
	TrustCommunity    = "community"
	TrustAgentCreated = "agent-created"
)

// Verdicts from scanning.
const (
	VerdictSafe      = "safe"
	VerdictCaution   = "caution"
	VerdictDangerous = "dangerous"
)

// Finding represents a single threat detected in a skill.
type Finding struct {
	PatternID   string `json:"pattern_id"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	Match       string `json:"match"`
	Description string `json:"description"`
}

// ScanResult holds the outcome of a skill security scan.
type ScanResult struct {
	SkillName  string    `json:"skill_name"`
	Source     string    `json:"source"`
	TrustLevel string   `json:"trust_level"`
	Verdict    string    `json:"verdict"`
	Findings   []Finding `json:"findings"`
	ScannedAt  string    `json:"scanned_at"`
	Summary    string    `json:"summary"`
}

// installPolicy maps (trust_level, verdict) -> action.
// Actions: "allow", "block", "ask".
var installPolicy = map[string][3]string{
	//                     safe      caution   dangerous
	TrustBuiltin:      {"allow", "allow", "allow"},
	TrustTrusted:      {"allow", "allow", "block"},
	TrustCommunity:    {"allow", "block", "block"},
	TrustAgentCreated: {"allow", "allow", "block"},
}

// ShouldAllowInstall checks the install policy for a scan result.
// Returns (allowed bool, reason string).
func ShouldAllowInstall(result *ScanResult) (bool, string) {
	policy, ok := installPolicy[result.TrustLevel]
	if !ok {
		return false, fmt.Sprintf("unknown trust level: %s", result.TrustLevel)
	}

	var verdictIdx int
	switch result.Verdict {
	case VerdictSafe:
		verdictIdx = 0
	case VerdictCaution:
		verdictIdx = 1
	case VerdictDangerous:
		verdictIdx = 2
	default:
		return false, fmt.Sprintf("unknown verdict: %s", result.Verdict)
	}

	action := policy[verdictIdx]
	switch action {
	case "allow":
		return true, "allowed"
	case "block":
		return false, fmt.Sprintf("blocked: %s source with %s verdict", result.TrustLevel, result.Verdict)
	default:
		return false, fmt.Sprintf("blocked: unknown policy action '%s'", action)
	}
}

// ScanSkillDir scans all files in a skill directory for threat patterns.
func ScanSkillDir(skillDir, skillName, source string) *ScanResult {
	trustLevel := ResolveTrustLevel(source)

	result := &ScanResult{
		SkillName:  skillName,
		Source:     source,
		TrustLevel: trustLevel,
		Verdict:    VerdictSafe,
		ScannedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	// Builtin skills are never scanned.
	if trustLevel == TrustBuiltin {
		result.Summary = "builtin skill, scan skipped"
		return result
	}

	// Walk all files in the skill directory.
	walkErr := filepath.Walk(skillDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		// Only scan text-like files.
		ext := strings.ToLower(filepath.Ext(path))
		if !isScannableExt(ext) {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		relPath, _ := filepath.Rel(skillDir, path)
		scanContent(string(data), relPath, result)
		return nil
	})

	if walkErr != nil {
		// Unreadable directory should not be classified as safe.
		result.Verdict = VerdictCaution
		result.Summary = fmt.Sprintf("scan error: %v", walkErr)
		return result
	}

	// Determine verdict from findings.
	result.Verdict = determineVerdict(result.Findings)
	result.Summary = fmt.Sprintf("%d findings (%s)", len(result.Findings), result.Verdict)
	return result
}

// ScanSkillContent scans a single piece of content (e.g., SKILL.md body).
func ScanSkillContent(content, fileName, skillName, source string) *ScanResult {
	trustLevel := ResolveTrustLevel(source)

	result := &ScanResult{
		SkillName:  skillName,
		Source:     source,
		TrustLevel: trustLevel,
		Verdict:    VerdictSafe,
		ScannedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	if trustLevel == TrustBuiltin {
		result.Summary = "builtin skill, scan skipped"
		return result
	}

	scanContent(content, fileName, result)
	result.Verdict = determineVerdict(result.Findings)
	result.Summary = fmt.Sprintf("%d findings (%s)", len(result.Findings), result.Verdict)
	return result
}

// ResolveTrustLevel maps a source string to a trust level.
func ResolveTrustLevel(source string) string {
	switch source {
	case "builtin":
		return TrustBuiltin
	case "agent", "agent-created":
		return TrustAgentCreated
	case "openai/skills", "anthropics/skills":
		return TrustTrusted
	default:
		return TrustCommunity
	}
}

// FormatScanReport produces a human-readable report of findings.
func FormatScanReport(result *ScanResult) string {
	if len(result.Findings) == 0 {
		return fmt.Sprintf("Skill %q: clean (%s)", result.SkillName, result.Verdict)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Security scan for %q (%s, %s):\n", result.SkillName, result.TrustLevel, result.Verdict))
	for i, f := range result.Findings {
		sb.WriteString(fmt.Sprintf("  %d. [%s/%s] %s in %s:%d\n     match: %s\n",
			i+1, f.Severity, f.Category, f.Description, f.File, f.Line, truncate(f.Match, 80)))
	}
	return sb.String()
}

// --- Internal ---

func scanContent(content, fileName string, result *ScanResult) {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		for _, tp := range threatPatterns {
			if tp.re.MatchString(line) {
				result.Findings = append(result.Findings, Finding{
					PatternID:   tp.id,
					Severity:    tp.severity,
					Category:    tp.category,
					File:        fileName,
					Line:        i + 1,
					Match:       truncate(strings.TrimSpace(line), 120),
					Description: tp.description,
				})
			}
		}
	}
}

func determineVerdict(findings []Finding) string {
	if len(findings) == 0 {
		return VerdictSafe
	}
	highCount := 0
	for _, f := range findings {
		if f.Severity == SeverityCritical {
			return VerdictDangerous
		}
		if f.Severity == SeverityHigh {
			highCount++
		}
	}
	// Multiple high-severity findings escalate to dangerous
	// to prevent accumulation attacks.
	if highCount >= 3 {
		return VerdictDangerous
	}
	return VerdictCaution
}

func isScannableExt(ext string) bool {
	switch ext {
	case ".md", ".txt", ".sh", ".bash", ".zsh", ".py", ".js", ".ts",
		".rb", ".go", ".rs", ".yaml", ".yml", ".toml", ".json", ".cfg",
		".conf", ".ini", ".env", ".dockerfile", ".tf", ".hcl", "":
		return true
	}
	return false
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// --- Threat patterns ---

type threatDef struct {
	re          *regexp.Regexp
	id          string
	severity    string
	category    string
	description string
}

var threatPatterns = func() []threatDef {
	raw := []struct {
		pattern, id, severity, category, description string
	}{
		// == Exfiltration: shell commands leaking secrets ==
		{`(?i)curl\s+[^\n]*\$\{?\w*(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)`, "env_exfil_curl", SeverityCritical, CategoryExfiltration, "curl command interpolating secret env var"},
		{`(?i)wget\s+[^\n]*\$\{?\w*(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)`, "env_exfil_wget", SeverityCritical, CategoryExfiltration, "wget command interpolating secret env var"},
		{`(?i)fetch\s*\([^\n]*\$\{?\w*(KEY|TOKEN|SECRET|PASSWORD|API)`, "env_exfil_fetch", SeverityCritical, CategoryExfiltration, "fetch() interpolating secret env var"},
		{`(?i)cat\s+[^\n]*(\.env|credentials|\.netrc|\.pgpass|\.npmrc|\.pypirc)`, "read_secrets_file", SeverityCritical, CategoryExfiltration, "reads known secrets file"},
		{`(?i)\$HOME/\.ssh|~/\.ssh`, "ssh_dir_access", SeverityHigh, CategoryExfiltration, "references SSH directory"},
		{`(?i)\$HOME/\.aws|~/\.aws`, "aws_dir_access", SeverityHigh, CategoryExfiltration, "references AWS credentials directory"},
		{`(?i)\$HOME/\.picoclaw/\.env|~/\.picoclaw/\.env`, "picoclaw_env", SeverityCritical, CategoryExfiltration, "references picoclaw secrets file"},
		{`(?i)printenv|env\s*\|`, "dump_all_env", SeverityHigh, CategoryExfiltration, "dumps all environment variables"},
		{`(?i)base64[^\n]*env`, "encoded_exfil", SeverityHigh, CategoryExfiltration, "base64 encoding with env access"},
		{`(?i)>\s*/tmp/[^\s]*\s*&&\s*(curl|wget|nc|python)`, "tmp_staging", SeverityCritical, CategoryExfiltration, "writes to /tmp then exfiltrates"},

		// == Prompt injection ==
		{`(?i)ignore\s+(\w+\s+)*(previous|all|above|prior)(\s+\w+)*\s+instructions`, "prompt_injection", SeverityCritical, CategoryInjection, "ignore previous instructions"},
		{`(?i)you\s+are\s+(\w+\s+)*now\s+`, "role_hijack", SeverityHigh, CategoryInjection, "overrides agent role"},
		{`(?i)do\s+not\s+(\w+\s+)*tell\s+(\w+\s+)*the\s+user`, "deception_hide", SeverityCritical, CategoryInjection, "hides information from user"},
		{`(?i)system\s+prompt\s+override`, "sys_prompt_override", SeverityCritical, CategoryInjection, "overrides system prompt"},
		{`(?i)disregard\s+(\w+\s+)*(your|all|any)\s+(\w+\s+)*(instructions|rules|guidelines)`, "disregard_rules", SeverityCritical, CategoryInjection, "disregard rules"},
		{`(?i)act\s+as\s+(if|though)\s+(\w+\s+)*you\s+(\w+\s+)*(have\s+no|don't\s+have)\s+(\w+\s+)*(restrictions|limits|rules)`, "bypass_restrictions", SeverityCritical, CategoryInjection, "bypass restrictions"},
		{`<!--[^>]*(?i)(ignore|override|system|secret|hidden)[^>]*-->`, "html_comment_injection", SeverityHigh, CategoryInjection, "hidden instructions in HTML comments"},

		// == Destructive operations ==
		{`rm\s+-rf\s+/`, "destructive_root_rm", SeverityCritical, CategoryDestructive, "recursive delete from root"},
		{`(?i)rm\s+(-[^\s]*)?r.*\$HOME`, "destructive_home_rm", SeverityCritical, CategoryDestructive, "recursive delete targeting home"},
		{`chmod\s+777`, "insecure_perms", SeverityMedium, CategoryDestructive, "world-writable permissions"},
		{`>\s*/etc/`, "system_overwrite", SeverityCritical, CategoryDestructive, "overwrites system config"},
		{`\bmkfs\b`, "format_filesystem", SeverityCritical, CategoryDestructive, "formats filesystem"},
		{`(?i)\bdd\s+.*if=.*of=/dev/`, "disk_overwrite", SeverityCritical, CategoryDestructive, "raw disk write"},

		// == Persistence ==
		{`\bcrontab\b`, "persistence_cron", SeverityMedium, CategoryPersistence, "modifies cron jobs"},
		{`\.(bashrc|zshrc|profile|bash_profile)\b`, "shell_rc_mod", SeverityMedium, CategoryPersistence, "references shell startup file"},
		{`authorized_keys`, "ssh_backdoor", SeverityCritical, CategoryPersistence, "modifies SSH authorized keys"},
		{`(?i)/etc/sudoers|visudo`, "sudoers_mod", SeverityCritical, CategoryPersistence, "modifies sudoers"},
		{`(?i)launchctl\s+load|LaunchAgents|LaunchDaemons`, "macos_launchd", SeverityMedium, CategoryPersistence, "macOS launch agent persistence"},
		{`(?i)systemd.*\.service|systemctl\s+(enable|start)`, "systemd_service", SeverityMedium, CategoryPersistence, "systemd service persistence"},

		// == Network ==
		{`(?i)\b(nc|ncat|netcat)\s+(-[^\s]*\s+)*-l`, "reverse_shell_nc", SeverityCritical, CategoryNetwork, "netcat listener (reverse shell)"},
		{`(?i)/dev/tcp/`, "bash_tcp", SeverityHigh, CategoryNetwork, "bash /dev/tcp network access"},
		{`(?i)\b(dig|nslookup|host)\s+[^\n]*\$`, "dns_exfil", SeverityCritical, CategoryNetwork, "DNS lookup with variable (DNS exfil)"},

		// == Obfuscation ==
		{`(?i)\beval\s*\(\s*(base64|atob|decode)`, "eval_encoded", SeverityCritical, CategoryObfuscation, "eval of encoded content"},
		{`(?i)\\x[0-9a-f]{2}.*\\x[0-9a-f]{2}.*\\x[0-9a-f]{2}`, "hex_escape_chain", SeverityHigh, CategoryObfuscation, "chained hex escapes"},
	}

	patterns := make([]threatDef, 0, len(raw))
	for _, r := range raw {
		patterns = append(patterns, threatDef{
			re:          regexp.MustCompile(r.pattern),
			id:          r.id,
			severity:    r.severity,
			category:    r.category,
			description: r.description,
		})
	}
	return patterns
}()
