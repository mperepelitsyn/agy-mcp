// Command agy-mcp is a tiny MCP (Model Context Protocol) server that bridges two
// coding agents in BOTH directions, exposing each as a spawnable sub-agent tool.
//
// Two tools are registered:
//
//   - gemini_agent — shells out to the Antigravity CLI (`agy --print <task>`),
//     i.e. spawns a Gemini sub-agent. Intended to be called from a Claude session.
//   - claude_agent — shells out to the Claude CLI (`claude --print <task>`),
//     i.e. spawns a Claude sub-agent. Intended to be called from a Gemini session.
//
// A parent agent calls the tool with a self-contained task; this server shells
// out to the corresponding CLI, lets the child agent perform the task, and
// returns the child's full output. In effect each tool is a spawned sub-agent
// callable from inside another agent's session.
//
// Safety: tool-use (the child editing files / running commands) is DISABLED by
// default. In the default mode the server runs the CLI with no permission-bypass,
// so the child can reason/answer but cannot take unattended actions. To let the
// spawned agent actually edit files in working_dir, the caller sets
// `allow_tools: true`:
//   - gemini_agent passes --dangerously-skip-permissions to `agy`.
//   - claude_agent passes --dangerously-skip-permissions to `claude`.
//
// Scope it with `working_dir`. For gemini_agent the `--sandbox` flag is OFF by
// default because it confines edits to an isolated scratch dir (set
// `sandbox: true` only for a confined "compute but don't touch my files" run);
// claude_agent has NO sandbox option. The tool result header always reports
// which mode ran.
//
// Loop guard: to prevent runaway A→B→A→B delegation chains, the shared run path
// reads AGENT_HOP_DEPTH (current delegation depth, default 0) and AGENT_HOP_MAX
// (max allowed depth, default 2) from the environment. If the current depth has
// reached the max, the tool returns an error instead of spawning a child.
// Otherwise the child is spawned with AGENT_HOP_DEPTH incremented by one.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	defaultTimeoutSeconds = 300
	maxTimeoutSeconds     = 1800

	// hopDepthEnv tracks the current delegation depth; hopMaxEnv caps it.
	hopDepthEnv     = "AGENT_HOP_DEPTH"
	hopMaxEnv       = "AGENT_HOP_MAX"
	defaultHopMax   = 2
	defaultHopDepth = 0
)

// timeoutHeadroom is the extra wall-clock allowed beyond the requested timeout
// before the child process is hard-killed, so the child can surface its own
// timeout message first. A var (not a const) so tests can shrink it.
var timeoutHeadroom = 30 * time.Second

// runOpts carries the fully-parsed, backend-agnostic parameters of a single
// tool invocation. It is a plain struct (no req access) so that buildArgs can
// be a pure, table-testable function.
type runOpts struct {
	task           string
	timeoutSeconds int
	allowTools     bool
	sandbox        bool // gemini-only; ignored by the claude backend
	model          string
	addDirs        []string
	workingDir     string
}

// backend describes one CLI adapter. The run/timeout/truncate/header/
// context-cancel/hop-guard logic in runAgent is shared across backends; each
// backend only supplies its tool name, binary resolver, argument builder, and
// mode-note formatter.
type backend struct {
	tool       string        // "gemini_agent" / "claude_agent"
	cliName    string        // underlying CLI name for the "(<cli> returned no stdout)" note
	resolveBin func() string // resolves the CLI executable path
	buildArgs  func(o runOpts) []string
	modeNote   func(o runOpts) string
}

// resolveAgyBinary finds the `agy` executable. Priority: AGY_BIN env override,
// then PATH, then the known install location. Claude Code may spawn this server
// with a minimal PATH, so the explicit fallback matters.
func resolveAgyBinary() string {
	if v := strings.TrimSpace(os.Getenv("AGY_BIN")); v != "" {
		return v
	}
	if p, err := exec.LookPath("agy"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		fallback := filepath.Join(home, ".local", "bin", "agy")
		if _, statErr := os.Stat(fallback); statErr == nil {
			return fallback
		}
	}
	return "agy"
}

// resolveClaudeBinary finds the `claude` executable. Priority: CLAUDE_BIN env
// override, then PATH, then the known install location. Mirrors
// resolveAgyBinary — a Gemini parent may spawn this server with a minimal PATH,
// so the explicit fallback matters.
func resolveClaudeBinary() string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_BIN")); v != "" {
		return v
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		fallback := filepath.Join(home, ".local", "bin", "claude")
		if _, statErr := os.Stat(fallback); statErr == nil {
			return fallback
		}
	}
	return "claude"
}

// buildGeminiArgs builds the `agy` invocation. CRITICAL: `--print` takes the
// prompt as its VALUE — the task must come immediately after it, with all other
// flags AFTER the prompt. (agy usage: `agy --print "<prompt>" --print-timeout 10m`.)
// Putting --print-timeout between --print and the task makes agy treat
// "--print-timeout" as the prompt. Pure function — table-testable.
func buildGeminiArgs(o runOpts) []string {
	args := []string{
		"--print", o.task,
		"--print-timeout", fmt.Sprintf("%ds", o.timeoutSeconds),
	}
	if strings.TrimSpace(o.model) != "" {
		args = append(args, "--model", o.model)
	}
	for _, d := range o.addDirs {
		args = append(args, "--add-dir", d)
	}
	if o.allowTools {
		args = append(args, "--dangerously-skip-permissions")
	}
	if o.sandbox {
		args = append(args, "--sandbox")
	}
	return args
}

// buildClaudeArgs builds the `claude` invocation. As with agy, `--print` takes
// the prompt as its VALUE, so the task comes immediately after it with all other
// flags AFTER it. claude has NO --print-timeout flag (the timeout is enforced
// purely by the process context deadline) and NO --sandbox flag (sandbox is
// gemini-only). Pure function — table-testable.
func buildClaudeArgs(o runOpts) []string {
	args := []string{
		"--print", o.task,
	}
	if strings.TrimSpace(o.model) != "" {
		args = append(args, "--model", o.model)
	}
	for _, d := range o.addDirs {
		args = append(args, "--add-dir", d)
	}
	if o.allowTools {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// geminiModeNote describes the run mode for gemini_agent's result header.
func geminiModeNote(o runOpts) string {
	if !o.allowTools {
		return "tool-use: disabled (reason/answer only)"
	}
	note := "tool-use: ENABLED (--dangerously-skip-permissions)"
	if o.sandbox {
		note += " in --sandbox"
	}
	return note
}

// claudeModeNote describes the run mode for claude_agent's result header.
func claudeModeNote(o runOpts) string {
	if !o.allowTools {
		return "tool-use: disabled (reason/answer only)"
	}
	return "tool-use: ENABLED (--dangerously-skip-permissions)"
}

var geminiBackend = backend{
	tool:       "gemini_agent",
	cliName:    "agy",
	resolveBin: resolveAgyBinary,
	buildArgs:  buildGeminiArgs,
	modeNote:   geminiModeNote,
}

var claudeBackend = backend{
	tool:       "claude_agent",
	cliName:    "claude",
	resolveBin: resolveClaudeBinary,
	buildArgs:  buildClaudeArgs,
	modeNote:   claudeModeNote,
}

// parseHopEnv reads the current delegation depth and max from a getenv-style
// lookup function. Invalid or missing values fall back to the defaults. Pure
// function — table-testable (pass it a map-backed getenv in tests).
func parseHopEnv(getenv func(string) string) (depth, hopMax int) {
	depth = defaultHopDepth
	if v, err := strconv.Atoi(strings.TrimSpace(getenv(hopDepthEnv))); err == nil {
		depth = v
	}
	hopMax = defaultHopMax
	if v, err := strconv.Atoi(strings.TrimSpace(getenv(hopMaxEnv))); err == nil {
		hopMax = v
	}
	return depth, hopMax
}

// hopLimitReached reports whether the current delegation depth has reached the
// configured maximum. Pure function — table-testable.
func hopLimitReached(depth, hopMax int) bool {
	return depth >= hopMax
}

// childHopEnv returns a copy of env (an os.Environ()-style slice) with any
// existing AGENT_HOP_DEPTH entry REMOVED and a single
// "AGENT_HOP_DEPTH=<depth+1>" appended, so the spawned child sees the
// incremented depth with no duplicate keys. Pure function — table-testable.
func childHopEnv(env []string, depth int) []string {
	prefix := hopDepthEnv + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, fmt.Sprintf("%s=%d", hopDepthEnv, depth+1))
	return out
}

func main() {
	s := server.NewMCPServer(
		"agy-mcp",
		"0.1.0",
		server.WithToolCapabilities(false),
	)

	geminiTool := mcp.NewTool("gemini_agent",
		mcp.WithDescription(
			"Spawn a Gemini agent (via the Antigravity `agy` CLI) to perform a task and return its response. "+
				"Give it a self-contained task in `task`; it runs non-interactively and returns Gemini's full output. "+
				"By default the spawned agent can reason and answer but CANNOT take unattended actions (no file edits / "+
				"command execution) — set `allow_tools: true` to let it act, which disables Gemini's permission prompts "+
				"and (by default) runs it in a restricted sandbox. Use `add_dirs` to give it workspace context and "+
				"`working_dir` to set where it runs.",
		),
		mcp.WithString("task",
			mcp.Required(),
			mcp.Description("The complete, self-contained task/prompt for the Gemini agent to perform."),
		),
		mcp.WithArray("add_dirs",
			mcp.Description("Directories to add to the agent's workspace (absolute paths). Repeatable."),
			mcp.Items(map[string]any{"type": "string"}),
		),
		mcp.WithString("working_dir",
			mcp.Description("Directory the agent runs in (absolute path). Defaults to this server's working directory."),
		),
		mcp.WithNumber("timeout_seconds",
			mcp.Description(fmt.Sprintf("Max seconds to wait for the agent (default %d, max %d).", defaultTimeoutSeconds, maxTimeoutSeconds)),
		),
		mcp.WithString("model",
			mcp.Description("Optional model to use (passed as --model). Leave empty for the CLI default."),
		),
		mcp.WithBoolean("allow_tools",
			mcp.Description("Allow the spawned agent to take actions (edit files in working_dir, run commands) by "+
				"auto-approving its permission prompts (passes --dangerously-skip-permissions). Default false "+
				"(reason/answer only). Use with care — this is unattended execution; scope it with working_dir."),
		),
		mcp.WithBoolean("sandbox",
			mcp.Description("Confine the agent to an isolated scratch dir with terminal restrictions (--sandbox). "+
				"Default false. WARNING: when true, the agent's file edits go to a scratch dir, NOT working_dir — "+
				"use only for a confined 'compute but don't touch my files' run."),
		),
	)

	claudeTool := mcp.NewTool("claude_agent",
		mcp.WithDescription(
			"Spawn a Claude agent (via the `claude` CLI) to perform a task and return its response. "+
				"Give it a self-contained task in `task`; it runs non-interactively (`claude --print`) and returns "+
				"Claude's full output. By default the spawned agent can reason and answer but CANNOT take unattended "+
				"actions (no file edits / command execution) — set `allow_tools: true` to let it act, which passes "+
				"--dangerously-skip-permissions so Claude auto-approves its own permission prompts and runs UNATTENDED "+
				"(it will edit files / run commands and consume Claude credits without further confirmation). "+
				"Use `add_dirs` to give it workspace context and `working_dir` to set where it runs. "+
				"Note: even reason-only runs consume Claude credits.",
		),
		mcp.WithString("task",
			mcp.Required(),
			mcp.Description("The complete, self-contained task/prompt for the Claude agent to perform."),
		),
		mcp.WithArray("add_dirs",
			mcp.Description("Directories to add to the agent's workspace (absolute paths). Repeatable."),
			mcp.Items(map[string]any{"type": "string"}),
		),
		mcp.WithString("working_dir",
			mcp.Description("Directory the agent runs in (absolute path). Defaults to this server's working directory."),
		),
		mcp.WithNumber("timeout_seconds",
			mcp.Description(fmt.Sprintf("Max seconds to wait for the agent (default %d, max %d).", defaultTimeoutSeconds, maxTimeoutSeconds)),
		),
		mcp.WithString("model",
			mcp.Description("Optional model to use (passed as --model). Leave empty for the CLI default."),
		),
		mcp.WithBoolean("allow_tools",
			mcp.Description("Allow the spawned agent to take actions (edit files in working_dir, run commands) by "+
				"auto-approving its permission prompts (passes --dangerously-skip-permissions). Default false "+
				"(reason/answer only). Use with care — this is unattended execution that consumes Claude credits; "+
				"scope it with working_dir."),
		),
	)

	s.AddTool(geminiTool, makeHandler(geminiBackend, true))
	s.AddTool(claudeTool, makeHandler(claudeBackend, false))

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "agy-mcp: server error: %v\n", err)
		os.Exit(1)
	}
}

// makeHandler returns the MCP handler for a backend. supportsSandbox controls
// whether the `sandbox` param is read from the request (gemini only).
func makeHandler(b backend, supportsSandbox bool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Check context cancellation before executing.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		task := strings.TrimSpace(req.GetString("task", ""))
		if task == "" {
			return mcp.NewToolResultError("`task` is required and must be a non-empty string"), nil
		}

		timeoutSeconds := defaultTimeoutSeconds
		if v := req.GetInt("timeout_seconds", 0); v > 0 {
			timeoutSeconds = v
		}
		if timeoutSeconds > maxTimeoutSeconds {
			timeoutSeconds = maxTimeoutSeconds
		}

		o := runOpts{
			task:           task,
			timeoutSeconds: timeoutSeconds,
			allowTools:     req.GetBool("allow_tools", false),
			model:          strings.TrimSpace(req.GetString("model", "")),
			workingDir:     req.GetString("working_dir", ""),
		}

		// sandbox defaults OFF and is gemini-only. With --sandbox, agy confines
		// the agent to an isolated scratch dir, so its file edits do NOT land in
		// working_dir — useless for real project edits. Callers wanting a
		// confined "compute but don't touch my files" run set sandbox: true
		// explicitly. claude has no sandbox concept, so the param is not read.
		if supportsSandbox {
			o.sandbox = req.GetBool("sandbox", false)
		}

		for _, d := range req.GetStringSlice("add_dirs", nil) {
			if s := strings.TrimSpace(d); s != "" {
				o.addDirs = append(o.addDirs, s)
			}
		}

		return runAgent(ctx, b, o)
	}
}

// runAgent is the shared backend run path: hop guard, command construction,
// timeout/context handling, truncation, and header formatting. Tool-level
// failures (timeout, child error, hop limit) are encoded as MCP error results
// with a nil Go error; only parent-context cancellation returns a Go error,
// mirroring the original gemini_agent behavior.
func runAgent(ctx context.Context, b backend, o runOpts) (*mcp.CallToolResult, error) {
	// Loop guard: refuse to spawn a child once the delegation depth limit is
	// reached, to prevent runaway A→B→A→B chains.
	depth, hopMax := parseHopEnv(os.Getenv)
	if hopLimitReached(depth, hopMax) {
		return mcp.NewToolResultError(fmt.Sprintf(
			"%s: delegation-depth limit reached (%s=%d, %s=%d). "+
				"Refusing to spawn another agent to avoid a runaway delegation loop. "+
				"Perform this task directly instead of delegating further.",
			b.tool, hopDepthEnv, depth, hopMaxEnv, hopMax,
		)), nil
	}

	args := b.buildArgs(o)
	modeNote := b.modeNote(o)

	// Give the process a little headroom beyond the requested timeout so we
	// surface the child's own timeout message rather than killing it first.
	hardDeadline := time.Duration(o.timeoutSeconds)*time.Second + timeoutHeadroom
	runCtx, cancel := context.WithTimeout(ctx, hardDeadline)
	defer cancel()

	cmd := exec.CommandContext(runCtx, b.resolveBin(), args...)
	if strings.TrimSpace(o.workingDir) != "" {
		cmd.Dir = o.workingDir
	}
	// Spawn the child with an incremented delegation depth (no duplicate keys).
	cmd.Env = childHopEnv(os.Environ(), depth)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start).Round(time.Millisecond)

	// If the parent context was canceled, return the cancellation error
	// (mirrors the original gemini_agent behavior: a Go error, not a result).
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if runCtx.Err() == context.DeadlineExceeded {
		return mcp.NewToolResultError(fmt.Sprintf(
			"%s timed out after %s (%s).\nPartial stdout:\n%s\nstderr:\n%s",
			b.tool, elapsed, modeNote, truncate(stdout.String(), 8000), truncate(stderr.String(), 2000),
		)), nil
	}

	if runErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf(
			"%s failed (%s): %v\nstderr:\n%s\nstdout:\n%s",
			b.tool, modeNote, runErr, truncate(stderr.String(), 4000), truncate(stdout.String(), 8000),
		)), nil
	}

	out := strings.TrimRight(stdout.String(), "\n")
	if strings.TrimSpace(out) == "" {
		out = fmt.Sprintf("(%s returned no stdout)", b.cliName)
		if se := strings.TrimSpace(stderr.String()); se != "" {
			out += "\nstderr:\n" + truncate(se, 2000)
		}
	}

	header := fmt.Sprintf("[%s | %s | %s]\n\n", b.tool, modeNote, elapsed)
	return mcp.NewToolResultText(header + out), nil
}

// truncate returns a copy of s truncated to at most max bytes, without splitting UTF-8 runes.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Back up to a valid UTF-8 rune boundary.
	// Continuation bytes start with the bits 10xxxxxx, i.e., byte & 0xC0 == 0x80.
	i := max
	for i > 0 && (s[i]&0xC0 == 0x80) {
		i--
	}
	return s[:i] + fmt.Sprintf("\n…(truncated, %d bytes total)", len(s))
}
