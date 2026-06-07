package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{
			name: "shorter than max",
			in:   "hello",
			max:  10,
			want: "hello",
		},
		{
			name: "equal to max",
			in:   "hello",
			max:  5,
			want: "hello",
		},
		{
			name: "longer ascii",
			in:   "hello world",
			max:  5,
			want: "hello\n…(truncated, 11 bytes total)",
		},
		{
			name: "utf-8 safe cut rocket emoji 1",
			in:   "hello 🚀", // rocket is 4 bytes: \xf0\x9f\x9a\x80
			max:  9,         // cut is in the middle of rocket (index 6, 7, 8, 9)
			want: "hello \n…(truncated, 10 bytes total)",
		},
		{
			name: "utf-8 safe cut rocket emoji 2",
			in:   "hello 🚀",
			max:  8,
			want: "hello \n…(truncated, 10 bytes total)",
		},
		{
			name: "utf-8 safe cut rocket emoji 3",
			in:   "hello 🚀",
			max:  7,
			want: "hello \n…(truncated, 10 bytes total)",
		},
		{
			name: "utf-8 safe cut rocket emoji 4",
			in:   "hello 🚀",
			max:  6,
			want: "hello \n…(truncated, 10 bytes total)",
		},
		{
			name: "utf-8 safe cut rocket emoji 5",
			in:   "hello 🚀",
			max:  5,
			want: "hello\n…(truncated, 10 bytes total)",
		},
		{
			name: "empty string",
			in:   "",
			max:  5,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.in, tt.max)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q; want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

func TestBuildClaudeArgs(t *testing.T) {
	tests := []struct {
		name string
		in   runOpts
		want []string
	}{
		{
			name: "default reason-only: task is value right after --print, no other flags",
			in:   runOpts{task: "do the thing", timeoutSeconds: 300},
			want: []string{"--print", "do the thing"},
		},
		{
			name: "allow_tools adds --dangerously-skip-permissions after the task",
			in:   runOpts{task: "edit files", timeoutSeconds: 300, allowTools: true},
			want: []string{"--print", "edit files", "--dangerously-skip-permissions"},
		},
		{
			name: "model adds --model only when non-empty",
			in:   runOpts{task: "ask", timeoutSeconds: 300, model: "opus"},
			want: []string{"--print", "ask", "--model", "opus"},
		},
		{
			name: "empty/whitespace model is dropped",
			in:   runOpts{task: "ask", timeoutSeconds: 300, model: "   "},
			want: []string{"--print", "ask"},
		},
		{
			name: "add_dirs becomes repeated --add-dir entries",
			in:   runOpts{task: "ctx", timeoutSeconds: 300, addDirs: []string{"/a", "/b"}},
			want: []string{"--print", "ctx", "--add-dir", "/a", "--add-dir", "/b"},
		},
		{
			name: "sandbox is ignored for claude (no --sandbox ever)",
			in:   runOpts{task: "compute", timeoutSeconds: 300, sandbox: true},
			want: []string{"--print", "compute"},
		},
		{
			name: "no --print-timeout even with a timeout set",
			in:   runOpts{task: "wait", timeoutSeconds: 600},
			want: []string{"--print", "wait"},
		},
		{
			name: "all options combined, correct ordering, no --sandbox / no --print-timeout",
			in: runOpts{
				task:           "full task",
				timeoutSeconds: 900,
				allowTools:     true,
				sandbox:        true, // must be ignored
				model:          "sonnet",
				addDirs:        []string{"/x"},
			},
			want: []string{
				"--print", "full task",
				"--model", "sonnet",
				"--add-dir", "/x",
				"--dangerously-skip-permissions",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildClaudeArgs(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildClaudeArgs(%+v) = %#v; want %#v", tt.in, got, tt.want)
			}
			// claude must NEVER emit --sandbox or --print-timeout.
			for _, a := range got {
				if a == "--sandbox" {
					t.Errorf("buildClaudeArgs emitted --sandbox: %#v", got)
				}
				if a == "--print-timeout" {
					t.Errorf("buildClaudeArgs emitted --print-timeout: %#v", got)
				}
			}
			// --print must be first and the task its immediate value.
			if len(got) < 2 || got[0] != "--print" || got[1] != tt.in.task {
				t.Errorf("buildClaudeArgs must start with --print <task>; got %#v", got)
			}
		})
	}
}

func TestBuildGeminiArgs(t *testing.T) {
	tests := []struct {
		name string
		in   runOpts
		want []string
	}{
		{
			name: "default: task right after --print, then --print-timeout from timeoutSeconds",
			in:   runOpts{task: "do the thing", timeoutSeconds: 300},
			want: []string{"--print", "do the thing", "--print-timeout", "300s"},
		},
		{
			name: "custom timeout reflected in --print-timeout",
			in:   runOpts{task: "slow", timeoutSeconds: 600},
			want: []string{"--print", "slow", "--print-timeout", "600s"},
		},
		{
			name: "model adds --model only when non-empty",
			in:   runOpts{task: "ask", timeoutSeconds: 300, model: "gemini-2.0"},
			want: []string{"--print", "ask", "--print-timeout", "300s", "--model", "gemini-2.0"},
		},
		{
			name: "add_dirs becomes repeated --add-dir entries",
			in:   runOpts{task: "ctx", timeoutSeconds: 300, addDirs: []string{"/a", "/b"}},
			want: []string{"--print", "ctx", "--print-timeout", "300s", "--add-dir", "/a", "--add-dir", "/b"},
		},
		{
			name: "allow_tools adds --dangerously-skip-permissions",
			in:   runOpts{task: "edit", timeoutSeconds: 300, allowTools: true},
			want: []string{"--print", "edit", "--print-timeout", "300s", "--dangerously-skip-permissions"},
		},
		{
			name: "sandbox adds --sandbox",
			in:   runOpts{task: "compute", timeoutSeconds: 300, sandbox: true},
			want: []string{"--print", "compute", "--print-timeout", "300s", "--sandbox"},
		},
		{
			name: "all options combined, correct ordering",
			in: runOpts{
				task:           "full task",
				timeoutSeconds: 900,
				allowTools:     true,
				sandbox:        true,
				model:          "gemini-pro",
				addDirs:        []string{"/x"},
			},
			want: []string{
				"--print", "full task",
				"--print-timeout", "900s",
				"--model", "gemini-pro",
				"--add-dir", "/x",
				"--dangerously-skip-permissions",
				"--sandbox",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildGeminiArgs(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildGeminiArgs(%+v) = %#v; want %#v", tt.in, got, tt.want)
			}
			// --print must be first and the task its immediate value (so --print-timeout
			// never lands between --print and the task).
			if len(got) < 2 || got[0] != "--print" || got[1] != tt.in.task {
				t.Errorf("buildGeminiArgs must start with --print <task>; got %#v", got)
			}
		})
	}
}

func TestParseHopEnv(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantDepth int
		wantMax   int
	}{
		{
			name:      "missing values fall back to defaults",
			env:       map[string]string{},
			wantDepth: defaultHopDepth,
			wantMax:   defaultHopMax,
		},
		{
			name:      "both values parsed",
			env:       map[string]string{hopDepthEnv: "1", hopMaxEnv: "5"},
			wantDepth: 1,
			wantMax:   5,
		},
		{
			name:      "whitespace is trimmed",
			env:       map[string]string{hopDepthEnv: " 2 ", hopMaxEnv: " 3 "},
			wantDepth: 2,
			wantMax:   3,
		},
		{
			name:      "invalid depth falls back to default depth, valid max kept",
			env:       map[string]string{hopDepthEnv: "notanint", hopMaxEnv: "4"},
			wantDepth: defaultHopDepth,
			wantMax:   4,
		},
		{
			name:      "invalid max falls back to default max, valid depth kept",
			env:       map[string]string{hopDepthEnv: "1", hopMaxEnv: ""},
			wantDepth: 1,
			wantMax:   defaultHopMax,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			depth, max := parseHopEnv(getenv)
			if depth != tt.wantDepth || max != tt.wantMax {
				t.Errorf("parseHopEnv(%v) = (depth=%d, max=%d); want (depth=%d, max=%d)",
					tt.env, depth, max, tt.wantDepth, tt.wantMax)
			}
		})
	}
}

func TestHopLimitReached(t *testing.T) {
	tests := []struct {
		name  string
		depth int
		max   int
		want  bool
	}{
		{name: "default depth below default max allowed", depth: defaultHopDepth, max: defaultHopMax, want: false},
		{name: "below max allowed", depth: 1, max: 2, want: false},
		{name: "at max refused", depth: 2, max: 2, want: true},
		{name: "above max refused", depth: 3, max: 2, want: true},
		{name: "zero max refuses immediately", depth: 0, max: 0, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hopLimitReached(tt.depth, tt.max); got != tt.want {
				t.Errorf("hopLimitReached(%d, %d) = %v; want %v", tt.depth, tt.max, got, tt.want)
			}
		})
	}
}

func TestChildHopEnv(t *testing.T) {
	tests := []struct {
		name  string
		env   []string
		depth int
		want  []string
	}{
		{
			name:  "appends incremented depth when none present",
			env:   []string{"PATH=/bin", "HOME=/home/x"},
			depth: 0,
			want:  []string{"PATH=/bin", "HOME=/home/x", hopDepthEnv + "=1"},
		},
		{
			name:  "removes existing depth entry and appends incremented (no duplicate keys)",
			env:   []string{"PATH=/bin", hopDepthEnv + "=1", "HOME=/home/x"},
			depth: 1,
			want:  []string{"PATH=/bin", "HOME=/home/x", hopDepthEnv + "=2"},
		},
		{
			name:  "removes multiple stale depth entries",
			env:   []string{hopDepthEnv + "=0", "A=1", hopDepthEnv + "=5"},
			depth: 2,
			want:  []string{"A=1", hopDepthEnv + "=3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := childHopEnv(tt.env, tt.depth)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("childHopEnv(%v, %d) = %v; want %v", tt.env, tt.depth, got, tt.want)
			}
			// Exactly one AGENT_HOP_DEPTH key must remain.
			count := 0
			for _, kv := range got {
				if len(kv) > len(hopDepthEnv) && kv[:len(hopDepthEnv)+1] == hopDepthEnv+"=" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("childHopEnv produced %d %s entries; want exactly 1: %v", count, hopDepthEnv, got)
			}
		})
	}
}

func TestResolveAgyBinary(t *testing.T) {
	// AGY_BIN override takes top priority and is reachable without network.
	t.Setenv("AGY_BIN", "/custom/path/to/agy")
	if got := resolveAgyBinary(); got != "/custom/path/to/agy" {
		t.Errorf("resolveAgyBinary() with AGY_BIN set = %q; want %q", got, "/custom/path/to/agy")
	}

	// Whitespace-only override is treated as unset (falls through to lookup/fallback,
	// which must never be the override value).
	t.Setenv("AGY_BIN", "   ")
	if got := resolveAgyBinary(); got == "   " {
		t.Errorf("resolveAgyBinary() treated whitespace AGY_BIN as a path: %q", got)
	}
}

func TestResolveClaudeBinary(t *testing.T) {
	// CLAUDE_BIN override takes top priority and is reachable without network.
	t.Setenv("CLAUDE_BIN", "/custom/path/to/claude")
	if got := resolveClaudeBinary(); got != "/custom/path/to/claude" {
		t.Errorf("resolveClaudeBinary() with CLAUDE_BIN set = %q; want %q", got, "/custom/path/to/claude")
	}

	// Whitespace-only override is treated as unset (falls through to lookup/fallback,
	// which must never be the override value).
	t.Setenv("CLAUDE_BIN", "   ")
	if got := resolveClaudeBinary(); got == "   " {
		t.Errorf("resolveClaudeBinary() treated whitespace CLAUDE_BIN as a path: %q", got)
	}
}

func TestModeNotes(t *testing.T) {
	tests := []struct {
		name string
		note func(runOpts) string
		in   runOpts
		want string
	}{
		{
			name: "gemini reason-only",
			note: geminiModeNote,
			in:   runOpts{},
			want: "tool-use: disabled (reason/answer only)",
		},
		{
			name: "gemini allow_tools without sandbox",
			note: geminiModeNote,
			in:   runOpts{allowTools: true},
			want: "tool-use: ENABLED (--dangerously-skip-permissions)",
		},
		{
			name: "gemini allow_tools with sandbox",
			note: geminiModeNote,
			in:   runOpts{allowTools: true, sandbox: true},
			want: "tool-use: ENABLED (--dangerously-skip-permissions) in --sandbox",
		},
		{
			name: "claude reason-only",
			note: claudeModeNote,
			in:   runOpts{},
			want: "tool-use: disabled (reason/answer only)",
		},
		{
			name: "claude allow_tools (sandbox ignored)",
			note: claudeModeNote,
			in:   runOpts{allowTools: true, sandbox: true},
			want: "tool-use: ENABLED (--dangerously-skip-permissions)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.note(tt.in); got != tt.want {
				t.Errorf("modeNote(%+v) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

// --- runAgent integration tests ---------------------------------------------
//
// runAgent is the shared run path for both tools. Because backend.resolveBin is
// a function field, these tests point it at a fake executable and drive the real
// gemini/claude backends (real tool, cliName, buildArgs, modeNote) end-to-end —
// no actual agy/claude CLI is spawned.

// writeFakeBin writes an executable shell script to a temp dir and returns its path.
func writeFakeBin(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return p
}

// withBin returns a copy of b whose resolveBin yields the given path.
func withBin(b backend, path string) backend {
	b.resolveBin = func() string { return path }
	return b
}

// resultText concatenates the text content of a tool result.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		t.Fatal("nil result")
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestRunAgentHopLimit(t *testing.T) {
	for _, base := range []backend{geminiBackend, claudeBackend} {
		t.Run(base.tool, func(t *testing.T) {
			// depth == max => the guard must refuse before any spawn.
			t.Setenv(hopDepthEnv, "2")
			t.Setenv(hopMaxEnv, "2")
			// resolveBin points at a path that would fail loudly if ever executed.
			tb := withBin(base, "/nonexistent/should-not-run")

			res, err := runAgent(context.Background(), tb, runOpts{task: "x", timeoutSeconds: 300})
			if err != nil {
				t.Fatalf("expected nil Go error (tool-level result), got %v", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("expected an error result, got %+v", res)
			}
			txt := resultText(t, res)
			if !strings.Contains(txt, base.tool) || !strings.Contains(txt, "delegation-depth limit") {
				t.Errorf("hop-limit message missing tool name / limit text: %q", txt)
			}
		})
	}
}

func TestRunAgentOutcomes(t *testing.T) {
	// Keep the hop guard well below the limit for these cases.
	t.Setenv(hopDepthEnv, "0")
	t.Setenv(hopMaxEnv, "2")

	tests := []struct {
		name   string
		script string
		check  func(t *testing.T, b backend, res *mcp.CallToolResult, err error)
	}{
		{
			name:   "success passes child stdout through with header",
			script: "#!/bin/sh\necho CHILD-OUTPUT\n",
			check: func(t *testing.T, b backend, res *mcp.CallToolResult, err error) {
				if err != nil {
					t.Fatalf("unexpected Go error: %v", err)
				}
				if res.IsError {
					t.Fatalf("unexpected error result: %q", resultText(t, res))
				}
				txt := resultText(t, res)
				wantPrefix := "[" + b.tool + " | tool-use: disabled (reason/answer only) | "
				if !strings.HasPrefix(txt, wantPrefix) {
					t.Errorf("header wrong; got %q want prefix %q", txt, wantPrefix)
				}
				if !strings.Contains(txt, "CHILD-OUTPUT") {
					t.Errorf("child stdout not passed through: %q", txt)
				}
			},
		},
		{
			name:   "empty stdout yields cli-specific no-output note",
			script: "#!/bin/sh\nexit 0\n",
			check: func(t *testing.T, b backend, res *mcp.CallToolResult, err error) {
				if err != nil {
					t.Fatalf("unexpected Go error: %v", err)
				}
				if res.IsError {
					t.Fatalf("unexpected error result: %q", resultText(t, res))
				}
				want := "(" + b.cliName + " returned no stdout)"
				if txt := resultText(t, res); !strings.Contains(txt, want) {
					t.Errorf("missing no-output note %q in %q", want, txt)
				}
			},
		},
		{
			name:   "non-zero exit becomes an error result carrying stderr",
			script: "#!/bin/sh\necho boom 1>&2\nexit 3\n",
			check: func(t *testing.T, b backend, res *mcp.CallToolResult, err error) {
				if err != nil {
					t.Fatalf("unexpected Go error: %v", err)
				}
				if res == nil || !res.IsError {
					t.Fatalf("expected an error result, got %+v", res)
				}
				txt := resultText(t, res)
				if !strings.Contains(txt, b.tool+" failed") || !strings.Contains(txt, "boom") {
					t.Errorf("error result missing 'failed'/stderr text: %q", txt)
				}
			},
		},
	}

	for _, tt := range tests {
		for _, base := range []backend{geminiBackend, claudeBackend} {
			t.Run(tt.name+"/"+base.tool, func(t *testing.T) {
				tb := withBin(base, writeFakeBin(t, tt.script))
				res, err := runAgent(context.Background(), tb, runOpts{task: "do it", timeoutSeconds: 300})
				tt.check(t, tb, res, err)
			})
		}
	}
}

func TestRunAgentParentCancel(t *testing.T) {
	// Only parent-context cancellation returns a Go error (nil result); tool-level
	// failures are error results. Drive a slow child and cancel the parent.
	t.Setenv(hopDepthEnv, "0")
	t.Setenv(hopMaxEnv, "2")

	for _, base := range []backend{geminiBackend, claudeBackend} {
		t.Run(base.tool, func(t *testing.T) {
			// `exec` so the killed process IS the sleep (no orphaned grandchild
			// holding the stdout pipe open past the cancellation).
			tb := withBin(base, writeFakeBin(t, "#!/bin/sh\nexec sleep 5\n"))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			res, err := runAgent(ctx, tb, runOpts{task: "slow", timeoutSeconds: 300})
			if err == nil {
				t.Fatalf("expected a Go error on parent cancellation, got result %+v", res)
			}
			if res != nil {
				t.Errorf("expected nil result alongside the Go error, got %+v", res)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("expected context.DeadlineExceeded, got %v", err)
			}
		})
	}
}

func TestRunAgentTimeoutResult(t *testing.T) {
	t.Setenv(hopDepthEnv, "0")
	t.Setenv(hopMaxEnv, "2")
	// Shrink the headroom so the hard deadline fires quickly; restore after.
	orig := timeoutHeadroom
	timeoutHeadroom = 80 * time.Millisecond
	defer func() { timeoutHeadroom = orig }()

	for _, base := range []backend{geminiBackend, claudeBackend} {
		t.Run(base.tool, func(t *testing.T) {
			tb := withBin(base, writeFakeBin(t, "#!/bin/sh\nexec sleep 5\n"))
			// timeoutSeconds 0 => hardDeadline == timeoutHeadroom (80ms). Parent ctx
			// has no deadline, so the child-timeout branch (not parent-cancel) fires.
			res, err := runAgent(context.Background(), tb, runOpts{task: "slow", timeoutSeconds: 0})
			if err != nil {
				t.Fatalf("expected a tool result, got Go error: %v", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("expected an error result, got %+v", res)
			}
			if txt := resultText(t, res); !strings.Contains(txt, base.tool+" timed out after") {
				t.Errorf("expected timeout message, got %q", txt)
			}
		})
	}
}

// --- makeHandler request-parsing tests --------------------------------------
//
// makeHandler converts an mcp.CallToolRequest into runOpts and delegates to
// runAgent. These tests drive it with an echo fake (prints its argv) so the
// parsed/clamped/gated values are observable in the resulting args; plus
// task-validation and ctx-cancel paths that return before any spawn.

// handlerEchoArgs strips the result header and returns the echoed child argv.
func handlerEchoArgs(t *testing.T, res *mcp.CallToolResult) []string {
	t.Helper()
	parts := strings.SplitN(resultText(t, res), "\n\n", 2)
	if len(parts) != 2 {
		t.Fatalf("result has no body after header: %q", resultText(t, res))
	}
	if strings.TrimSpace(parts[1]) == "" {
		return nil
	}
	return strings.Split(parts[1], "\n")
}

// argsHave reports whether want appears as a contiguous subsequence of args.
func argsHave(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestMakeHandlerParsing(t *testing.T) {
	t.Setenv(hopDepthEnv, "0")
	t.Setenv(hopMaxEnv, "2")
	echo := writeFakeBin(t, "#!/bin/sh\nprintf '%s\\n' \"$@\"\n")

	call := func(b backend, supportsSandbox bool, args map[string]any) (*mcp.CallToolResult, error) {
		h := makeHandler(withBin(b, echo), supportsSandbox)
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: b.tool, Arguments: args}}
		return h(context.Background(), req)
	}

	t.Run("empty/whitespace task is rejected before spawning", func(t *testing.T) {
		for _, b := range []backend{geminiBackend, claudeBackend} {
			res, err := call(b, true, map[string]any{"task": "   "})
			if err != nil {
				t.Fatalf("[%s] unexpected Go error: %v", b.tool, err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("[%s] expected error result, got %+v", b.tool, res)
			}
			if txt := resultText(t, res); !strings.Contains(txt, "is required and must be a non-empty string") {
				t.Errorf("[%s] wrong message: %q", b.tool, txt)
			}
		}
	})

	t.Run("timeout defaults and clamps (gemini --print-timeout)", func(t *testing.T) {
		cases := []struct {
			name string
			args map[string]any
			want string
		}{
			{"missing -> default", map[string]any{"task": "x"}, "300s"},
			{"zero -> default", map[string]any{"task": "x", "timeout_seconds": 0}, "300s"},
			{"negative -> default", map[string]any{"task": "x", "timeout_seconds": -5}, "300s"},
			{"over max -> clamped", map[string]any{"task": "x", "timeout_seconds": 999999}, "1800s"},
			{"in range -> kept", map[string]any{"task": "x", "timeout_seconds": 600}, "600s"},
		}
		for _, c := range cases {
			res, _ := call(geminiBackend, true, c.args)
			if a := handlerEchoArgs(t, res); !argsHave(a, "--print-timeout", c.want) {
				t.Errorf("%s: want --print-timeout %s; args=%v", c.name, c.want, a)
			}
		}
	})

	t.Run("sandbox gated to gemini only", func(t *testing.T) {
		res, _ := call(geminiBackend, true, map[string]any{"task": "x", "sandbox": true})
		if a := handlerEchoArgs(t, res); !argsContain(a, "--sandbox") {
			t.Errorf("gemini should pass --sandbox; args=%v", a)
		}
		// claude is registered with supportsSandbox=false: the param is never read.
		res, _ = call(claudeBackend, false, map[string]any{"task": "x", "sandbox": true})
		if a := handlerEchoArgs(t, res); argsContain(a, "--sandbox") {
			t.Errorf("claude must never pass --sandbox; args=%v", a)
		}
	})

	t.Run("add_dirs trimmed and empties dropped", func(t *testing.T) {
		res, _ := call(geminiBackend, true, map[string]any{
			"task":     "x",
			"add_dirs": []any{"/a", "  ", "", " /b "},
		})
		a := handlerEchoArgs(t, res)
		if !argsHave(a, "--add-dir", "/a") || !argsHave(a, "--add-dir", "/b") {
			t.Errorf("want --add-dir /a and /b; args=%v", a)
		}
		if argsContain(a, "  ") || argsContain(a, "") {
			t.Errorf("empty/whitespace add_dirs must be dropped; args=%v", a)
		}
	})

	t.Run("model trimmed; blank dropped", func(t *testing.T) {
		res, _ := call(claudeBackend, false, map[string]any{"task": "x", "model": "  opus  "})
		if a := handlerEchoArgs(t, res); !argsHave(a, "--model", "opus") {
			t.Errorf("model should be trimmed to opus; args=%v", a)
		}
		res, _ = call(claudeBackend, false, map[string]any{"task": "x", "model": "   "})
		if a := handlerEchoArgs(t, res); argsContain(a, "--model") {
			t.Errorf("blank model must be dropped; args=%v", a)
		}
	})

	t.Run("allow_tools adds skip-permissions", func(t *testing.T) {
		res, _ := call(claudeBackend, false, map[string]any{"task": "x", "allow_tools": true})
		if a := handlerEchoArgs(t, res); !argsContain(a, "--dangerously-skip-permissions") {
			t.Errorf("allow_tools should pass the skip flag; args=%v", a)
		}
	})

	t.Run("working_dir sets the child cwd", func(t *testing.T) {
		dir := t.TempDir()
		// The fake writes a marker into its cwd; if working_dir applied, it lands in dir.
		marker := writeFakeBin(t, "#!/bin/sh\ntouch cwd-marker\n")
		h := makeHandler(withBin(geminiBackend, marker), true)
		req := mcp.CallToolRequest{Params: mcp.CallToolParams{
			Name:      geminiBackend.tool,
			Arguments: map[string]any{"task": "x", "working_dir": dir},
		}}
		if _, err := h(context.Background(), req); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "cwd-marker")); err != nil {
			t.Errorf("working_dir not applied; marker missing in %s: %v", dir, err)
		}
	})

	t.Run("cancelled context returns a Go error before spawning", func(t *testing.T) {
		for _, b := range []backend{geminiBackend, claudeBackend} {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			h := makeHandler(withBin(b, echo), true)
			req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: b.tool, Arguments: map[string]any{"task": "x"}}}
			res, err := h(ctx, req)
			if err == nil {
				t.Fatalf("[%s] expected Go error on cancelled ctx, got %+v", b.tool, res)
			}
			if res != nil {
				t.Errorf("[%s] expected nil result alongside Go error, got %+v", b.tool, res)
			}
		}
	})
}

// --- resolveBin fallback-chain tests ----------------------------------------

// writeExec writes an empty executable named `name` into dir (creating dir).
func writeExec(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	return p
}

func TestResolveBinaryFallbacks(t *testing.T) {
	backends := []struct {
		name    string
		envKey  string
		binName string
		resolve func() string
	}{
		{"agy", "AGY_BIN", "agy", resolveAgyBinary},
		{"claude", "CLAUDE_BIN", "claude", resolveClaudeBinary},
	}

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Run("found on PATH via LookPath", func(t *testing.T) {
				t.Setenv(b.envKey, "")
				dir := t.TempDir()
				want := writeExec(t, dir, b.binName)
				t.Setenv("PATH", dir)
				if got := b.resolve(); got != want {
					t.Errorf("resolve() = %q; want PATH hit %q", got, want)
				}
			})

			t.Run("falls back to ~/.local/bin", func(t *testing.T) {
				t.Setenv(b.envKey, "")
				t.Setenv("PATH", t.TempDir()) // a dir without the binary
				home := t.TempDir()
				t.Setenv("HOME", home)
				want := writeExec(t, filepath.Join(home, ".local", "bin"), b.binName)
				if got := b.resolve(); got != want {
					t.Errorf("resolve() = %q; want ~/.local/bin fallback %q", got, want)
				}
			})

			t.Run("falls back to bare name", func(t *testing.T) {
				t.Setenv(b.envKey, "")
				t.Setenv("PATH", t.TempDir())
				t.Setenv("HOME", t.TempDir()) // no .local/bin/<bin>
				if got := b.resolve(); got != b.binName {
					t.Errorf("resolve() = %q; want bare name %q", got, b.binName)
				}
			})
		})
	}
}
