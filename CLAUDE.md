# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

MCP SSH Server is a pure Go Model Context Protocol server that provides AI agents with persistent SSH shell sessions to remote hosts. Key feature: environment and directory state persists across multiple command executions within the same connection.

**Prerequisites:** Go 1.25+

## Common Commands

```bash
# Build
make build

# Run tests
make test

# Run tests with race detector
make test-race

# Lint (requires golangci-lint)
make lint

# Format code
make fmt

# Run all checks (fmt, vet, lint, test)
make check

# Cross-compile for multiple platforms
make build-all

# Run locally
make run

# Release (requires goreleaser)
make release-dry-run
```

## Architecture

```
main.go → cmd/root.go → pkg/mcp/handlers.go ↔ pkg/ssh/
              (Cobra)     (MCP tool handlers)   ├── manager.go   (connection lifecycle)
                                                  ├── executor.go (persistent shell sessions)
                                                  └── validator.go (host allowlist)
```

### Key Components

- **pkg/ssh/executor.go**: Manages persistent shell sessions using delimiter-based output capture (`__MCP_SSH_END_<nonce>__`). Commands are sent with an echo delimiter to parse exit codes.
- **pkg/ssh/manager.go**: Thread-safe connection pool using RWMutex. Stores connections by user-provided connection_id.
- **pkg/ssh/validator.go**: Host allowlist with glob pattern support (e.g., `192.168.1.*,*.example.com`).
- **pkg/mcp/handlers.go**: Bridges MCP protocol to SSH operations (ssh_connect, ssh_execute, ssh_close, ssh_list).

### Security Notes

- Host key verification is disabled (`InsecureIgnoreHostKey()`) - see ARCHITECTURE.md security section before production use
- Always use `--allowed-hosts` flag to restrict access
- Passwords/keys are handled in memory only and never logged
