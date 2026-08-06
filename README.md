# audit-loop

Automated cross-agent review loop. One agent critiques, another addresses findings by editing code directly. Loops until approved.

By default, Claude is the driver — reading files, making changes, and deciding what to fix or reject. Codex is the adversary — it only reviews and critiques. Both slots are independently selectable among `claude`, `codex`, and `opencode` (see [Selecting agents](#selecting-agents)).

Works on code diffs (default) or any file content — design docs, specs, proposals, whatever you point it at.

## Install

```bash
go install github.com/mobiusdickus/audit-loop@latest
```

Or build from source:

```bash
git clone https://github.com/mobiusdickus/audit-loop.git
cd audit-loop
make install
```

By default installs to `~/.local/bin/`. Override with `make install PREFIX=/usr/local/bin`.

## Requirements

- One or more of the `claude`, `codex`, or `opencode` CLIs (only the agents you select must be installed)
- `git` (only in diff mode)

## Usage

```bash
# Code review — diff current branch against main
audit-loop

# Review entire repo
audit-loop --full

# Code review — custom base branch
audit-loop --base develop

# Review a specific file instead of a diff
audit-loop --input docs/architecture.md --theme doc-review

# More rounds
audit-loop --max-rounds 8

# Use a custom theme
audit-loop --theme security
audit-loop --theme ./my-theme/

# Preview without running
audit-loop --dry-run

# Swap roles — codex drives (edits code), claude critiques (read-only)
audit-loop --swap

# Use opencode as the critic
audit-loop --critic opencode

# Use opencode as the driver
audit-loop --driver opencode

# Mixed pairing: opencode critic, claude driver
audit-loop --critic opencode --driver claude

# Per-slot models
audit-loop --critic opencode --critic-model anthropic/claude-sonnet-4-6
```

## Selecting agents

`--critic` and `--driver` independently choose the agent for each slot from `claude`, `codex`, or `opencode`. Defaults reproduce the original behavior exactly: `codex` critiques and `claude` drives.

- `--swap` inverts whichever two agents are selected (so `--critic opencode --swap` makes opencode the driver).
- `codex` can be used in both slots, as can `claude` or `opencode`.
- `opencode` is the only agent with real read-scoping when used as critic — see [Security](#security).

### Models

Each slot accepts its own model flag: `--critic-model` and `--driver-model`. `--model` is kept as an alias for `--driver-model` for backwards compatibility.

- Claude: any model name (e.g. `claude-sonnet-4-6`).
- opencode: the model must use `provider/model` format (e.g. `anthropic/claude-sonnet-4-6`); a bare name is a fatal error.
- codex: ignores model flags.

## Discuss Mode

Two-agent deliberation on a design question. Blind first round prevents sycophancy; steelman requirement in subsequent rounds forces genuine debate.

```bash
# Ask a design question
audit-loop discuss "Should we use a connection pool here?"

# Include code context for both agents
audit-loop discuss --context main.go "Is the error handling sufficient?"

# Multiple files/dirs as context
audit-loop discuss --context "main.go,prompts/" "Should prompts be runtime-configurable?"
```

- Round 1: both agents state positions independently (blind)
- Round 2+: each must steelman the opposing view before responding
- Exit 0 = consensus, Exit 1 = no agreement after max rounds
- Logs to `.audit/reviews/discuss-<timestamp>.md`

The *grounded* debater (read-only repo access) is the selected driver agent; the *blind* debater (text-only) is the selected critic agent. With defaults that's claude grounded / codex blind; `--swap` flips them, and `--critic opencode` puts opencode in the blind slot.

## How it works

1. Captures content (git diff by default, or `--input` file)
2. Sends content to the critic for critique
3. If the critic says NEEDS_CHANGES → sends findings to the driver
4. The driver fixes what it agrees with, rejects what it doesn't (with reasoning)
5. Content re-captured and sent back to the critic (with the driver's prior response)
6. Repeats until APPROVED or max rounds exhausted

No commits are made during the loop. Changes stay unstaged. You decide what to keep.

## Input modes

### Diff mode (default)

Reviews your branch changes. Requires a git repo. Re-captures the diff each round (since the driver may modify files).

```bash
audit-loop                     # diff against main
audit-loop --base develop      # diff against develop
```

### File mode (`--input`)

Reviews any file's contents. No git required. Re-reads the file each round (in case the driver edits it).

```bash
audit-loop --input docs/design.md --theme doc-review
audit-loop --input proposal.txt --theme critique
```

## Themes

A theme is a directory with two files:

```
my-theme/
├── auditor.md      # prompt for the critic role (codex by default, claude with --swap)
└── addresser.md    # prompt for the driver role (claude by default, codex with --swap)
```
That's it. No config files, no special format.

### Resolution order

For `--theme <value>`:
1. Literal path (if it exists as a directory)
2. `~/.config/audit-loop/themes/<name>/`
3. If no `--theme` specified, uses embedded code-review defaults

### Template variables

| Variable | Available in | Content |
|----------|-------------|---------|
| `{{CONTENT}}` | auditor | The input content (diff or file contents) |
| `{{DIFF}}` | auditor | Alias for `{{CONTENT}}` (backwards compat) |
| `{{PRIOR_RESPONSE}}` | auditor | Addresser's previous round output (empty on round 1) |
| `{{FINDINGS}}` | addresser | Auditor's findings (everything after verdict line) |
| `{{BRANCH}}` | both | Current branch name (empty in file mode) |
| `{{BASE}}` | both | Base branch name |
| `{{ROUND}}` | both | Current round number |

### Example themes

#### Security audit

```bash
mkdir -p ~/.config/audit-loop/themes/security
```

`auditor.md`:
```
You are a security auditor. Review the following code changes for vulnerabilities.

Focus on: injection, auth bypass, secrets exposure, SSRF, path traversal, broken access control.
Ignore: style, performance, missing tests.

{{PRIOR_RESPONSE}}

Output format:
- First line: APPROVED or NEEDS_CHANGES
- If NEEDS_CHANGES, list findings with severity and fix suggestions.

{{CONTENT}}
```

`addresser.md`:
```
You are addressing security audit findings. For each finding, either fix the vulnerability or explain why it's a false positive.

Read the relevant files before making changes.

Findings:
{{FINDINGS}}
```

#### Document review

```bash
mkdir -p ~/.config/audit-loop/themes/doc-review
```

`auditor.md`:
```
You are reviewing a technical document for clarity, accuracy, and completeness.

Check for: logical gaps, unsupported claims, ambiguous language, missing context, contradictions.
Ignore: grammar, formatting.

{{PRIOR_RESPONSE}}

First line: APPROVED or NEEDS_CHANGES
If NEEDS_CHANGES, list issues with specific quotes and suggestions.

Document:
{{CONTENT}}
```

`addresser.md`:
```
You are improving a document based on review feedback. Edit the file directly to address valid concerns. Reject feedback that misunderstands the intent.

Findings:
{{FINDINGS}}
```

#### Architecture critique

`auditor.md`:
```
You are a systems architect reviewing a design for scalability, failure modes, and operational complexity.

Focus on: single points of failure, missing error handling, unclear ownership boundaries, over-engineering.

{{PRIOR_RESPONSE}}

First line: APPROVED or NEEDS_CHANGES

Design:
{{CONTENT}}
```

### The auditor contract

The auditor prompt **must** produce output where the first matching line is either `APPROVED` or `NEEDS_CHANGES`. Everything after that line becomes `{{FINDINGS}}` for the addresser.

## Output

- Live progress in terminal
- Full audit log written to `.audit/reviews/<timestamp>.md`
- Exit 0 = approved, Exit 1 = max rounds hit

## Options

| Flag | Default | Description |
|------|---------|-------------|
| `--max-rounds N` | 5 | Max review iterations |
| `--base BRANCH` | main | Base branch to diff against |
| `--input PATH` | — | File to review (uses file mode instead of diff mode) |
| `--theme NAME` | — | Theme name or path (dir with auditor.md + addresser.md) |
| `--critic AGENT` | codex | Critic agent: `claude`, `codex`, or `opencode` |
| `--driver AGENT` | claude | Driver agent: `claude`, `codex`, or `opencode` |
| `--critic-model MODEL` | — | Model for the critic agent (provider/model for opencode) |
| `--driver-model MODEL` | — | Model for the driver agent (provider/model for opencode) |
| `--model MODEL` | — | Alias for `--driver-model` (backwards compatible) |
| `--timeout SECS` | 300 | Timeout per agent call |
| `--log-dir PATH` | .audit/reviews | Log output directory |
| `--dry-run` | — | Preview without running |
| `--full` | — | Review entire repo, not just branch diff |
| `--context PATHS` | — | Comma-separated files/dirs for discuss context |
| `--swap` | false | Swap roles: the selected driver critiques, the selected critic drives |

## Swapping roles

By default the selected driver (claude) edits code and the selected critic (codex) critiques. `--swap` inverts the two selected agents: the driver gets read-only access and critiques, the critic gets write access and edits. Useful for comparing how each model performs in either role, or if one CLI is temporarily unavailable/rate-limited in its default role.

This also inverts `discuss` mode: the grounded debater (reads the repo) is the selected driver agent and the blind debater (text/context only) is the selected critic agent; `--swap` flips which agent gets which prompt and tool access.

## Security

The critic runs read-only; the driver gets write access. Which agent fills which role depends on `--critic`/`--driver` and `--swap`.

| Agent | Critic (read-only) | Driver (write) |
|-------|--------------------|-----------------|
| **claude** | `claude -p --allowedTools Read,Grep,Glob` — no write, no shell, no network. Real tool-level isolation | `claude -p --permission-mode acceptEdits --allowedTools Read,Write,Edit,Grep,Glob` — no shell, no network |
| **codex** | `codex exec --sandbox read-only` in an empty temp dir — cannot write files or reach the network, but **can read anything your OS user can read** (see below) | `codex exec --sandbox workspace-write` — can write within the project workspace, no network |
| **opencode** | `opencode run --pure --auto=false` from an empty temp dir with `OPENCODE_PERMISSION` denying everything except `read`/`glob`/`grep`, including `external_directory` — **real read-scoping** | `opencode run --pure --auto` with `read`/`edit`/`glob`/`grep` allowed and everything else (incl. `external_directory`) denied |

### Known limitation: Codex's sandbox doesn't scope reads

`--sandbox` in Codex CLI restricts *writes* and *network* — it does **not** restrict *what Codex can read*. Wherever Codex runs (critic, driver, or either side of `discuss` mode), it can read any file your OS user account can read: not just this repo, but your home directory, SSH keys, cloud credentials, shell history, other projects — everything. A prompt-injected diff (or a malicious `discuss` question) can instruct Codex to read such a file and quote it back in its findings, which then lands in your terminal and in `.audit/reviews/*.md`. No network access is needed for that leak — the exfiltration channel is the review output itself.

The tool takes one small, partial precaution: when Codex runs as critic, it's started from an empty temp directory (`main.go`, `criticDir`) instead of the repo root. This blocks trivial relative-path reads (`cat ./secrets`) but does **not** block absolute-path reads or filesystem traversal — it is not real isolation, and the tool prints a warning to this effect at startup whenever Codex is a selected agent.

### opencode critic is genuinely read-scoped

When `opencode` is the critic (or the blind `discuss` debater), the read-scoping is real. opencode runs from an empty temp directory *and* its per-invocation `OPENCODE_PERMISSION` config denies `external_directory`, so its `read`/`glob`/`grep` tools cannot touch anything outside that temp dir — a prompt-injected diff cannot read your home directory, keys, or other projects. The permission config uses a `*`-deny catch-all, so unlisted tools (and MCP/custom tools from your opencode config) are denied too. This closes the Codex limitation for the opencode critic path.

Two caveats to know when using opencode:

- **Your opencode config still applies.** opencode honors its own project/global config (`~/.config/opencode/`, project `.opencode/`, `AGENTS.md`): your custom agents, skills, and permissions are in effect. The permission isolation above sets a baseline deny; anything your config explicitly allows (e.g. an `external_directory` allow rule for a specific path) still wins for that path.
- **Run with `--pure` and auto-update disabled.** opencode is always invoked with `--pure` (no external plugins) and `OPENCODE_DISABLE_AUTOUPDATE=1`, so audits never trigger plugin installs or update checks.

### Real isolation for Codex

Real isolation for Codex would mean running it inside a container or OS-level sandbox (Docker/Podman, or a custom `sandbox-exec`/bubblewrap profile) with nothing mounted but the prompt text. This isn't implemented because it requires Docker (or a platform-specific sandboxing tool) to be installed and running, which is a heavier requirement than this tool currently asks for. If you run audit-loop against diffs or `discuss` questions from untrusted sources while Codex is selected, treat this as a live limitation, not a solved problem. The fix is simple: select `opencode` as the critic.

## Environment variables

All flags have env var equivalents: `AUDIT_MAX_ROUNDS`, `AUDIT_BASE`, `AUDIT_INPUT`, `AUDIT_CRITIC`, `AUDIT_DRIVER`, `AUDIT_CRITIC_MODEL`, `AUDIT_DRIVER_MODEL`, `AUDIT_MODEL` (alias for `AUDIT_DRIVER_MODEL`), `AUDIT_TIMEOUT`, `AUDIT_THEME`, `AUDIT_LOG_DIR`, `AUDIT_SWAP`.
