package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func resetFlags(t *testing.T) {
	t.Helper()
	*critic = "codex"
	*driver = "claude"
	*criticMdl = ""
	*driverMdl = ""
	*model = ""
	*swap = false
}

func TestCriticDriverDispatch(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		resetFlags(t)
		if got := criticName(); got != "codex" {
			t.Errorf("criticName() = %q, want codex", got)
		}
		if got := driverName(); got != "claude" {
			t.Errorf("driverName() = %q, want claude", got)
		}
		bin, args, stdin, env := criticArgs("P")
		if bin != "codex" || !reflect.DeepEqual(args, []string{"exec", "--sandbox", "read-only", "-"}) || stdin != "P" || len(env) != 0 {
			t.Errorf("criticArgs default = %q %v %q %v", bin, args, stdin, env)
		}
		bin, args, stdin, env = driverArgs("Q")
		if bin != "claude" {
			t.Errorf("driverArgs default bin = %q, want claude", bin)
		}
		if !reflect.DeepEqual(args, []string{"-p", "--model", "claude-sonnet-4-6", "--permission-mode", "acceptEdits", "--allowedTools", "Read,Write,Edit,Grep,Glob"}) {
			t.Errorf("driverArgs default args = %v", args)
		}
	})

	t.Run("swap", func(t *testing.T) {
		resetFlags(t)
		*swap = true
		if got := criticName(); got != "claude" {
			t.Errorf("criticName() = %q, want claude", got)
		}
		if got := driverName(); got != "codex" {
			t.Errorf("driverName() = %q, want codex", got)
		}
		bin, _, _, _ := criticArgs("P")
		if bin != "claude" {
			t.Errorf("criticArgs bin = %q, want claude", bin)
		}
		bin, _, _, _ = driverArgs("Q")
		if bin != "codex" {
			t.Errorf("driverArgs bin = %q, want codex", bin)
		}
	})

	t.Run("explicit opencode critic", func(t *testing.T) {
		resetFlags(t)
		*critic = "opencode"
		bin, args, _, env := criticArgs("P")
		if bin != "opencode" {
			t.Errorf("bin = %q, want opencode", bin)
		}
		if args[0] != "run" || !containsString(args, "--pure") || !containsString(args, "--log-level") {
			t.Errorf("args = %v, want run --pure --log-level ERROR", args)
		}
		if len(env) == 0 || !hasEnv(env, "OPENCODE_PERMISSION") || !hasEnv(env, "OPENCODE_DISABLE_AUTOUPDATE=1") {
			t.Errorf("env = %v, want OPENCODE_PERMISSION + OPENCODE_DISABLE_AUTOUPDATE", env)
		}
	})

	t.Run("same agent both roles", func(t *testing.T) {
		resetFlags(t)
		*critic = "codex"
		*driver = "codex"
		if err := validateAgents(); err != nil {
			t.Errorf("validateAgents() error = %v, want nil", err)
		}
		bin, _, _, _ := criticArgs("P")
		if bin != "codex" {
			t.Errorf("critic bin = %q", bin)
		}
		bin, _, _, _ = driverArgs("Q")
		if bin != "codex" {
			t.Errorf("driver bin = %q", bin)
		}
	})

	t.Run("unknown agent fails validation", func(t *testing.T) {
		resetFlags(t)
		*critic = "gemini"
		if err := validateAgents(); err == nil || !strings.Contains(err.Error(), "unknown agent") || !strings.Contains(err.Error(), "opencode") {
			t.Errorf("validateAgents() = %v, want unknown-agent error listing valid agents", err)
		}
	})

	t.Run("swap with explicit selections", func(t *testing.T) {
		resetFlags(t)
		*critic = "opencode"
		*swap = true
		if got := criticName(); got != "claude" {
			t.Errorf("criticName() = %q, want claude (the driver default, swapped in)", got)
		}
		if got := driverName(); got != "opencode" {
			t.Errorf("driverName() = %q, want opencode", got)
		}
	})
}

func TestModelResolution(t *testing.T) {
	t.Run("claude driver defaults to claude-sonnet-4-6", func(t *testing.T) {
		resetFlags(t)
		if got := resolvedDriverModel(); got != "claude-sonnet-4-6" {
			t.Errorf("resolvedDriverModel() = %q, want claude-sonnet-4-6", got)
		}
	})

	t.Run("driver-model overrides default", func(t *testing.T) {
		resetFlags(t)
		*driverMdl = "claude-opus-4-6"
		if got := resolvedDriverModel(); got != "claude-opus-4-6" {
			t.Errorf("resolvedDriverModel() = %q", got)
		}
	})

	t.Run("model alias maps to driver model", func(t *testing.T) {
		resetFlags(t)
		*model = "claude-sonnet-4-5"
		if got := resolvedDriverModel(); got != "claude-sonnet-4-5" {
			t.Errorf("resolvedDriverModel() = %q", got)
		}
	})

	t.Run("critic model empty by default", func(t *testing.T) {
		resetFlags(t)
		if got := resolvedCriticModel(); got != "" {
			t.Errorf("resolvedCriticModel() = %q, want empty", got)
		}
	})

	t.Run("opencode driver model needs provider/model", func(t *testing.T) {
		resetFlags(t)
		*driver = "opencode"
		*driverMdl = "gpt-4"
		if err := validateAgents(); err == nil || !strings.Contains(err.Error(), "provider/model") {
			t.Errorf("validateAgents() = %v, want provider/model error", err)
		}
		*driverMdl = "anthropic/claude-sonnet-4-6"
		if err := validateAgents(); err != nil {
			t.Errorf("validateAgents() = %v, want nil", err)
		}
	})

	t.Run("model forwarded to claude only", func(t *testing.T) {
		resetFlags(t)
		*driverMdl = "claude-opus-4-6"
		bin, args, _, _ := driverArgs("P")
		if bin == "claude" && !containsString(args, "claude-opus-4-6") {
			t.Errorf("claude args = %v, want model", args)
		}
		*driver = "codex"
		bin, args, _, _ = driverArgs("P")
		if bin == "codex" && containsString(args, "claude-opus-4-6") {
			t.Errorf("codex args = %v, must not contain model", args)
		}
	})

	t.Run("opencode unset model adds no --model", func(t *testing.T) {
		resetFlags(t)
		*critic = "opencode"
		bin, args, _, _ := criticArgs("P")
		if bin == "opencode" && containsString(args, "--model") {
			t.Errorf("opencode args = %v, must not contain --model when unset", args)
		}
	})
}

func TestOpencodeArgs(t *testing.T) {
	resetFlags(t)

	t.Run("critic read-only args", func(t *testing.T) {
		bin, args, stdin, _ := opencodeArgs("P", false, "", "critic")
		if bin != "opencode" || stdin != "P" {
			t.Errorf("bin=%q stdin=%q", bin, stdin)
		}
		if !reflect.DeepEqual(args, []string{"run", "--pure", "--log-level", "ERROR"}) {
			t.Errorf("args = %v, want run --pure --log-level ERROR", args)
		}
	})

	t.Run("driver write args with model and auto", func(t *testing.T) {
		_, args, _, _ := opencodeArgs("P", true, "anthropic/claude-sonnet-4-6", "driver")
		if !containsString(args, "--auto") || !containsString(args, "--model") || !containsString(args, "anthropic/claude-sonnet-4-6") {
			t.Errorf("args = %v, want --auto and --model anthropic/claude-sonnet-4-6", args)
		}
	})

	t.Run("critic permission denies risky tools", func(t *testing.T) {
		perm := opencodePermissionJSON("critic")
		var m map[string]any
		if err := json.Unmarshal([]byte(perm), &m); err != nil {
			t.Fatalf("bad permission json: %v", err)
		}
		if m["*"] != "deny" {
			t.Errorf("catch-all = %v, want deny", m["*"])
		}
		for _, allowed := range []string{"read", "glob", "grep"} {
			rule, ok := m[allowed].(map[string]any)
			if !ok || rule["*"] != "allow" {
				t.Errorf("critic %s permission = %v, want allow", allowed, m[allowed])
			}
		}
		for _, denied := range []string{"edit", "bash", "webfetch", "websearch", "task", "question", "lsp", "external_directory"} {
			if _, ok := m[denied]; ok {
				t.Errorf("critic %s permission should not be set (caught by * deny)", denied)
			}
		}
	})

	t.Run("driver permission allows edit", func(t *testing.T) {
		perm := opencodePermissionJSON("driver")
		var m map[string]any
		if err := json.Unmarshal([]byte(perm), &m); err != nil {
			t.Fatalf("bad permission json: %v", err)
		}
		if m["edit"].(map[string]any)["*"] != "allow" {
			t.Errorf("driver edit permission = %v, want allow", m["edit"])
		}
	})

	t.Run("blind denies everything", func(t *testing.T) {
		perm := opencodePermissionJSON("blind")
		var m map[string]any
		if err := json.Unmarshal([]byte(perm), &m); err != nil {
			t.Fatalf("bad permission json: %v", err)
		}
		if m["*"] != "deny" {
			t.Errorf("blind catch-all = %v, want deny", m["*"])
		}
		for k := range m {
			if k != "*" {
				t.Errorf("blind permission should have only catch-all, got %s", k)
			}
		}
	})
}

func TestCriticNeedsIsolation(t *testing.T) {
	for _, c := range []struct {
		bin  string
		want bool
	}{
		{"codex", true},
		{"opencode", true},
		{"claude", false},
	} {
		if got := criticNeedsIsolation(c.bin); got != c.want {
			t.Errorf("criticNeedsIsolation(%q) = %v, want %v", c.bin, got, c.want)
		}
	}
}

func TestDiscussArgs(t *testing.T) {
	resetFlags(t)

	t.Run("default grounded claude blind codex", func(t *testing.T) {
		bin, args, _, _ := discussArgs("claude", "grounded", "P")
		if bin != "claude" || !containsString(args, "--allowedTools") {
			t.Errorf("grounded claude args = %v, want Read,Grep,Glob", args)
		}
		bin, args, _, _ = discussArgs("codex", "blind", "P")
		if bin != "codex" || !containsString(args, "read-only") {
			t.Errorf("blind codex args = %v, want read-only sandbox", args)
		}
	})

	t.Run("swap grounded codex blind claude", func(t *testing.T) {
		*swap = true
		bin, args, _, _ := discussArgs("codex", "grounded", "P")
		if bin != "codex" || !containsString(args, "read-only") {
			t.Errorf("grounded codex args = %v", args)
		}
		bin, args, _, _ = discussArgs("claude", "blind", "P")
		if bin != "claude" || containsString(args, "--allowedTools") {
			t.Errorf("blind claude args = %v, must have no tools", args)
		}
	})

	t.Run("opencode blind text-only", func(t *testing.T) {
		bin, _, _, env := discussArgs("opencode", "blind", "P")
		if bin != "opencode" {
			t.Errorf("bin = %q", bin)
		}
		var perm string
		for _, e := range env {
			if strings.HasPrefix(e, "OPENCODE_PERMISSION=") {
				perm = strings.TrimPrefix(e, "OPENCODE_PERMISSION=")
			}
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(perm), &m); err != nil {
			t.Fatalf("bad permission json: %v", err)
		}
		if m["*"] != "deny" {
			t.Errorf("blind opencode catch-all = %v, want deny", m["*"])
		}
	})

	t.Run("opencode grounded read-only", func(t *testing.T) {
		bin, _, _, env := discussArgs("opencode", "grounded", "P")
		if bin != "opencode" {
			t.Errorf("bin = %q", bin)
		}
		var perm string
		for _, e := range env {
			if strings.HasPrefix(e, "OPENCODE_PERMISSION=") {
				perm = strings.TrimPrefix(e, "OPENCODE_PERMISSION=")
			}
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(perm), &m); err != nil {
			t.Fatalf("bad permission json: %v", err)
		}
		if m["read"].(map[string]any)["*"] != "allow" {
			t.Errorf("grounded opencode read permission = %v, want allow", m["read"])
		}
	})
}

func TestRunAgentEnv(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.sh")
	script := "#!/bin/sh\nprintf '%s' \"$OPENCODE_PROBE_ENV\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runAgent(stub, []string{}, "", "", []string{"OPENCODE_PROBE_ENV=hello-env"}, time.Minute)
	if err != nil {
		t.Fatalf("runAgent error: %v", err)
	}
	if out != "hello-env" {
		t.Errorf("runAgent output = %q, want hello-env (env not propagated)", out)
	}
}

func hasEnv(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
