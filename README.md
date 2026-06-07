# agy-mcp

> A bidirectional **Claude ↔ Gemini** agent bridge over MCP. (The `agy-mcp` /
> `agy-gemini` names are historical — the server now drives both CLIs.)

A tiny [MCP](https://modelcontextprotocol.io) server that bridges two coding
agents in **both directions**, exposing each as a spawnable sub-agent tool. One
binary registers two tools:

- **`gemini_agent`** — shells out to the Antigravity CLI (`agy --print <task>`),
  i.e. spawns a **Gemini** sub-agent. Intended to be called from a Claude session.
- **`claude_agent`** — shells out to the Claude CLI (`claude --print <task>`),
  i.e. spawns a **Claude** sub-agent. Intended to be called from a Gemini session.

A parent agent calls a tool with a self-contained task; this server shells out to
the corresponding CLI, lets the child agent perform it, and returns the child's
full output. In effect each tool is a **spawned sub-agent callable from inside
another agent's session**.

Both tools share one backend adapter, so the run / timeout / truncation / result
header / context-cancel / loop-guard behavior is identical; they differ only in
which CLI they invoke and which CLI-specific flags they support.

## Tool: `gemini_agent`

Spawns a Gemini agent via the Antigravity `agy` CLI.

| Param | Type | Default | Notes |
|---|---|---|---|
| `task` | string (required) | — | The complete, self-contained task/prompt for Gemini. |
| `add_dirs` | string[] | — | Directories to add to the agent's workspace (absolute paths). Repeated as `--add-dir`. |
| `working_dir` | string | server cwd | Directory the agent runs in (sets `cmd.Dir`). |
| `timeout_seconds` | number | 300 (max 1800) | Maps to `agy --print-timeout`. |
| `model` | string | CLI default | Optional model; passed as `--model <model>` only when non-empty. |
| `allow_tools` | bool | **false** | Let the agent edit files in `working_dir` / run commands by auto-approving its permission prompts (`--dangerously-skip-permissions`). |
| `sandbox` | bool | **false** | Confine the agent to an isolated scratch dir (`--sandbox`). **Warning:** when true, its edits go to the scratch dir, NOT `working_dir`. Leave off for real edits. **Gemini-only** — `claude_agent` has no `sandbox` param. |

## Tool: `claude_agent`

Spawns a **Claude** agent via the `claude` CLI. This is the reverse direction:
intended to be called *from a Gemini session* so Gemini can delegate to Claude.
It mirrors `gemini_agent`'s semantics. **Note:** every run shells out to the
`claude` CLI and therefore **consumes Claude credits** — even reason-only runs.

| Param | Type | Default | Notes |
|---|---|---|---|
| `task` | string (required) | — | The complete, self-contained task/prompt for Claude. Passed as the value of `--print` (`claude --print <task>`). |
| `add_dirs` | string[] | — | Directories to add to the agent's workspace (absolute paths). Repeated as `--add-dir`. |
| `working_dir` | string | server cwd | Directory the agent runs in (sets `cmd.Dir`). |
| `timeout_seconds` | number | 300 (max 1800) | The `claude` CLI has **no** `--print-timeout`; the timeout is enforced purely by the process context deadline (no timeout flag is passed to `claude`). |
| `model` | string | CLI default | Optional model; passed as `--model <model>` only when non-empty. |
| `allow_tools` | bool | **false** | Let the agent edit files in `working_dir` / run commands by auto-approving its permission prompts (`--dangerously-skip-permissions`). This is **unattended execution that consumes Claude credits**. |

There is **no `sandbox` param** on `claude_agent` — sandboxing is Gemini-only and
`--sandbox` is never passed to `claude`.

### Safety model (both tools)

By default the spawned agent is **reason/answer only** — it runs `--print` with no
permission bypass, so it can analyze, draft, and answer but cannot take unattended
actions. To let it actually act on your files/system, the caller must explicitly
pass `allow_tools: true`, which passes `--dangerously-skip-permissions` to the
underlying CLI (the child's approval gates are off — this is unattended
execution). Scope it with `working_dir`; the agent's edits land there.

For `gemini_agent`, `--sandbox` is **off by default**: with it on, `agy` confines
the agent to an isolated scratch dir, so edits would *not* reach `working_dir`.
Set `sandbox: true` only for a confined "compute but don't touch my files" run.
`claude_agent` has no sandbox concept.

The tool result header always reports which tool ran, the mode, and the elapsed
time: `[<tool> | <modeNote> | <elapsed>]`.

### Loop guard (`AGENT_HOP_DEPTH` / `AGENT_HOP_MAX`)

Because the two tools can call each other (Claude → Gemini → Claude → …), the
shared run path enforces a delegation-depth limit to prevent runaway A→B→A→B
chains. It reads two environment variables:

| Env var | Default | Meaning |
|---|---|---|
| `AGENT_HOP_DEPTH` | `0` | Current delegation depth. |
| `AGENT_HOP_MAX` | `2` | Maximum allowed depth. |

On each call:

- If the current depth has **reached the max** (`depth >= max`), the tool returns
  an **MCP error result** explaining the delegation-depth limit was reached and
  does **not** spawn a child. The parent agent should perform the task itself.
- Otherwise the child is spawned with `AGENT_HOP_DEPTH` set to `depth + 1` (the
  server rebuilds the child's environment from its own, removing any existing
  `AGENT_HOP_DEPTH` entry so there are no duplicate keys).

Set `AGENT_HOP_MAX` in the MCP server's environment to allow deeper (or shallower)
delegation chains. Invalid/missing values fall back to the defaults above.

## Build

```sh
go build -o agy-mcp .          # local binary
# or
go install github.com/adubkov/agy-mcp@latest
```

Each tool requires its CLI:

- `gemini_agent` needs `agy` on `PATH` (or set `AGY_BIN=/path/to/agy`); the server
  falls back to `~/.local/bin/agy`, then `agy`.
- `claude_agent` needs `claude` on `PATH` (or set `CLAUDE_BIN=/path/to/claude`);
  the server falls back to `~/.local/bin/claude`, then `claude`.

You only need the CLI for the tool you actually call.

## Install into Claude Code

Use this when the **parent** is Claude Code (so Claude can delegate to Gemini via
`gemini_agent`). Two ways — pick one. **Either way, requires `agy` authenticated**
(`agy` login once) and on `PATH` (or set `AGY_BIN`; the server also falls back to
`~/.local/bin/agy`). Restart Claude Code afterward (MCP loads at session start);
run `/mcp` to confirm the `agy` server is connected. The tools appear as
`gemini_agent` and `claude_agent`.

### A) MCP server only — `make install-claude` (simplest)

Registers just the tools (user scope, available in every project):

```sh
make install-claude     # build + `claude mcp add agy --scope user -- <binary>`
# remove later with:
make uninstall-claude
```

Equivalent manual command:

```sh
claude mcp add agy --scope user -- "$(pwd)/agy-mcp"
```

Or project scope via `.mcp.json` in a repo root:

```json
{
  "mcpServers": {
    "agy": {
      "command": "/absolute/path/to/agy-mcp/agy-mcp",
      "env": { "AGY_BIN": "/Users/you/.local/bin/agy" }
    }
  }
}
```

### B) As a plugin — `make plugin-install` (tool + skill)

This repo is also a Claude Code **plugin** (`agy-gemini`): installing it wires the
MCP server *and* ships a skill (`skills/gemini-agent/SKILL.md`) that teaches Claude
when and how to delegate to `gemini_agent` (and to verify its output).

Claude Code discovers plugins through **marketplaces**, not by scanning a
directory — so this repo carries a single-plugin local marketplace
(`.claude-plugin/marketplace.json`). The target registers that marketplace and
installs the plugin from it:

```sh
make plugin-install     # build + marketplace add (this repo) + plugin install
# then restart Claude Code; run /plugin and /mcp to confirm
# remove later with:
make plugin-uninstall
```

Equivalent manual commands:

```sh
claude plugin marketplace add "$(pwd)"
claude plugin install agy-gemini@agy-gemini-local
```

> The marketplace records this repo's **absolute path** in your user settings, so
> this is a local-dev install tied to your checkout location. To share it, point a
> marketplace at the GitHub repo instead of the local path.

## Install into Antigravity (agy)

Use this when the **parent** is Antigravity/Gemini (so Gemini can delegate to
Claude via `claude_agent`). The Antigravity CLI manages plugins with `agy plugin`
(run `agy plugin help` to see the subcommands). Because this repo is a
Claude-format plugin (`.claude-plugin/plugin.json`), the documented way to bring it
into agy is `agy plugin install <plugin-dir>`, which reads the plugin manifest and
imports its skill + MCP server. **Requires `claude` authenticated** and on `PATH`
(or set `CLAUDE_BIN`; the server also falls back to `~/.local/bin/claude`).

```sh
make build                              # build the ./agy-mcp binary first
agy plugin install "$(pwd)"             # install this plugin dir into agy
agy plugin list                         # confirm it's imported (source: claude-code)
# remove later with:
agy plugin uninstall agy-gemini
```

Installing reports the components it picked up, e.g.:

```
[ok]  agy-gemini
      ✔ skills      : 1 processed
      ✔ mcpServers  : 1 processed
```

The MCP server it registers is the same `agy-mcp` binary, so **both**
`gemini_agent` and `claude_agent` become available inside agy; from a Gemini
session you'll typically call `claude_agent`.

> **Alternatives (documented agy subcommands):**
> - `agy plugin import [gemini|claude]` imports plugins/extensions that are
>   *already installed* in the Gemini CLI or Claude Code into agy. So if you first
>   run the Claude Code plugin install above (option B), `agy plugin import claude`
>   will pull it in. With nothing installed it prints `No claude extensions found.`
> - `agy plugin install <plugin@marketplace>` is supported too, but it resolves the
>   marketplace from **agy's** registered marketplaces — the Claude Code local
>   marketplace (`agy-gemini-local`) is not one of those, so use the **plugin-dir
>   path** form shown above rather than `agy-gemini@agy-gemini-local`.
>
> If your `agy` version behaves differently, run `agy plugin help` and
> `agy plugin <command>` (with no args) to see the exact usage for your build.

The plugin bundles:

- `.claude-plugin/plugin.json` — plugin manifest.
- `.claude-plugin/marketplace.json` — single-plugin local marketplace
  (`agy-gemini-local`) so `claude plugin marketplace add` can find it.
- `.mcp.json` — registers the `agy` MCP server (`${CLAUDE_PLUGIN_ROOT}/agy-mcp`).
- `skills/gemini-agent/SKILL.md` — guidance for Claude on delegating tasks
  (when to use it, the two modes, how to write a good `task`, and "always verify
  the output").

## Build (Makefile)

```sh
make build         # compile ./agy-mcp (referenced by .mcp.json)
make install       # go install into $GOBIN
make vet           # static checks
make smoke         # reason-only round-trip against BOTH tools (needs agy + claude authed)
make smoke-gemini  # round-trip against gemini_agent only (needs agy authed)
make smoke-claude  # round-trip against claude_agent only (needs claude authed)
make help          # list targets
```

## Example calls

### `gemini_agent`

Reason-only (safe default):

```json
{ "task": "Review this Go error-handling pattern and suggest improvements: ..." }
```

Acting mode — let Gemini edit files (auto-approves its permission prompts; scope
it with `working_dir` and verify the diff afterward):

```json
{
  "task": "Rename the symbol Foo to Bar across this package and update callers. Make the edits and list the files you changed.",
  "working_dir": "/path/to/project",
  "add_dirs": ["/path/to/project"],
  "allow_tools": true
}
```

### `claude_agent`

Reason-only (safe default — still consumes Claude credits):

```json
{ "task": "Review this Go error-handling pattern and suggest improvements: ..." }
```

Acting mode — let Claude edit files (unattended; scope it with `working_dir`):

```json
{
  "task": "Rename the symbol Foo to Bar across this package and update callers. Make the edits and list the files you changed.",
  "working_dir": "/path/to/project",
  "add_dirs": ["/path/to/project"],
  "model": "sonnet",
  "allow_tools": true
}
```
