package ssh

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// Shell initialization timeouts
	shellInitialDrainDelay = 500 * time.Millisecond
	shellInitCommandDelay  = 200 * time.Millisecond
	shellPromptTimeout     = 2 * time.Second

	// Default command execution timeout
	DefaultCommandTimeout = 30 * time.Second

	// Read timeouts
	stderrReadTimeout = 100 * time.Millisecond
	pollInterval      = 10 * time.Millisecond

	// Binary detection
	binarySampleSize = 8192
	binaryThreshold  = 0.30
)

// ExecuteOptions controls command execution behavior
type ExecuteOptions struct {
	MaxLines      int  // Maximum output lines (0 = unlimited)
	MaxBytes      int  // Maximum output bytes (0 = unlimited)
	UseLoginShell bool // If true, use login shell (-l) to source profiles
	EnablePTY     bool // If true, allocate PTY for interactive apps
	PtyCols       uint // PTY columns (default: 80)
	PtyRows       uint // PTY rows (default: 24)
}

// CommandResult represents the result of a command execution
type CommandResult struct {
	Stdout       string
	Stderr       string
	ExitCode     int
	Signal       int    // Signal number if process was killed by signal (0 if not)
	SignalName   string // Signal name e.g., "SIGSEGV", "SIGTERM"
	TimedOut     bool   // True if command timed out
	BinaryOutput bool   // True if output was binary and truncated
}

// SignalError represents a process killed by signal
type SignalError struct {
	Signal   int
	Name     string
	ExitCode int
}

func (e *SignalError) Error() string {
	return fmt.Sprintf("process terminated by signal %d (%s)", e.Signal, e.Name)
}

// ShellExecutor manages a persistent shell session for executing commands
type ShellExecutor struct {
	session        *ssh.Session
	stdin          io.WriteCloser
	stdout         *bufio.Reader
	stderr         *bufio.Reader
	mu             sync.Mutex
	commandTimeout time.Duration
	isPty          bool
	ptyCols        uint
	ptyRows        uint
}

// NewShellExecutor creates a new persistent shell executor
func NewShellExecutor(client *ssh.Client, timeout time.Duration) (*ShellExecutor, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// Set a reasonable timeout for the session
	//nolint:errcheck // Non-fatal error, TERM variable is optional
	_ = session.Setenv("TERM", "dumb")

	// Get stdin pipe
	stdin, err := session.StdinPipe()
	if err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	// Get stdout pipe
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	// Get stderr pipe
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	// Start shell
	if err := session.Shell(); err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to start shell: %w", err)
	}

	executor := &ShellExecutor{
		session:        session,
		stdin:          stdin,
		stdout:         bufio.NewReader(stdoutPipe),
		stderr:         bufio.NewReader(stderrPipe),
		commandTimeout: timeout,
	}

	// Wait for initial shell output
	time.Sleep(shellInitialDrainDelay)
	executor.drainOutput()

	// Try to set up the shell for clean output
	// Use a simpler approach that's more compatible with different shells
	initCommands := []string{
		"stty -echo 2>/dev/null || true",           // Disable echo if available
		"export PS1='' 2>/dev/null || true",        // Empty prompt if available
		"unset PROMPT_COMMAND 2>/dev/null || true", // Disable prompt command
		"set +o vi 2>/dev/null || true",            // Disable vi mode
	}

	// Execute init commands one by one to handle failures gracefully
	for _, cmd := range initCommands {
		if _, err := stdin.Write([]byte(cmd + "\n")); err != nil {
			//nolint:errcheck // Best effort cleanup
			_ = session.Close()
			return nil, fmt.Errorf("failed to initialize shell: %w", err)
		}
		time.Sleep(shellInitCommandDelay)
		executor.drainOutput()
	}

	// Wait for shell to be ready by sending a simple command and waiting for response
	readyDelim := "__MCP_READY__"
	if _, err := stdin.Write([]byte("echo " + readyDelim + "\n")); err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to send ready check: %w", err)
	}

	// Wait for the ready response with timeout
	readyChan := make(chan string, 1)
	go func() {
		var output strings.Builder
		deadline := time.Now().Add(shellPromptTimeout)
		for time.Now().Before(deadline) {
			line, err := executor.stdout.ReadString('\n')
			if err != nil {
				break
			}
			output.WriteString(line)
			if strings.Contains(line, readyDelim) {
				readyChan <- output.String()
				return
			}
		}
		readyChan <- ""
	}()

	// Don't block on ready check - just drain and continue
	executor.drainOutput()

	return executor, nil
}

// Execute runs a command in the persistent shell and returns the result
// Deprecated: Use ExecuteWithOptions instead
func (e *ShellExecutor) Execute(command string) (*CommandResult, error) {
	return e.ExecuteWithOptions(command, ExecuteOptions{})
}

// allocatePty allocates a PTY for the session if not already allocated
func (e *ShellExecutor) allocatePty(opts ExecuteOptions) error {
	if !opts.EnablePTY || e.isPty {
		return nil
	}

	cols := opts.PtyCols
	if cols == 0 {
		cols = 80
	}
	rows := opts.PtyRows
	if rows == 0 {
		rows = 24
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}
	if err := e.session.RequestPty("xterm-256color", int(cols), int(rows), modes); err != nil {
		return fmt.Errorf("failed to request PTY: %w", err)
	}
	e.isPty = true
	e.ptyCols = cols
	e.ptyRows = rows
	return nil
}

// prepareCommand wraps the command with delimiter and exit code capture
func prepareCommand(command, delimiter string) string {
	return fmt.Sprintf("%s\necho \"%s:$?\"\n", command, delimiter)
}

// ExecuteWithOptions runs a command in the persistent shell with additional options
func (e *ShellExecutor) ExecuteWithOptions(command string, opts ExecuteOptions) (*CommandResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.allocatePty(opts); err != nil {
		return nil, err
	}

	if opts.UseLoginShell {
		command = "bash -l -c " + escapeForShell(command)
	}

	delimiter := fmt.Sprintf("__MCP_SSH_END_%d__", time.Now().UnixNano())

	if strings.Contains(command, "__MCP_SSH_END_") {
		return nil, fmt.Errorf("command contains forbidden delimiter pattern '__MCP_SSH_END_'")
	}

	fullCommand := prepareCommand(command, delimiter)

	if _, err := e.stdin.Write([]byte(fullCommand)); err != nil {
		return nil, fmt.Errorf("failed to write command: %w", err)
	}

	return e.collectOutput(delimiter, opts)
}

// stdoutResult holds stdout reading results
type stdoutResult struct {
	output   string
	exitCode int
	err      error
}

// stderrResult holds stderr reading results
type stderrResult struct {
	output string
	err    error
}

// collectOutput reads stdout and stderr until command completion
func (e *ShellExecutor) collectOutput(delimiter string, opts ExecuteOptions) (*CommandResult, error) {
	stdoutChan := make(chan stdoutResult, 1)
	stderrChan := make(chan stderrResult, 1)
	doneChan := make(chan struct{})

	go func() {
		output, code, err := e.readUntilDelimiter(e.stdout, delimiter)
		stdoutChan <- stdoutResult{output: output, exitCode: code, err: err}
		close(doneChan)
	}()

	go func() {
		output, err := e.readStderrUntilDone(e.stderr, doneChan)
		stderrChan <- stderrResult{output: output, err: err}
	}()

	timeout := time.After(e.commandTimeout)

	var stdoutRes stdoutResult
	var stderrRes stderrResult
	var stdoutReceived, stderrReceived bool

	for !stdoutReceived || !stderrReceived {
		select {
		case res := <-stdoutChan:
			stdoutRes = res
			stdoutReceived = true
		case res := <-stderrChan:
			stderrRes = res
			stderrReceived = true
		case <-timeout:
			return &CommandResult{
				Stdout:   "",
				Stderr:   "command execution timed out",
				ExitCode: -1,
				TimedOut: true,
			}, nil
		}
	}

	return e.processOutput(stdoutRes, stderrRes, opts)
}

// processOutput handles output processing including signal errors, binary detection, and truncation
func (e *ShellExecutor) processOutput(stdoutRes stdoutResult, stderrRes stderrResult, opts ExecuteOptions) (*CommandResult, error) {
	stdoutStr := strings.TrimSpace(stdoutRes.output)
	stderrStr := strings.TrimSpace(stderrRes.output)

	if stdoutRes.err != nil {
		if sigErr, ok := stdoutRes.err.(*SignalError); ok {
			return &CommandResult{
				Stdout:     stdoutStr,
				Stderr:     stderrStr,
				ExitCode:   sigErr.ExitCode,
				Signal:     sigErr.Signal,
				SignalName: sigErr.Name,
			}, nil
		}
		return nil, fmt.Errorf("stdout read error: %w", stdoutRes.err)
	}
	if stderrRes.err != nil {
		return nil, fmt.Errorf("stderr read error: %w", stderrRes.err)
	}

	binaryOutput := false
	if opts.MaxBytes > 0 && len(stdoutStr) > opts.MaxBytes {
		sampleSize := min(opts.MaxBytes, binarySampleSize)
		if detectBinary([]byte(stdoutStr[:sampleSize])) {
			binaryOutput = true
		}
	}

	stdoutStr = truncateOutput(stdoutStr, opts.MaxLines, opts.MaxBytes)

	return &CommandResult{
		Stdout:       stdoutStr,
		Stderr:       stderrStr,
		ExitCode:     stdoutRes.exitCode,
		BinaryOutput: binaryOutput,
	}, nil
}

// escapeForShell wraps a command string in single quotes for safe shell execution
func escapeForShell(cmd string) string {
	return "'" + strings.ReplaceAll(cmd, "'", "'\\''") + "'"
}

// readUntilDelimiter reads from stdout until it finds the delimiter
func (e *ShellExecutor) readUntilDelimiter(reader *bufio.Reader, delimiter string) (string, int, error) {
	var output strings.Builder
	var exitCode int

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return output.String(), exitCode, nil
			}
			// Check if this is an ExitError (process killed by signal)
			if exitErr, ok := err.(*ssh.ExitError); ok {
				signal, name := mapExitErrorToSignal(exitErr.ExitStatus())
				return output.String(), exitErr.ExitStatus(), &SignalError{
					Signal:   signal,
					Name:     name,
					ExitCode: exitErr.ExitStatus(),
				}
			}
			return "", 0, err
		}

		// Check if this line contains the delimiter
		if strings.Contains(line, delimiter) {
			// Extract exit code from delimiter line (format: __DELIMITER__:123)
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				//nolint:errcheck // Exit code parsing failure is handled by returning 0
				_, _ = fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &exitCode)
			}
			return output.String(), exitCode, nil
		}

		output.WriteString(line)
	}
}

// readStderrUntilDone reads stderr until the done channel is closed, then does a final drain
func (e *ShellExecutor) readStderrUntilDone(reader *bufio.Reader, done <-chan struct{}) (string, error) {
	var output strings.Builder

	// Read stderr while command is running
	for {
		select {
		case <-done:
			// Command completed, do final drain with timeout
			deadline := time.Now().Add(stderrReadTimeout)
			for time.Now().Before(deadline) {
				if reader.Buffered() > 0 {
					line, err := reader.ReadString('\n')
					if err != nil && err != io.EOF {
						return output.String(), err
					}
					output.WriteString(line)
					if err == io.EOF {
						return output.String(), nil
					}
				} else {
					time.Sleep(pollInterval)
				}
			}
			return output.String(), nil
		default:
			// Check if data is available
			if reader.Buffered() > 0 {
				line, err := reader.ReadString('\n')
				if err != nil && err != io.EOF {
					return "", err
				}
				output.WriteString(line)
				if err == io.EOF {
					return output.String(), nil
				}
			} else {
				time.Sleep(pollInterval)
			}
		}
	}
}

// drainOutput drains any pending output from stdout and stderr
func (e *ShellExecutor) drainOutput() {
	// Non-blocking drain
	for e.stdout.Buffered() > 0 {
		//nolint:errcheck // Best effort drain, errors ignored
		_, _ = e.stdout.ReadString('\n')
	}
	for e.stderr.Buffered() > 0 {
		//nolint:errcheck // Best effort drain, errors ignored
		_, _ = e.stderr.ReadString('\n')
	}
}

// isPrintable checks if a byte is printable ASCII
func isPrintable(b byte) bool {
	return (b >= 0x20 && b <= 0x7e) || b == '\t' || b == '\n' || b == '\r'
}

// detectBinary checks if data appears to be binary based on null bytes
// and non-printable character ratio
func detectBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	// Check for null bytes (definitive binary marker)
	if bytes.Contains(data, []byte{0x00}) {
		return true
	}

	// Check ratio of non-printable characters
	nonPrintable := 0
	for _, b := range data {
		if !isPrintable(b) {
			nonPrintable++
		}
	}

	return float64(nonPrintable)/float64(len(data)) > binaryThreshold
}

// truncateOutput truncates output according to MaxLines and MaxBytes
func truncateOutput(output string, maxLines, maxBytes int) string {
	if maxLines <= 0 && maxBytes <= 0 {
		return output
	}

	// Apply line limit first
	if maxLines > 0 {
		lines := strings.Split(output, "\n")
		if len(lines) > maxLines {
			output = strings.Join(lines[:maxLines], "\n")
			output += "\n... output truncated (max lines exceeded)"
		}
	}

	// Apply byte limit
	if maxBytes > 0 && len(output) > maxBytes {
		output = output[:maxBytes]
		output += "\n... output truncated (max bytes exceeded)"
	}

	return output
}

// mapExitErrorToSignal maps an exit code to signal information
func mapExitErrorToSignal(code int) (int, string) {
	switch code {
	case 137:
		return 9, "SIGKILL"
	case 139:
		return 11, "SIGSEGV"
	case 143:
		return 15, "SIGTERM"
	case 141:
		return 13, "SIGPIPE"
	case 136:
		return 8, "SIGFPE"
	case 134:
		return 6, "SIGABRT"
	case 135:
		return 7, "SIGBUS"
	case 140:
		return 12, "SIGSYS"
	default:
		// If exit code > 128, likely a signal
		if code > 128 {
			return code - 128, fmt.Sprintf("SIGUNKNOWN(%d)", code-128)
		}
		return 0, ""
	}
}

// Close closes the shell executor
func (e *ShellExecutor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.stdin != nil {
		//nolint:errcheck // Best effort cleanup
		_ = e.stdin.Close()
	}

	if e.session != nil {
		return e.session.Close()
	}

	return nil
}
