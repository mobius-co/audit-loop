package main

import (
	"bytes"
	"context"
	"embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed prompts/*.md
var prompts embed.FS

var (
	maxRounds = flag.Int("max-rounds", 5, "Max review rounds")
	base      = flag.String("base", "main", "Base branch to diff against")
	input     = flag.String("input", "", "File to review (uses file mode instead of diff mode)")
	model     = flag.String("model", "claude-sonnet-4-6", "Claude model for the driver (addresser) role")
	timeout   = flag.Duration("timeout", 5*time.Minute, "Timeout per agent invocation")
	logDir    = flag.String("log-dir", ".audit/reviews", "Directory for review logs")
	theme     = flag.String("theme", "", "Theme name or path (directory with auditor.md + addresser.md)")
	dryRun    = flag.Bool("dry-run", false, "Show what would be reviewed, don't run")
	full      = flag.Bool("full", false, "Review entire repo, not just branch diff")
	ctxPaths  = flag.String("context", "", "Comma-separated file/dir paths for discuss context")
	swap      = flag.Bool("swap", false, "Swap roles: codex drives (edits code), claude critiques (read-only)")
)

func main() {
	// Subcommand dispatch before flag.Parse()
	if len(os.Args) > 1 && os.Args[1] == "discuss" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		flag.Parse()
		loadEnvDefaults()
		runDiscuss()
		return
	}

	flag.Parse()
	loadEnvDefaults()

	if *maxRounds <= 0 {
		fatal("max-rounds must be >= 1, got %d", *maxRounds)
	}
	if *timeout <= 0 {
		fatal("timeout must be positive, got %s", *timeout)
	}

	paths := flag.Args() // positional args are path filters

	fileMode := *input != ""

	if !fileMode {
		if err := preflight(); err != nil {
			fatal(err.Error())
		}
	} else {
		// In file mode, just check codex and claude are available
		if err := preflightTools(); err != nil {
			fatal(err.Error())
		}
		if _, err := os.Stat(*input); err != nil {
			fatal("input file not found: %v", err)
		}
	}

	branch := ""
	if !fileMode {
		branch = gitBranch()
	}

	content, err := captureContent(fileMode, paths)
	if err != nil {
		fatal(err.Error())
	}
	if content == "" {
		if fileMode {
			info("Input file %s is empty", *input)
		} else {
			info("No changes to review (branch %s vs %s)", branch, *base)
		}
		os.Exit(0)
	}

	if *dryRun {
		info("Dry run — would review:")
		if fileMode {
			info("  Input: %s", *input)
		} else {
			info("  Branch: %s", branch)
			info("  Base: %s", *base)
			info("  Stats: %s", diffStats(*base, paths))
		}
		info("  Max rounds: %d", *maxRounds)
		if *theme != "" {
			info("  Theme: %s", *theme)
		}
		info("  Critic: %s / Driver: %s", criticName(), driverName())
		os.Exit(0)
	}

	warnCodexSandboxScope()

	themeDir := resolveThemeDir()
	if themeDir != "" {
		for _, f := range []string{"auditor.md", "addresser.md"} {
			if _, err := os.Stat(filepath.Join(themeDir, f)); err != nil {
				fatal("theme missing required file %s: %v", f, err)
			}
		}
	}

	log := newReviewLog(*logDir, branch, *base, *maxRounds)
	start := time.Now()

	var priorResponse string
	var verdict string
	round := 0

	for round < *maxRounds {
		round++
		info("Round %d/%d — sending content to %s...", round, *maxRounds, criticName())

		content, err = captureContent(fileMode, paths)
		if err != nil {
			errorf("content capture failed (round %d): %v", round, err)
			log.writeRoundAudit(round, criticName(), "ERROR", err.Error())
			verdict = "ERROR"
			break
		}
		if content == "" {
			info("No content remaining — treating as resolved")
			verdict = "APPROVED"
			break
		}
		auditPrompt := buildAuditorPrompt(themeDir, content, priorResponse, branch, round)

		criticBin, criticCmdArgs, stdinData := criticArgs(auditPrompt)
		criticDir := ""
		if criticName() == "codex" {
			// Starting codex outside the repo blocks trivial relative-path
			// reads (`cat ./secrets`) from a prompt-injected diff. It is NOT
			// real isolation: --sandbox read-only restricts writes/network,
			// not read scope, so absolute-path reads still reach anything
			// the OS user account can read. See README's Security section.
			td, tdErr := os.MkdirTemp("", "audit-critic-*")
			if tdErr != nil {
				errorf("could not create temp dir for codex critic (round %d): %v", round, tdErr)
				verdict = "ERROR"
				break
			}
			criticDir = td
		}
		auditOutput, err := runAgent(criticBin, criticCmdArgs, stdinData, criticDir, *timeout)
		if criticDir != "" {
			os.RemoveAll(criticDir)
		}
		if err != nil {
			errorf("%s failed (round %d): %v", criticName(), round, err)
			detail := err.Error()
			if auditOutput != "" {
				detail = auditOutput + "\n\n" + detail
			}
			log.writeRoundAudit(round, criticName(), "ERROR", detail)
			verdict = "ERROR"
			break
		}

		verdict = parseVerdict(auditOutput)
		findings := parseFindings(auditOutput)

		info("%s verdict: %s", criticName(), verdict)
		log.writeRoundAudit(round, criticName(), verdict, findings)

		if verdict == "APPROVED" {
			break
		}
		if verdict == "UNKNOWN" {
			errorf("Could not parse verdict from %s's response", criticName())
			break
		}

		info("Sending findings to %s for resolution...", driverName())
		addresserPrompt := buildAddresserPrompt(themeDir, findings, branch, round)

		driverBin, driverCmdArgs, driverStdin := driverArgs(addresserPrompt)
		driverOutput, err := runAgent(driverBin, driverCmdArgs, driverStdin, "", *timeout)
		if err != nil {
			detail := err.Error()
			if driverOutput != "" {
				detail = driverOutput + "\n\n" + detail
			}
			errorf("%s failed (round %d): %v", driverName(), round, detail)
			log.writeRoundResponse(driverName(), "Agent failed: "+detail)
			break
		}

		responseTable := parseResponseTable(driverOutput)
		priorResponse = responseTable

		info("%s addressed findings", driverName())
		log.writeRoundResponse(driverName(), responseTable)
	}

	elapsed := time.Since(start)
	stats := ""
	if !fileMode {
		stats = diffStats(*base, paths)
	} else {
		stats = *input
	}
	log.finish(round, verdict, elapsed, stats)

	info("Done in %s. Log: %s", elapsed.Round(time.Second), log.path)

	if verdict == "APPROVED" {
		os.Exit(0)
	}
	os.Exit(1)
}

// --- Discuss mode ---

var discussVerdictRe = regexp.MustCompile(`(?m)^(CONSENSUS|DISAGREE)`)

func runDiscuss() {
	question := strings.Join(flag.Args(), " ")
	if question == "" {
		fatal("usage: audit-loop discuss [--context files] \"question\"")
	}

	if *maxRounds < 1 {
		fatal("--max-rounds must be at least 1")
	}

	if err := preflightTools(); err != nil {
		fatal(err.Error())
	}

	fileContext := loadContext(*ctxPaths)

	if *dryRun {
		grounded, blind := "claude", "codex"
		if *swap {
			grounded, blind = "codex", "claude"
		}
		info("Dry run — would discuss:")
		info("  Question: %s", question)
		info("  Context: %s", *ctxPaths)
		info("  Max rounds: %d", *maxRounds)
		info("  Grounded: %s / Blind: %s", grounded, blind)
		os.Exit(0)
	}

	warnCodexSandboxScope()

	dlog := newDiscussLog(*logDir, question, *ctxPaths, *maxRounds)
	start := time.Now()

	var claudePosition, codexPosition string
	var verdict string

	// One debater is grounded (read-only repo access), the other blind
	// (text-only). Default: claude grounded, codex blind. --swap inverts
	// which identity gets which prompt/tool access; codex has no true
	// zero-tool mode, so its args stay a read-only sandbox either way.
	claudeTemplate, codexTemplate := "discuss-grounded.md", "discuss-blind.md"
	claudeDiscussArgs := []string{"-p", "--model", *model, "--allowedTools", "Read,Grep,Glob"}
	if *swap {
		claudeTemplate, codexTemplate = "discuss-blind.md", "discuss-grounded.md"
		claudeDiscussArgs = []string{"-p", "--model", *model}
	}
	codexDiscussArgs := []string{"exec", "--sandbox", "read-only", "-"}

	for round := 1; round <= *maxRounds; round++ {
		if round == 1 {
			// Blind first round — both agents respond independently
			info("Round 1/%d — blind positions...", *maxRounds)

			claudePrompt := buildDiscussPrompt(claudeTemplate, question, fileContext, "", true)
			codexPrompt := buildDiscussPrompt(codexTemplate, question, fileContext, "", true)

			var claudeErr, codexErr error
			claudePosition, claudeErr = runAgent("claude", claudeDiscussArgs, claudePrompt, "", *timeout)
			if claudeErr != nil {
				errorf("Claude failed: %v", claudeErr)
				verdict = "ERROR"
				break
			}

			codexPosition, codexErr = runAgent("codex", codexDiscussArgs, codexPrompt, "", *timeout)
			if codexErr != nil {
				errorf("Codex failed: %v", codexErr)
				verdict = "ERROR"
				break
			}

			dlog.writeRound(round, claudePosition, codexPosition)
			info("  Claude: %s", parseDiscussVerdict(claudePosition))
			info("  Codex: %s", parseDiscussVerdict(codexPosition))
		} else {
			// Debate rounds — each sees the other's prior position
			info("Round %d/%d — debate...", round, *maxRounds)

			claudePrompt := buildDiscussPrompt(claudeTemplate, question, fileContext, codexPosition, false)
			prevClaudePosition := claudePosition
			var claudeErr error
			claudePosition, claudeErr = runAgent("claude", claudeDiscussArgs, claudePrompt, "", *timeout)
			if claudeErr != nil {
				errorf("Claude failed (round %d): %v", round, claudeErr)
				verdict = "ERROR"
				break
			}

			codexPrompt := buildDiscussPrompt(codexTemplate, question, fileContext, prevClaudePosition, false)
			var codexErr error
			codexPosition, codexErr = runAgent("codex", codexDiscussArgs, codexPrompt, "", *timeout)
			if codexErr != nil {
				errorf("Codex failed (round %d): %v", round, codexErr)
				verdict = "ERROR"
				break
			}

			dlog.writeRound(round, claudePosition, codexPosition)
			info("  Claude: %s", parseDiscussVerdict(claudePosition))
			info("  Codex: %s", parseDiscussVerdict(codexPosition))
		}

		// Check if both reached consensus
		cv := parseDiscussVerdict(claudePosition)
		kv := parseDiscussVerdict(codexPosition)
		if cv == "CONSENSUS" && kv == "CONSENSUS" {
			verdict = "CONSENSUS"
			break
		}
		// If either says CONSENSUS in a later round, keep going to confirm the other agrees
		if round == *maxRounds {
			verdict = "SPLIT"
		}
	}

	elapsed := time.Since(start)
	dlog.finish(verdict, elapsed)
	info("Done in %s. Outcome: %s. Log: %s", elapsed.Round(time.Second), verdict, dlog.path)

	if verdict == "CONSENSUS" {
		os.Exit(0)
	}
	os.Exit(1)
}

func buildDiscussPrompt(tmplName, question, fileContext, priorPosition string, blind bool) string {
	tmpl := loadPrompt("", tmplName)

	var ctxBlock string
	if fileContext != "" {
		var indented strings.Builder
		indented.WriteString("## Code Context\n")
		for _, line := range strings.Split(fileContext, "\n") {
			indented.WriteString("    " + line + "\n")
		}
		ctxBlock = indented.String()
	}

	var priorBlock, format string
	if blind {
		format = "## Position\n<your reasoning and conclusion>"
	} else {
		priorBlock = fmt.Sprintf("## Other Agent's Position\n%s\n", priorPosition)
		format = "## Steelman (opposing view)\n<strongest case for their position>\n\n## Position\n<your reasoning and conclusion>"
	}

	return strings.NewReplacer(
		"{{QUESTION}}", question,
		"{{CONTEXT}}", ctxBlock,
		"{{PRIOR_ROUND}}", priorBlock,
		"{{FORMAT}}", format,
	).Replace(tmpl)
}

const (
	maxFileSize    = 100 << 10 // 100 KB per file
	maxContextSize = 512 << 10 // 512 KB total
)

func loadContext(paths string) string {
	if paths == "" {
		return ""
	}
	var parts []string
	var total int

	addFile := func(path string) error {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			errorf("cannot resolve %q: %v", path, err)
			return nil
		}
		if isSensitivePath(real) {
			info("skipping sensitive file: %s", path)
			return nil
		}
		fi, err := os.Stat(real)
		if err != nil {
			errorf("cannot read context %q: %v", path, err)
			return nil
		}
		if fi.Size() > maxFileSize {
			errorf("skipping %q: exceeds %d KB limit", path, maxFileSize>>10)
			return nil
		}
		data, err := os.ReadFile(real)
		if err != nil {
			errorf("cannot read context %q: %v", path, err)
			return nil
		}
		if bytes.ContainsRune(data, 0) {
			errorf("skipping %q: binary file", path)
			return nil
		}
		if total+len(data) > maxContextSize {
			return fmt.Errorf("total context exceeds %d KB limit", maxContextSize>>10)
		}
		total += len(data)
		parts = append(parts, fmt.Sprintf("// %s\n%s", path, string(data)))
		return nil
	}

	for _, p := range strings.Split(paths, ",") {
		p = strings.TrimSpace(p)
		fi, err := os.Stat(p)
		if err != nil {
			errorf("cannot read context %q: %v", p, err)
			continue
		}
		if fi.IsDir() {
			werr := filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if d.Name() == ".git" || d.Name() == "node_modules" {
						return filepath.SkipDir
					}
					return nil
				}
				if isSensitivePath(path) {
					info("skipping sensitive file: %s", path)
					return nil
				}
				if e := addFile(path); e != nil {
					return e
				}
				return nil
			})
			if werr != nil {
				errorf("walk %q: %v", p, werr)
				break
			}
			continue
		}
		if e := addFile(p); e != nil {
			errorf("%v", e)
			break
		}
	}
	return strings.Join(parts, "\n\n")
}

func isSensitivePath(path string) bool {
	name := filepath.Base(path)
	lower := strings.ToLower(name)
	switch lower {
	case ".env", ".env.local", ".env.production", ".env.development",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
		"credentials.json", ".npmrc", ".pypirc",
		"credentials", "kubeconfig", "config.json":
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".jks", ".tfvars", ".token"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	if strings.HasPrefix(lower, ".env.") {
		return true
	}
	// Catch files inside .ssh/ directories
	if strings.Contains(filepath.ToSlash(path), ".ssh/") {
		return true
	}
	return false
}

func parseDiscussVerdict(output string) string {
	m := discussVerdictRe.FindStringSubmatch(output)
	if m == nil {
		return "DISAGREE"
	}
	return m[1]
}

// --- Discuss log writer ---

type discussLog struct {
	path string
	f    *os.File
}

func newDiscussLog(dir, question, ctxPaths string, maxRounds int) *discussLog {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cannot create log dir: %v", err)
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, "discuss-"+ts+".md")

	f, err := os.Create(path)
	if err != nil {
		fatal("cannot create log: %v", err)
	}

	fmt.Fprintf(f, "# Discussion — %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "## Question\n%s\n\n", question)
	if ctxPaths != "" {
		fmt.Fprintf(f, "## Context\n%s\n\n", ctxPaths)
	}
	fmt.Fprintf(f, "## Meta\n- **Max rounds**: %d\n", maxRounds)

	return &discussLog{path: path, f: f}
}

func (l *discussLog) writeRound(round int, claudeOutput, codexOutput string) {
	label := fmt.Sprintf("Round %d", round)
	if round == 1 {
		label = "Round 1 (blind)"
	}
	fmt.Fprintf(l.f, "\n---\n\n## %s\n\n", label)
	fmt.Fprintf(l.f, "### Claude\n%s\n\n", strings.TrimSpace(claudeOutput))
	fmt.Fprintf(l.f, "### Codex\n%s\n", strings.TrimSpace(codexOutput))
}

func (l *discussLog) finish(verdict string, elapsed time.Duration) {
	fmt.Fprintf(l.f, "\n---\n\n## Conclusion\n")
	fmt.Fprintf(l.f, "- **Outcome**: %s\n", verdict)
	fmt.Fprintf(l.f, "- **Duration**: %s\n", elapsed.Round(time.Second))
	switch verdict {
	case "CONSENSUS":
		fmt.Fprintf(l.f, "\n✅ Consensus reached.\n")
	case "ERROR":
		fmt.Fprintf(l.f, "\n⚠️ Agent error — review halted early.\n")
	default:
		fmt.Fprintf(l.f, "\n⚠️ No consensus. See final positions above.\n")
	}
	if err := l.f.Close(); err != nil {
		errorf("log close failed: %v", err)
	}
}

// claudeArgs invokes claude. With write=true it can read/write/edit source
// files (driver role); otherwise it's restricted to read-only tools (critic
// role) — no shell or network access either way.
func claudeArgs(prompt string, write bool) (string, []string, string) {
	if write {
		return "claude", []string{
			"-p", "--model", *model,
			"--permission-mode", "acceptEdits",
			"--allowedTools", "Read,Write,Edit,Grep,Glob",
		}, prompt
	}
	return "claude", []string{"-p", "--model", *model, "--allowedTools", "Read,Grep,Glob"}, prompt
}

// codexArgs invokes codex. With write=true it runs with a writable sandbox
// (driver role); otherwise it's sandboxed read-only (critic role).
func codexArgs(prompt string, write bool) (string, []string, string) {
	sandbox := "read-only"
	if write {
		sandbox = "workspace-write"
	}
	return "codex", []string{"exec", "--sandbox", sandbox, "-"}, prompt
}

// criticArgs/driverArgs resolve which binary fills which role based on
// --swap. Default: codex critiques (read-only), claude drives (edits code).
func criticArgs(prompt string) (string, []string, string) {
	if *swap {
		return claudeArgs(prompt, false)
	}
	return codexArgs(prompt, false)
}

func driverArgs(prompt string) (string, []string, string) {
	if *swap {
		return codexArgs(prompt, true)
	}
	return claudeArgs(prompt, true)
}

func criticName() string {
	if *swap {
		return "claude"
	}
	return "codex"
}

func driverName() string {
	if *swap {
		return "codex"
	}
	return "claude"
}

func runAgent(name string, args []string, stdinData, dir string, t time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if stdinData != "" {
		cmd.Stdin = strings.NewReader(stdinData)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	output := stripANSI(string(out))

	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("timed out after %s", t)
	}
	if err != nil {
		return output, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return output, nil
}

// --- Parser ---

var (
	verdictRe  = regexp.MustCompile(`(?m)^(APPROVED|NEEDS_CHANGES)`)
	ansiRe     = regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)
	tableRowRe = regexp.MustCompile(`(?m)^\|.+\|$`)
)

func parseVerdict(output string) string {
	m := verdictRe.FindString(output)
	if m == "" {
		return "UNKNOWN"
	}
	return m
}

func parseFindings(output string) string {
	idx := verdictRe.FindStringIndex(output)
	if idx == nil {
		return output
	}
	// Everything after the verdict line
	rest := output[idx[1]:]
	return strings.TrimSpace(rest)
}

func parseResponseTable(output string) string {
	lines := strings.Split(output, "\n")
	var table []string
	inTable := false
	for _, line := range lines {
		if strings.Contains(line, "| #") && strings.Contains(line, "Finding") {
			inTable = true
		}
		if inTable {
			if tableRowRe.MatchString(line) || strings.TrimSpace(line) == "" {
				table = append(table, line)
				if strings.TrimSpace(line) == "" {
					break
				}
			} else if len(table) > 0 {
				break
			}
		}
	}
	if len(table) == 0 {
		errorf("could not extract response table; passing full Claude output as prior context")
		return output
	}
	return strings.Join(table, "\n")
}

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// --- Theme / Prompt builder ---

func resolveThemeDir() string {
	if *theme == "" {
		return "" // use embedded defaults
	}
	// If path exists as-is, use it
	if info, err := os.Stat(*theme); err == nil && info.IsDir() {
		return *theme
	}
	// Check ~/.config/audit-loop/themes/<name>/
	home, err := os.UserHomeDir()
	if err == nil {
		themesRoot := filepath.Join(home, ".config", "audit-loop", "themes")
		dir, _ := filepath.Abs(filepath.Join(themesRoot, *theme))
		if !strings.HasPrefix(dir+string(os.PathSeparator), themesRoot+string(os.PathSeparator)) {
			fatal("theme path %q escapes themes directory", *theme)
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			// Resolve symlinks and re-check containment
			realDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				fatal("cannot resolve theme path: %v", err)
			}
			realRoot, err := filepath.EvalSymlinks(themesRoot)
			if err != nil {
				fatal("cannot resolve themes root: %v", err)
			}
			if !strings.HasPrefix(realDir+string(os.PathSeparator), realRoot+string(os.PathSeparator)) {
				fatal("theme path %q escapes themes directory via symlink", *theme)
			}
			return realDir
		}
	}
	fatal("theme %q not found (checked path and ~/.config/audit-loop/themes/)", *theme)
	return ""
}

func loadPrompt(themeDir, name string) string {
	if themeDir != "" {
		path := filepath.Join(themeDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			fatal("theme missing %s: %v", path, err)
		}
		return string(data)
	}
	data, err := prompts.ReadFile("prompts/" + name)
	if err != nil {
		fatal("missing embedded prompt: %s", name)
	}
	return string(data)
}

func buildAuditorPrompt(themeDir, diff, priorResponse string, branch string, round int) string {
	tmpl := loadPrompt(themeDir, "auditor.md")

	var priorCtx string
	if priorResponse != "" {
		priorCtx = fmt.Sprintf(`## Prior Round Context

The implementer addressed your previous findings. Here is their response:

%s

If they rejected a finding with valid reasoning, do not re-flag it.
If their reasoning is wrong, escalate with stronger justification.`, priorResponse)
	}

	grounding := extractEnvironmentFacts()

	r := strings.NewReplacer(
		"{{CONTENT}}", diff,
		"{{DIFF}}", diff,
		"{{PRIOR_RESPONSE}}", priorCtx,
		"{{GROUNDING}}", grounding,
		"{{BRANCH}}", branch,
		"{{BASE}}", *base,
		"{{ROUND}}", fmt.Sprintf("%d", round),
	)
	return r.Replace(tmpl)
}

func buildAddresserPrompt(themeDir, findings string, branch string, round int) string {
	tmpl := loadPrompt(themeDir, "addresser.md")
	r := strings.NewReplacer(
		"{{FINDINGS}}", findings,
		"{{BRANCH}}", branch,
		"{{BASE}}", *base,
		"{{ROUND}}", fmt.Sprintf("%d", round),
	)
	return r.Replace(tmpl)
}

// --- Log writer ---

type reviewLog struct {
	path string
	f    *os.File
}

func newReviewLog(dir, branch, base string, maxRounds int) *reviewLog {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cannot create log dir: %v", err)
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, ts+".md")

	f, err := os.Create(path)
	if err != nil {
		fatal("cannot create log: %v", err)
	}

	fmt.Fprintf(f, "# Audit Review — %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "## Meta\n")
	fmt.Fprintf(f, "- **Branch**: %s\n", branch)
	fmt.Fprintf(f, "- **Base**: %s\n", base)
	fmt.Fprintf(f, "- **Max rounds**: %d\n", maxRounds)

	return &reviewLog{path: path, f: f}
}

func (l *reviewLog) writeRoundAudit(round int, agent, verdict, findings string) {
	fmt.Fprintf(l.f, "\n---\n\n## Round %d\n\n", round)
	fmt.Fprintf(l.f, "### Audit (%s)\n**Verdict**: %s\n\n%s\n", agent, verdict, findings)
}

func (l *reviewLog) writeRoundResponse(agent, response string) {
	fmt.Fprintf(l.f, "\n### Response (%s)\n%s\n", agent, response)
}

func (l *reviewLog) finish(rounds int, verdict string, elapsed time.Duration, stats string) {
	fmt.Fprintf(l.f, "\n---\n\n## Meta (final)\n")
	fmt.Fprintf(l.f, "- **Rounds**: %d/%d\n", rounds, *maxRounds)
	fmt.Fprintf(l.f, "- **Verdict**: %s\n", verdict)
	fmt.Fprintf(l.f, "- **Duration**: %s\n", elapsed.Round(time.Second))
	fmt.Fprintf(l.f, "- **Diff stats**: %s\n", stats)
	fmt.Fprintf(l.f, "\n---\n\n## Result\n")

	if verdict == "APPROVED" {
		fmt.Fprintf(l.f, "✅ Approved after %d round(s).\n", rounds)
	} else {
		fmt.Fprintf(l.f, "⚠️ Max rounds exhausted. Unresolved findings remain.\n")
	}
	if err := l.f.Close(); err != nil {
		errorf("log close failed: %v", err)
	}
}

// --- Content capture ---

func captureContent(fileMode bool, paths []string) (string, error) {
	if fileMode {
		data, err := os.ReadFile(*input)
		if err != nil {
			return "", fmt.Errorf("cannot read input file: %v", err)
		}
		return string(data), nil
	}
	return captureDiff(*base, paths)
}

// --- Git helpers ---

func captureDiff(base string, paths []string) (string, error) {
	if *full {
		const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
		committed, err := gitCmd(append([]string{"diff", emptyTree, "HEAD", "--"}, paths...)...)
		if err != nil {
			return "", fmt.Errorf("failed to capture committed diff: %v", err)
		}
		working, err := gitCmd(append([]string{"diff", "HEAD", "--"}, paths...)...)
		if err != nil {
			return "", fmt.Errorf("failed to capture working diff: %v", err)
		}
		return committed + working, nil
	}
	committed, err := gitCmd(append([]string{"diff", base + "..HEAD", "--"}, paths...)...)
	if err != nil {
		return "", fmt.Errorf("failed to capture committed diff: %v", err)
	}
	working, err := gitCmd(append([]string{"diff", "HEAD", "--"}, paths...)...)
	if err != nil {
		return "", fmt.Errorf("failed to capture working diff: %v", err)
	}
	return committed + working, nil
}

func diffStats(base string, paths []string) string {
	baseRef := base + "..HEAD"
	if *full {
		baseRef = "4b825dc642cb6eb9a060e54bf8d69288fbee4904..HEAD"
	}
	var parts []string
	for _, baseArgs := range [][]string{
		{"diff", "--stat", baseRef, "--"},
		{"diff", "--stat", "HEAD", "--"},
	} {
		args := append(baseArgs, paths...)
		out, err := gitCmd(args...)
		if err != nil {
			continue
		}
		out = strings.TrimSpace(out)
		if out != "" {
			lines := strings.Split(out, "\n")
			parts = append(parts, lines[len(lines)-1])
		}
	}
	return strings.Join(parts, " | ")
}

func gitBranch() string {
	out, err := gitCmd("branch", "--show-current")
	if err != nil {
		fatal("failed to get current branch: %v", err)
	}
	return strings.TrimSpace(out)
}

func gitCmd(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// --- Environment grounding ---

// manifestFiles are project manifests that declare language/dependency versions.
var manifestFiles = []string{
	"go.mod",
	"go.sum",
	"package.json",
	"Cargo.toml",
	"pyproject.toml",
	"requirements.txt",
	"pom.xml",
	"build.gradle",
	"Gemfile",
}

// extractEnvironmentFacts reads manifest files from the project root and returns
// an authoritative context block for the auditor. This prevents false positives
// caused by stale training data (e.g., flagging a valid Go version as non-existent).
func extractEnvironmentFacts() string {
	var sections []string
	for _, name := range manifestFiles {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		content := string(data)
		// For large files (go.sum, lock files), truncate to keep prompt reasonable
		if len(content) > 4096 {
			content = content[:4096] + "\n... (truncated)\n"
		}
		sections = append(sections, fmt.Sprintf("// %s\n%s", name, content))
	}
	if len(sections) == 0 {
		return ""
	}
	return "## Environment (authoritative — do not contradict)\nThese are runtime-extracted project facts. If your training data conflicts with them, your training data is wrong.\n\n" +
		strings.Join(sections, "\n\n")
}

// --- Env/config helpers ---

func loadEnvDefaults() {
	if v := os.Getenv("AUDIT_MAX_ROUNDS"); v != "" {
		fmt.Sscanf(v, "%d", maxRounds)
	}
	if v := os.Getenv("AUDIT_BASE"); v != "" {
		*base = v
	}
	if v := os.Getenv("AUDIT_INPUT"); v != "" {
		*input = v
	}
	if v := os.Getenv("AUDIT_MODEL"); v != "" {
		*model = v
	}
	if v := os.Getenv("AUDIT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*timeout = d
		}
	}
	if v := os.Getenv("AUDIT_THEME"); v != "" {
		*theme = v
	}
	if v := os.Getenv("AUDIT_LOG_DIR"); v != "" {
		*logDir = v
	}
	if v := os.Getenv("AUDIT_SWAP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*swap = b
		}
	}
}

func preflightTools() error {
	for _, cmd := range []string{"codex", "claude"} {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("%s not found in PATH", cmd)
		}
	}
	return nil
}

// warnCodexSandboxScope flags a real, unresolved limitation: codex's
// --sandbox flag restricts writes and network, not read scope. Codex always
// participates in both the review loop and discuss mode, so it can read any
// file your OS user account can read — not just this repo. See README's
// Security section for why this isn't containerized away.
func warnCodexSandboxScope() {
	warn("codex's sandbox restricts writes/network only — it can still read any file your OS user account can read, not just this repo. See README Security section.")
}

func preflight() error {
	if err := preflightTools(); err != nil {
		return err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found in PATH")
	}
	out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("not inside a git repository")
	}
	if !*full {
		if _, err := exec.Command("git", "rev-parse", "--verify", *base).Output(); err != nil {
			return fmt.Errorf("base ref %q not found", *base)
		}
	}
	return nil
}

// --- Output helpers ---

func info(format string, args ...any) {
	fmt.Printf("\033[36m[audit-loop]\033[0m "+format+"\n", args...)
}

func errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\033[31m[audit-loop]\033[0m "+format+"\n", args...)
}

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\033[33m[audit-loop]\033[0m "+format+"\n", args...)
}

func fatal(format string, args ...any) {
	errorf(format, args...)
	os.Exit(1)
}
