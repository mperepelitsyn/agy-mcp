BINARY := agy-mcp
PKG    := github.com/adubkov/agy-mcp

MARKETPLACE := agy-gemini-local
PLUGIN      := agy-gemini

# Where the Antigravity CLI copies imported plugins. agy imports the plugin
# MANIFESTS but not the built binary, and doesn't expand Claude's
# ${CLAUDE_PLUGIN_ROOT}, so install-agy repoints the imported mcp_config.json at
# the absolute repo binary. Override AGY_PLUGIN_DIR if your agy layout differs.
AGY_PLUGIN_DIR := $(HOME)/.gemini/config/plugins/$(PLUGIN)

.PHONY: build install vet test clean smoke smoke-gemini smoke-claude install-claude uninstall-claude install-agy uninstall-agy plugin-install plugin-uninstall help

## build: compile the binary into the REPO ROOT (./agy-mcp). This is the canonical
##        artifact: the plugin's .mcp.json (${CLAUDE_PLUGIN_ROOT}/agy-mcp) and
##        `make install-claude` ($(CURDIR)/agy-mcp) both reference it.
build:
	go build -o $(BINARY) .

## install: OPTIONAL — `go install` to $GOBIN/$GOPATH/bin for standalone PATH use.
##          NOT used by the plugin or install-claude (those use the repo-dir binary
##          from `make build`). Only needed if you want `agy-mcp` on your PATH.
install:
	go install .

## vet: static checks
vet:
	go vet ./...

## test: run tests
test:
	go test ./...

## smoke: build + smoke-test BOTH tools (gemini_agent + claude_agent). Needs agy AND
##        claude authed; runs each in a clean temp dir. For one tool, use the
##        smoke-gemini / smoke-claude targets.
smoke: smoke-gemini smoke-claude
	@echo "smoke OK (gemini + claude)"

# Map each smoke-<label> target to the MCP tool it exercises.
TOOL_gemini := gemini_agent
TOOL_claude := claude_agent

## smoke-gemini: smoke-test just gemini_agent (clean temp dir; needs agy authed)
## smoke-claude: smoke-test just claude_agent (clean temp dir; needs claude authed)
smoke-gemini smoke-claude: smoke-%: build
	@mkdir -p /tmp/agy-mcp-smoke-$*
	@printf '%s\n' \
	'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
	'{"jsonrpc":"2.0","method":"notifications/initialized"}' \
	'{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"$(TOOL_$*)","arguments":{"task":"Reply with exactly the word: PONG","working_dir":"/tmp/agy-mcp-smoke-$*","timeout_seconds":120}}}' \
	| ./$(BINARY) | grep -q PONG && echo "smoke-$* OK" || (echo "smoke-$* FAILED"; exit 1)

## install-claude: register the MCP server with Claude Code (user scope) via `claude mcp add`
install-claude: build
	claude mcp add agy --scope user -- $(CURDIR)/$(BINARY)
	@echo "registered 'agy' MCP server (tools: gemini_agent + claude_agent — from Claude use gemini_agent). Restart Claude Code, then /mcp to confirm."

## uninstall-claude: remove the MCP server registration from Claude Code
uninstall-claude:
	-claude mcp remove agy --scope user
	@echo "removed 'agy' MCP server registration."

## plugin-install: register this repo as a local marketplace and install the plugin
##                  (loads BOTH the gemini-agent skill and the agy MCP server).
##                  Requires .claude-plugin/marketplace.json. Restart Claude Code after.
plugin-install: build
	-claude plugin marketplace remove $(MARKETPLACE)
	claude plugin marketplace add $(CURDIR)
	claude plugin install $(PLUGIN)@$(MARKETPLACE)
	@echo "installed $(PLUGIN)@$(MARKETPLACE) (skill: gemini-agent, MCP: agy). Restart Claude Code, then /mcp + /plugin to confirm."

## plugin-uninstall: remove the plugin and its local marketplace
plugin-uninstall:
	-claude plugin uninstall $(PLUGIN)@$(MARKETPLACE)
	-claude plugin marketplace remove $(MARKETPLACE)
	@echo "removed $(PLUGIN) and marketplace $(MARKETPLACE)."

## install-agy: register this repo's plugin with the Antigravity `agy` CLI, then
##              repoint the imported MCP server at the absolute repo binary. agy
##              copies the plugin MANIFESTS but NOT the built binary, and does not
##              expand Claude's ${CLAUDE_PLUGIN_ROOT}, so the imported
##              mcp_config.json must use an absolute command path. Restart Antigravity.
install-agy: build
	agy plugin install $(CURDIR)
	@cfg='$(AGY_PLUGIN_DIR)/mcp_config.json'; \
	if [ -f "$$cfg" ]; then \
	  sed 's#$${CLAUDE_PLUGIN_ROOT}/$(BINARY)#$(CURDIR)/$(BINARY)#' "$$cfg" > "$$cfg.tmp" && mv "$$cfg.tmp" "$$cfg" && \
	  echo "repointed agy MCP command -> $(CURDIR)/$(BINARY)"; \
	else \
	  echo "WARNING: $$cfg not found; agy plugin layout may differ. Set the MCP command to $(CURDIR)/$(BINARY) manually (or pass AGY_PLUGIN_DIR=...)."; \
	fi
	@echo "installed $(PLUGIN) into agy (MCP: agy; tools: gemini_agent + claude_agent — from agy use claude_agent). Restart Antigravity; 'agy plugin list' to confirm."

## uninstall-agy: remove this plugin's registration from the Antigravity `agy` CLI
uninstall-agy:
	-agy plugin uninstall $(PLUGIN)
	@echo "removed $(PLUGIN) from agy."

## clean: remove the built binary
clean:
	rm -f $(BINARY)

## help: list targets (one line each; see the Makefile for full descriptions)
help:
	@grep -E '^## [^ ]' $(MAKEFILE_LIST) | sed 's/## //'
