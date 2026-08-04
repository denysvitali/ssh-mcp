# MCP SSH Server Architecture

## Overview

This is a Model Context Protocol (MCP) server written in pure Go that enables AI agents to establish and manage SSH connections to remote hosts. The key feature is persistent shell sessions that maintain environment variables and working directory state across multiple command executions.

## Design Principles

1. **Pure Go Implementation**: No external executables or shell dependencies
2. **Persistent Shell Sessions**: Single shell session per connection maintains state
3. **Thread-Safe Operations**: All connection operations are mutex-protected
4. **Security by Default**: Host allowlist with glob pattern support
5. **Stdio Transport**: Standard MCP stdio transport for easy integration

## Component Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                         main.go                              │
│  - MCP Server Setup                                          │
│  - Tool Registration (ssh_connect, ssh_execute, etc.)       │
│  - Signal Handling & Graceful Shutdown                       │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                      cmd/root.go                             │
│  - Cobra CLI Configuration                                   │
│  - Flag Parsing (--allowed-hosts, --log-level, etc.)        │
│  - Logger Setup                                              │
└──────────────────────────┬──────────────────────────────────┘
                           │
        ┌──────────────────┴──────────────────┐
        ▼                                     ▼
┌──────────────────┐              ┌────────────────────────┐
│ pkg/mcp/         │              │ pkg/ssh/               │
│ handlers.go      │──────────────│ manager.go             │
│                  │              │                        │
│ - HandleConnect  │              │ - Connect()            │
│ - HandleExecute  │              │ - Execute()            │
│ - HandleClose    │              │ - Close()              │
│ - HandleList     │              │ - List()               │
└──────────────────┘              │ - CloseAll()           │
                                  └────────┬───────────────┘
                                           │
                        ┌──────────────────┴──────────────────┐
                        ▼                                     ▼
                ┌──────────────────┐              ┌────────────────────┐
                │ pkg/ssh/         │              │ pkg/ssh/           │
                │ validator.go     │              │ executor.go        │
                │                  │              │                    │
                │ - Validate()     │              │ - Execute()        │
                │ - Glob Matching  │              │ - Persistent Shell │
                └──────────────────┘              │ - Delimiter-based  │
                                                  │   output capture   │
                                                  └────────────────────┘
```

## Key Components

### 1. Host Validator (`pkg/ssh/validator.go`)

**Purpose**: Validates SSH connection requests against allowed host patterns.

**Features**:
- Glob pattern matching (e.g., `*.example.com`, `192.168.1.*`)
- Comma-separated pattern lists
- Thread-safe validation

**Example**:
```go
validator, err := ssh.NewHostValidator("192.168.1.*,*.example.com")
err := validator.Validate("192.168.1.100") // nil (allowed)
err := validator.Validate("10.0.0.1")      // error (not allowed)
```

### 2. Shell Executor (`pkg/ssh/executor.go`)

**Purpose**: Manages persistent shell sessions for command execution.

**How It Works**:

1. **Session Creation**:
   - Creates SSH session with stdin/stdout/stderr pipes
   - Starts interactive shell (`/bin/bash`)
   - Drains initial shell output (prompt, etc.)

2. **Command Execution**:
   ```
   User Command: export FOO=bar

   Sent to Shell:
   ┌─────────────────────────────────────┐
   │ export FOO=bar                      │
   │ echo "__MCP_SSH_END_123456__:$?"    │
   └─────────────────────────────────────┘

   Output Parsing:
   ┌─────────────────────────────────────┐
   │ [any command output]                │
   │ __MCP_SSH_END_123456__:0            │ ← Delimiter + Exit Code
   └─────────────────────────────────────┘

   Returned to Client:
   {
     "stdout": "[any command output]",
     "stderr": "",
     "exit_code": 0
   }
   ```

3. **State Persistence**:
   - Same shell session = same environment
   - Environment variables persist
   - Working directory persists
   - Shell history available

**Key Methods**:
- `NewShellExecutor(client)`: Creates persistent shell
- `Execute(command)`: Executes command and captures output
- `Close()`: Terminates shell session

### 3. SSH Manager (`pkg/ssh/manager.go`)

**Purpose**: Manages multiple SSH connections and their lifecycles.

**Features**:
- Connection pooling with unique IDs
- Thread-safe operations (RWMutex)
- Host validation before connection
- Automatic cleanup on shutdown
- Local credential discovery (see Auth Resolver below)

**Connection Lifecycle**:
```
Connect() → [Validate Host] → [Build Auth] → [SSH Dial] → [Create Shell] → [Store Connection]
                                                                       │
                                                                       ▼
Execute() → [Lookup Connection] → [executor.Execute()] → [Return Result]
                                                                       │
                                                                       ▼
Close() → [Lookup Connection] → [Close Shell] → [Close Client] → [Remove]
```

**Data Structures**:
```go
type Connection struct {
    Info     ConnectionInfo  // ID, Host, Port, Username, Created
    client   *ssh.Client     // SSH client
    executor *ShellExecutor  // Persistent shell executor
}

type Manager struct {
    connections map[string]*Connection  // ID → Connection
    validator   *HostValidator          // Host validator
    mu          sync.RWMutex            // Thread safety
}
```

### 3b. Auth Resolver (`pkg/ssh/auth.go`)

**Purpose**: Builds the `[]ssh.AuthMethod` list for a connection from explicit
input plus whatever credentials exist on the local machine.

**Resolution order**:
```
1. SSH agent            (SSH_AUTH_SOCK, when use_agent is enabled and the socket dials)
2. Explicit private key (private_key_path / identity_file) — hard error if unusable
   ...otherwise...
   Discovered keys      (~/.ssh/id_ed25519, id_ecdsa, id_rsa, id_dsa) — unusable keys skipped
3. Password             (when provided)
```

An unreachable agent socket is a no-op, not an error. `password` doubles as the
passphrase for encrypted keys. If the list ends up empty, `Connect` fails with a
message describing all the available options.

**Not supported**: `~/.ssh/config` parsing (would require a third-party
dependency such as `kevinburke/ssh_config`).

### 4. MCP Handlers (`pkg/mcp/handlers.go`)

**Purpose**: Bridges MCP protocol to SSH operations.

**Tools Implemented**:

1. **ssh_connect**: Establishes SSH connection
   - Validates host against allowlist
   - Supports password, explicit key, SSH agent, and ~/.ssh default keys
   - Credentials are optional; falls back to agent/local keys
   - Creates persistent shell session
   - Returns connection info

2. **ssh_execute**: Executes command on connection
   - Looks up connection by ID
   - Executes in persistent shell
   - Returns stdout, stderr, exit code

3. **ssh_close**: Closes SSH connection
   - Gracefully terminates shell
   - Closes SSH client
   - Removes from connection pool

4. **ssh_list**: Lists active connections
   - Returns all connection metadata
   - Includes ID, host, port, username, created timestamp

### 5. Main Server (`main.go`)

**Purpose**: MCP server initialization and orchestration.

**Responsibilities**:
- Parse CLI flags via Cobra
- Initialize host validator
- Create SSH manager
- Register MCP tools
- Handle graceful shutdown
- Start stdio transport

**Tool Definitions**:
```go
ssh_connect:
  - connection_id (required)
  - host (required)
  - port (optional, default: 22)
  - username (required)
  - password (optional, also used as private key passphrase)
  - private_key_path (optional)
  - identity_file (optional, alias for private_key_path)
  - use_agent (optional, default: true when SSH_AUTH_SOCK is set)
  # With no explicit credentials: SSH agent, then ~/.ssh default keys
  # (id_ed25519, id_ecdsa, id_rsa, id_dsa). ~/.ssh/config is not parsed.

ssh_execute:
  - connection_id (required)
  - command (required)

ssh_close:
  - connection_id (required)

ssh_list:
  - (no parameters)
```

## Security Considerations

### Implemented

1. **Host Allowlist**: Mandatory `--allowed-hosts` flag with glob patterns
2. **No Arbitrary Execution**: All commands run in controlled SSH sessions
3. **Private Key Security**: Keys read from files, not passed as strings
4. **Credential Isolation**: Passwords/keys not logged

### TODO for Production

1. **Host Key Verification**: Currently uses `InsecureIgnoreHostKey()`
   - Should implement proper host key verification
   - Store known_hosts file
   - Validate host keys on connection

2. **Encrypted Private Keys**: Support passphrase-protected keys

3. **Rate Limiting**: Prevent connection spam

4. **Audit Logging**: Enhanced logging for security events

## Data Flow Example

### Scenario: Setting and Reading Environment Variable

```
1. Client → MCP Server
   Tool: ssh_connect
   {
     "connection_id": "webserver",
     "host": "192.168.1.100",
     "username": "admin",
     "password": "secret"
   }

2. MCP Server Processing
   - Validate host: "192.168.1.100" ✓
   - SSH Dial: 192.168.1.100:22 ✓
   - Create Shell Session ✓
   - Store in Manager: connections["webserver"] = Connection{...}

3. Client ← MCP Server
   {
     "success": true,
     "connection_id": "webserver",
     "message": "SSH connection established successfully"
   }

4. Client → MCP Server
   Tool: ssh_execute
   {
     "connection_id": "webserver",
     "command": "export FOO=bar"
   }

5. MCP Server Processing
   - Lookup: connections["webserver"] → Connection
   - Execute in persistent shell:
     ┌─────────────────────────────┐
     │ export FOO=bar              │
     │ echo "__END_123__:$?"       │
     └─────────────────────────────┘
   - Parse output: exit_code = 0

6. Client ← MCP Server
   {
     "success": true,
     "stdout": "",
     "stderr": "",
     "exit_code": 0
   }

7. Client → MCP Server
   Tool: ssh_execute
   {
     "connection_id": "webserver",
     "command": "echo $FOO"
   }

8. MCP Server Processing
   - Lookup: connections["webserver"] → Connection (same shell!)
   - Execute in persistent shell:
     ┌─────────────────────────────┐
     │ echo $FOO                   │
     │ echo "__END_456__:$?"       │
     └─────────────────────────────┘
   - Parse output:
     ┌─────────────────────────────┐
     │ bar                         │
     │ __END_456__:0               │
     └─────────────────────────────┘
   - Extract: stdout = "bar", exit_code = 0

9. Client ← MCP Server
   {
     "success": true,
     "stdout": "bar",           ← Environment persisted!
     "stderr": "",
     "exit_code": 0
   }
```

## Dependencies

### Core Libraries

- `golang.org/x/crypto/ssh`: Pure Go SSH client
- `github.com/mark3labs/mcp-go`: MCP protocol implementation
- `github.com/spf13/cobra`: CLI framework
- `github.com/sirupsen/logrus`: Structured logging
- `github.com/gobwas/glob`: Glob pattern matching
- `github.com/charmbracelet/lipgloss`: Terminal styling

### Why These Choices?

1. **golang.org/x/crypto/ssh**: Official Go SSH package, pure Go, no cgo
2. **mcp-go**: Clean MCP abstraction, stdio transport built-in
3. **cobra**: Industry standard CLI framework, great UX
4. **logrus**: Structured logging, field support, multiple outputs
5. **gobwas/glob**: Fast glob matching, no regex overhead
6. **lipgloss**: Beautiful terminal output, ANSI styling

## Performance Characteristics

### Memory

- **Base overhead**: ~10MB per server instance
- **Per connection**: ~2-5MB (SSH client + buffers)
- **Connection limit**: Configurable, default unlimited

### Latency

- **Connection establishment**: 100-500ms (network + SSH handshake)
- **Command execution**: 50-200ms (network RTT + shell processing)
- **Persistent shell overhead**: <1ms (in-memory state)

### Concurrency

- **Thread safety**: All manager operations mutex-protected
- **Concurrent connections**: Limited by system resources
- **Concurrent commands**: One per connection (sequential within shell)

## Testing Strategy

### Manual Testing

```bash
# Start server
./mcp-ssh --allowed-hosts "localhost" --log-level debug

# Send JSON-RPC via stdin
echo '{"jsonrpc":"2.0","id":1,"method":"initialize",...}' | ./mcp-ssh ...
```

### Integration Testing

Use MCP Inspector:
```bash
npm install -g @modelcontextprotocol/inspector
mcp-inspector ./mcp-ssh --allowed-hosts "localhost"
```

### Unit Tests (Future)

- `validator_test.go`: Glob pattern matching
- `executor_test.go`: Command execution, delimiter parsing
- `manager_test.go`: Connection lifecycle
- `handlers_test.go`: MCP tool handlers

## Deployment

### Claude Desktop

```json
{
  "mcpServers": {
    "ssh": {
      "command": "/path/to/mcp-ssh",
      "args": ["--allowed-hosts", "192.168.1.*,*.example.com"],
      "env": {
        "LOG_LEVEL": "info"
      }
    }
  }
}
```

## Future Enhancements

1. **Host Key Verification**: Proper SSH host key checking
2. **Connection Pooling**: Reuse connections across requests
3. **Command Timeout**: Per-command execution timeout
4. **Metrics**: Prometheus metrics export
5. **File Transfer**: SCP/SFTP support
6. **Port Forwarding**: SSH tunnel management
7. **Jump Hosts**: Multi-hop SSH connections
8. **Session Recording**: Audit trail of all commands
