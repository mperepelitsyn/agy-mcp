// Command agent-bridge-mcp is a tiny MCP (Model Context Protocol) server that
// bridges coding agents, exposing each as a spawnable sub-agent tool.
//
// Three tools are registered:
//
//   - gemini_agent — shells out to the Antigravity CLI (`agy --print <task>`),
//     i.e. spawns a Gemini sub-agent. Intended to be called from a Claude session.
//   - claude_agent — shells out to the Claude CLI (`claude --print <task>`),
//     i.e. spawns a Claude sub-agent. Intended to be called from a Gemini session.
//   - codex_agent — shells out to the OpenAI Codex CLI (`codex exec <task>`),
//     i.e. spawns a Codex sub-agent. Callable from any parent session.
//
// A parent agent calls the tool with a self-contained task; this server shells
// out to the corresponding CLI, lets the child agent perform the task, and
// returns the child's full output. In effect each tool is a spawned sub-agent
// callable from inside another agent's session. Backends are declared in a small
// in-code registry (see `backends`), so adding another CLI coding-agent is one
// entry, not new code.
//
// Safety: tool-use (the child editing files / running commands) is DISABLED by
// default. In the default mode the server runs the CLI with no permission-bypass,
// so the child can reason/answer but cannot take unattended actions. To let the
// spawned agent actually edit files in working_dir, the caller sets
// `allow_tools: true`:
//   - gemini_agent passes --dangerously-skip-permissions to `agy`.
//   - claude_agent passes --dangerously-skip-permissions to `claude`.
//   - codex_agent passes --dangerously-bypass-approvals-and-sandbox to `codex`.
//
// Scope it with `working_dir`. For gemini_agent the `--sandbox` flag is OFF by
// default because it confines edits to an isolated scratch dir (set
// `sandbox: true` only for a confined "compute but don't touch my files" run);
// claude_agent has NO sandbox option. codex_agent has no pure no-tools mode, so
// its default (`allow_tools: false`) runs it in a read-only sandbox
// (--sandbox read-only) rather than fully disabling tools. The tool result header
// always reports which mode ran.
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

	// geminiTimeoutHeadroom is the extra wall-clock allowed beyond the requested
	// timeout before agy is hard-killed, so agy's own --print-timeout fires first
	// and surfaces its message. claude has no --print-timeout, so its backend uses
	// zero headroom (the context deadline IS the timeout).
	geminiTimeoutHeadroom = 30 * time.Second

	// childWaitDelay bounds how long cmd.Run may block on stdout/stderr I/O after
	// the child is killed, so a grandchild that inherited the pipes cannot hang the
	// call past the deadline. Paired with process-group kill (setupProcessGroup).
	childWaitDelay = 5 * time.Second
)

// runOpts carries the fully-parsed, backend-agnostic parameters of a single
// tool invocation. It is a plain struct (no req access) so that buildArgs can
// be a pure, table-testable function.
type runOpts struct {
	task           string
	timeoutSeconds int
	allowTools     bool
	sandbox        bool // gemini-only boolean --sandbox; ignored by claude/codex
	model          string
	addDirs        []string
	workingDir     string
}

// Model-facing tool descriptions. The prose differs per backend; the parameter
// set is shared (see commonToolOptions) so the tool schemas can't drift.
const (
	geminiToolDescription = "Spawn a Gemini agent (via the Antigravity `agy` CLI) to perform a task and return its " +
		"response. Give it a self-contained task in `task`; it runs non-interactively and returns Gemini's full " +
		"output. By default the spawned agent can reason and answer but CANNOT take unattended actions (no file " +
		"edits / command execution) — set `allow_tools: true` to let it act, which disables Gemini's permission " +
		"prompts and runs it UNATTENDED, with edits landing in `working_dir`. Sandboxing is OFF by default; set " +
		"`sandbox: true` to instead confine edits to an isolated scratch dir. Use `add_dirs` for workspace context " +
		"and `working_dir` to set where it runs."

	claudeToolDescription = "Spawn a Claude agent (via the `claude` CLI) to perform a task and return its response. " +
		"Give it a self-contained task in `task`; it runs non-interactively (`claude --print`) and returns Claude's " +
		"full output. By default the spawned agent can reason and answer but CANNOT take unattended actions (no file " +
		"edits / command execution) — set `allow_tools: true` to let it act, which passes --dangerously-skip-permissions " +
		"so Claude auto-approves its own permission prompts and runs UNATTENDED (it will edit files / run commands and " +
		"consume Claude credits without further confirmation). Use `add_dirs` for workspace context and `working_dir` " +
		"to set where it runs. Note: even reason-only runs consume Claude credits."

	geminiAllowToolsDescription = "Allow the spawned agent to take actions (edit files in working_dir, run commands) by " +
		"auto-approving its permission prompts (passes --dangerously-skip-permissions). Default false (reason/answer " +
		"only). Use with care — this is unattended execution; scope it with working_dir."

	claudeAllowToolsDescription = "Allow the spawned agent to take actions (edit files in working_dir, run commands) by " +
		"auto-approving its permission prompts (passes --dangerously-skip-permissions). Default false (reason/answer " +
		"only). Use with care — this is unattended execution that consumes Claude credits; scope it with working_dir."

	codexToolDescription = "Spawn an OpenAI Codex agent (via the `codex` CLI, `codex exec`) to perform a task and return " +
		"its response. Give it a self-contained task in `task`; it runs non-interactively and returns Codex's full " +
		"output. By default (`allow_tools` false) the agent runs READ-ONLY (--sandbox read-only): it can read files " +
		"and reason but CANNOT edit files or run effectful commands — note this is a read-only sandbox, not a pure " +
		"no-tools mode. Set `allow_tools: true` to let it act, which passes --dangerously-bypass-approvals-and-sandbox " +
		"so it runs UNATTENDED with full file/command access and NO sandbox, with edits landing in `working_dir`. Use " +
		"`add_dirs` for additional writable context and `working_dir` to set where it runs. Codex runs even outside a " +
		"Git repo (--skip-git-repo-check is always passed). The tool returns Codex's final message; its session banner " +
		"and step-by-step transcript go to stderr and are surfaced only if the run fails or times out."

	codexAllowToolsDescription = "Allow the spawned agent to take actions (edit files in working_dir, run commands) by " +
		"passing --dangerously-bypass-approvals-and-sandbox, which skips ALL approvals AND disables Codex's sandbox. " +
		"Default false (read-only sandbox: reads/reasons, no writes). Use with care — this is fully unattended, " +
		"unsandboxed execution; scope it with working_dir."

	sandboxDescription = "Confine the agent to an isolated scratch dir with terminal restrictions (--sandbox). Default " +
		"false. WARNING: when true, the agent's file edits go to a scratch dir, NOT working_dir — use only for a " +
		"confined 'compute but don't touch my files' run."
)

// backend declares one CLI adapter as DATA: the shared run/timeout/truncate/
// header/context-cancel/hop-guard logic lives in runAgent, so adding a CLI
// coding-agent is a single registry entry (see the registry below), not new code.
// Optional flags (timeoutFlag, sandboxFlag) are "" when the CLI lacks them.
type backend struct {
	tool    string // MCP tool name, e.g. "gemini_agent"
	cliName string // CLI binary name, e.g. "agy"; used for PATH/fallback lookup and the "(<cli> returned no stdout)" note
	binEnv  string // env override for the CLI path, e.g. "AGY_BIN"

	// subcmd is emitted right after the binary, before any flags or the prompt
	// (e.g. ["exec"] for codex). nil for CLIs invoked as `<bin> [flags] <prompt>`.
	subcmd []string

	// Flag names. For flag-style CLIs (gemini/claude) promptFlag carries the task
	// as its VALUE and is emitted FIRST; every other flag follows. "" means the CLI
	// does not support that flag. When promptPositional is set the task is a
	// trailing positional argument instead and promptFlag is unused.
	promptFlag    string // "--print" (flag-style) | "" (positional, see promptPositional)
	timeoutFlag   string // "--print-timeout" (gemini) | "" (claude/codex: ctx deadline only)
	modelFlag     string // "--model"
	addDirFlag    string // "--add-dir"
	skipPermsFlag string // "--dangerously-skip-permissions" (gemini/claude) | "--dangerously-bypass-approvals-and-sandbox" (codex)
	sandboxFlag   string // "--sandbox" (gemini boolean) | "" (claude/codex)

	// promptPositional makes the task a trailing positional argument (emitted LAST,
	// after subcmd and every flag) instead of promptFlag's value — for CLIs like
	// codex whose non-interactive form is `<bin> exec [flags] <prompt>`.
	promptPositional bool

	// extraArgs are static flags always appended to the invocation (e.g. codex's
	// ["--skip-git-repo-check", "--color", "never"]). nil for CLIs that need none.
	extraArgs []string

	// reasonOnlyArgs are appended only when allow_tools is false — the restraint a
	// CLI needs to stay read-only when it has no true no-tools mode (codex:
	// ["--sandbox", "read-only"]). gemini/claude leave this nil: omitting the
	// skip-perms flag already makes them reason-only.
	reasonOnlyArgs []string

	// reasonOnlyNote overrides the result-header mode note for reason-only runs.
	// "" yields the default "tool-use: disabled (reason/answer only)"; codex sets it
	// to reflect that its reason-only run is a read-only sandbox, not pure no-tools.
	reasonOnlyNote string

	// timeoutHeadroom is extra wall-clock added to the requested timeout before the
	// child is hard-killed. Non-zero only for CLIs with their own internal timeout
	// (gemini/agy); zero for claude/codex (the context deadline is the timeout).
	timeoutHeadroom time.Duration

	description    string // model-facing tool description
	allowToolsDesc string // description for the allow_tools param
}

// supportsSandbox reports whether the backend exposes the sandbox option.
func (b backend) supportsSandbox() bool { return b.sandboxFlag != "" }

// resolveBin finds the backend's CLI executable: binEnv override → PATH →
// ~/.local/bin/<cliName> → bare cliName. The explicit fallback matters because a
// parent agent may spawn this server with a minimal PATH.
func (b backend) resolveBin() string {
	if v := strings.TrimSpace(os.Getenv(b.binEnv)); v != "" {
		return v
	}
	if p, err := exec.LookPath(b.cliName); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		fallback := filepath.Join(home, ".local", "bin", b.cliName)
		if _, statErr := os.Stat(fallback); statErr == nil {
			return fallback
		}
	}
	return b.cliName
}

// buildArgs builds the CLI invocation from the backend's flag spec. CRITICAL for
// flag-style CLIs: promptFlag takes the task as its VALUE and is emitted FIRST,
// with every other flag AFTER it — putting another flag between promptFlag and the
// task makes the CLI treat that flag as the prompt. Positional CLIs
// (promptPositional) instead carry the task as a TRAILING argument emitted last,
// after subcmd and every flag (e.g. `codex exec [flags] <prompt>`). An empty flag
// name means the CLI lacks that option, so it (and its value/loop) is skipped.
// Pure — table-testable.
func (b backend) buildArgs(o runOpts) []string {
	args := append([]string{}, b.subcmd...)
	if !b.promptPositional {
		args = append(args, b.promptFlag, o.task)
	}
	if b.timeoutFlag != "" {
		args = append(args, b.timeoutFlag, fmt.Sprintf("%ds", o.timeoutSeconds))
	}
	if b.modelFlag != "" && strings.TrimSpace(o.model) != "" {
		args = append(args, b.modelFlag, o.model)
	}
	if b.addDirFlag != "" {
		for _, d := range o.addDirs {
			args = append(args, b.addDirFlag, d)
		}
	}
	if o.allowTools && b.skipPermsFlag != "" {
		args = append(args, b.skipPermsFlag)
	}
	if o.sandbox && b.sandboxFlag != "" {
		args = append(args, b.sandboxFlag)
	}
	args = append(args, b.extraArgs...)
	// reasonOnlyArgs keep a CLI without a true no-tools mode read-only when
	// allow_tools is false (e.g. codex's --sandbox read-only).
	if !o.allowTools {
		args = append(args, b.reasonOnlyArgs...)
	}
	// Positional prompt goes LAST, after subcmd and every flag, preceded by the "--"
	// end-of-options marker. Without it a task that starts with a dash (e.g.
	// "--fix the bug") or matches a subcommand name (codex exec's resume/review/help)
	// is parsed as a flag/subcommand instead of the prompt — codex itself rejects it
	// with "unexpected argument ... use '-- ...'".
	if b.promptPositional {
		args = append(args, "--", o.task)
	}
	return args
}

// modeNote describes the run mode for the result header. The strings are derived
// from the backend's own flags so they stay accurate per CLI: reason-only uses
// reasonOnlyNote when set (codex is read-only, not no-tools), and the enabled note
// names the backend's actual skip-perms / sandbox flags.
func (b backend) modeNote(o runOpts) string {
	if !o.allowTools {
		if b.reasonOnlyNote != "" {
			return b.reasonOnlyNote
		}
		return "tool-use: disabled (reason/answer only)"
	}
	note := fmt.Sprintf("tool-use: ENABLED (%s)", b.skipPermsFlag)
	if o.sandbox && b.supportsSandbox() {
		note += " in " + b.sandboxFlag
	}
	return note
}

// backends is the registry and SINGLE SOURCE OF TRUTH: adding a CLI coding-agent
// is one entry here — no new code, and main() iterates it to register tools. The
// named vars below are derived from it purely for convenient test reference, so a
// new entry can never be silently forgotten.
var backends = []backend{
	{
		tool:            "gemini_agent",
		cliName:         "agy",
		binEnv:          "AGY_BIN",
		promptFlag:      "--print",
		timeoutFlag:     "--print-timeout",
		modelFlag:       "--model",
		addDirFlag:      "--add-dir",
		skipPermsFlag:   "--dangerously-skip-permissions",
		sandboxFlag:     "--sandbox",
		timeoutHeadroom: geminiTimeoutHeadroom,
		description:     geminiToolDescription,
		allowToolsDesc:  geminiAllowToolsDescription,
	},
	{
		tool:          "claude_agent",
		cliName:       "claude",
		binEnv:        "CLAUDE_BIN",
		promptFlag:    "--print",
		modelFlag:     "--model",
		addDirFlag:    "--add-dir",
		skipPermsFlag: "--dangerously-skip-permissions",
		// timeoutFlag/sandboxFlag "" — claude has neither; timeoutHeadroom 0 — deadline is the timeout.
		description:    claudeToolDescription,
		allowToolsDesc: claudeAllowToolsDescription,
	},
	{
		tool:             "codex_agent",
		cliName:          "codex",
		binEnv:           "CODEX_BIN",
		subcmd:           []string{"exec"},
		promptPositional: true,
		modelFlag:        "--model",
		addDirFlag:       "--add-dir",
		skipPermsFlag:    "--dangerously-bypass-approvals-and-sandbox",
		extraArgs:        []string{"--skip-git-repo-check", "--color", "never"},
		reasonOnlyArgs:   []string{"--sandbox", "read-only"},
		reasonOnlyNote:   "tool-use: read-only (--sandbox read-only)",
		// promptFlag/timeoutFlag/sandboxFlag "" — codex takes the prompt positionally
		// (`codex exec [flags] <prompt>`), has no internal print-timeout (timeoutHeadroom
		// 0 — the ctx deadline IS the timeout), and exposes no boolean sandbox param
		// (allow_tools toggles read-only sandbox vs. the bypass flag instead).
		description:    codexToolDescription,
		allowToolsDesc: codexAllowToolsDescription,
	},
}

// Named references into the registry, for tests. Derived from backends so they
// can't drift from what the server actually registers.
var (
	geminiBackend = backends[0]
	claudeBackend = backends[1]
	codexBackend  = backends[2]
)

// parseHopEnv reads the current delegation depth and max from a getenv-style
// lookup function. Invalid, missing, or out-of-range values fall back to the
// defaults: a negative depth → defaultHopDepth, and a max < 1 (e.g. a
// fat-fingered AGENT_HOP_MAX=0, which would otherwise refuse every call) →
// defaultHopMax. Pure function — table-testable (pass a map-backed getenv).
func parseHopEnv(getenv func(string) string) (depth, hopMax int) {
	depth = defaultHopDepth
	if v, err := strconv.Atoi(strings.TrimSpace(getenv(hopDepthEnv))); err == nil && v >= 0 {
		depth = v
	}
	hopMax = defaultHopMax
	if v, err := strconv.Atoi(strings.TrimSpace(getenv(hopMaxEnv))); err == nil && v >= 1 {
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
		"agent-bridge-mcp",
		"0.1.0",
		server.WithToolCapabilities(false),
	)

	for _, b := range backends {
		s.AddTool(newTool(b), makeHandler(b))
	}

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "agent-bridge-mcp: server error: %v\n", err)
		os.Exit(1)
	}
}

// commonToolOptions returns the tool options shared by both tools: the given
// description plus the task/add_dirs/working_dir/timeout_seconds/model/allow_tools
// params. Per-tool extras (e.g. gemini's sandbox) are appended by the caller.
// Defining the shared params once keeps the two tool schemas from drifting.
func commonToolOptions(description, allowToolsDescription string) []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithDescription(description),
		mcp.WithString("task",
			mcp.Required(),
			mcp.Description("The complete, self-contained task/prompt for the agent to perform."),
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
			mcp.Description(allowToolsDescription),
		),
	}
}

// newTool builds the MCP tool for a backend: the shared params, plus the sandbox
// option for backends that support it.
func newTool(b backend) mcp.Tool {
	opts := commonToolOptions(b.description, b.allowToolsDesc)
	if b.supportsSandbox() {
		opts = append(opts, mcp.WithBoolean("sandbox", mcp.Description(sandboxDescription)))
	}
	return mcp.NewTool(b.tool, opts...)
}

// makeHandler returns the MCP handler for a backend. The `sandbox` param is read
// from the request only for backends that support it (b.supportsSandbox()).
func makeHandler(b backend) server.ToolHandlerFunc {
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
		if b.supportsSandbox() {
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
	modeNoteStr := b.modeNote(o)

	// Give backends with their own internal timeout (gemini/agy) a little headroom
	// beyond the requested timeout so they surface their own timeout message rather
	// than us killing them first. claude has no internal timeout (headroom 0), so
	// the context deadline IS the timeout. Guard against a negative timeout (a
	// direct runAgent caller bypassing makeHandler's clamp) collapsing the deadline.
	effectiveTimeout := o.timeoutSeconds
	if effectiveTimeout < 0 {
		effectiveTimeout = 0
	}
	hardDeadline := time.Duration(effectiveTimeout)*time.Second + b.timeoutHeadroom
	runCtx, cancel := context.WithTimeout(ctx, hardDeadline)
	defer cancel()

	cmd := exec.CommandContext(runCtx, b.resolveBin(), args...)
	if strings.TrimSpace(o.workingDir) != "" {
		cmd.Dir = o.workingDir
	}
	// Spawn the child with an incremented delegation depth (no duplicate keys).
	cmd.Env = childHopEnv(os.Environ(), depth)

	// Kill the whole process group on cancel/timeout (so grandchildren the child
	// spawned die too) and bound how long Run may block on I/O afterward. Without
	// this, a surviving grandchild that inherited the stdout/stderr pipes keeps
	// them open and cmd.Run() hangs past the deadline, leaking the goroutine/fds.
	setupProcessGroup(cmd)
	cmd.WaitDelay = childWaitDelay

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
			b.tool, elapsed, modeNoteStr, truncate(stdout.String(), 8000), truncate(stderr.String(), 2000),
		)), nil
	}

	if runErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf(
			"%s failed (%s): %v\nstderr:\n%s\nstdout:\n%s",
			b.tool, modeNoteStr, runErr, truncate(stderr.String(), 4000), truncate(stdout.String(), 8000),
		)), nil
	}

	out := strings.TrimRight(stdout.String(), "\n")
	if strings.TrimSpace(out) == "" {
		out = fmt.Sprintf("(%s returned no stdout)", b.cliName)
		if se := strings.TrimSpace(stderr.String()); se != "" {
			out += "\nstderr:\n" + truncate(se, 2000)
		}
	}

	header := fmt.Sprintf("[%s | %s | %s]\n\n", b.tool, modeNoteStr, elapsed)
	return mcp.NewToolResultText(header + out), nil
}

// truncate returns a copy of s truncated to at most limit bytes, without
// splitting UTF-8 runes. A negative limit is treated as 0 (no content kept),
// guarding against an out-of-range slice panic.
func truncate(s string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s
	}
	// Back up to a valid UTF-8 rune boundary.
	// Continuation bytes start with the bits 10xxxxxx, i.e., byte & 0xC0 == 0x80.
	i := limit
	for i > 0 && (s[i]&0xC0 == 0x80) {
		i--
	}
	return s[:i] + fmt.Sprintf("\n…(truncated, %d bytes total)", len(s))
}
