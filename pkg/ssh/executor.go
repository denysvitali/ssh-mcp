package ssh

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// Maximum time to wait for a new shell to answer its ready marker
	shellReadyTimeout = 15 * time.Second

	// Default command execution timeout
	DefaultCommandTimeout = 30 * time.Second

	// How long to wait for the stderr marker once stdout has completed
	stderrGracePeriod = 500 * time.Millisecond

	// Completion marker printed after every command
	markerPrefix     = "__MCP_SSH_END_"
	markerCodeMaxLen = 16

	// Highest signal number reported for exit codes above 128
	maxSignal = 64

	// Binary detection
	binarySampleSize = 8192
	binaryThreshold  = 0.30
)

// ExecuteOptions controls command execution behavior
type ExecuteOptions struct {
	MaxLines      int           // Maximum output lines (0 = unlimited)
	MaxBytes      int           // Maximum output bytes (0 = unlimited)
	UseLoginShell bool          // If true, use login shell (-l) to source profiles
	EnablePTY     bool          // If true, allocate PTY for interactive apps
	PtyCols       uint          // PTY columns (default: 80)
	PtyRows       uint          // PTY rows (default: 24)
	Timeout       time.Duration // Command timeout (0 = executor default)
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
	Warning      string // Out-of-band note about the shell session (e.g. it was restarted)
}

// ShellExecutor manages a persistent shell session for executing commands.
//
// Output of the remote shell is continuously pumped into in-memory buffers by
// exactly one goroutine per stream, for the whole lifetime of the shell. A
// command's completion is detected by a unique marker printed after it on both
// stdout and stderr. If a command times out, the shell is left running; the
// next command either discards the late output (if the timed-out command has
// finished meanwhile) or transparently replaces the shell with a fresh one.
type ShellExecutor struct {
	client         *ssh.Client
	mu             sync.Mutex // serializes commands
	commandTimeout time.Duration

	stateMu sync.Mutex // guards shell, closed
	shell   *shellSession
	closed  bool

	// pending is the marker of a timed-out command whose completion has not
	// been observed yet. Only accessed with mu held.
	pending string

	isPty   bool
	ptyCols uint
	ptyRows uint
}

// shellSession is one remote shell process together with its output buffers
type shellSession struct {
	session   *ssh.Session
	stdin     io.WriteCloser
	stdout    *streamBuffer
	stderr    *streamBuffer
	notify    chan struct{} // signaled whenever either stream changes
	closeOnce sync.Once
}

// streamBuffer accumulates everything read from a stream until reset
type streamBuffer struct {
	mu      sync.Mutex
	buf     []byte
	scanned int // offset up to which marker search has been done
	err     error
	notify  chan<- struct{}
}

func newStreamBuffer(r io.Reader, notify chan<- struct{}) *streamBuffer {
	s := &streamBuffer{notify: notify}
	go s.pump(r)
	return s
}

func (s *streamBuffer) pump(r io.Reader) {
	chunk := make([]byte, 32*1024)
	for {
		n, err := r.Read(chunk)
		s.mu.Lock()
		s.buf = append(s.buf, chunk[:n]...)
		if err != nil {
			s.err = err
		}
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
		if err != nil {
			return
		}
	}
}

// findMarker looks for a complete "<marker>:<code>\n" line. It returns the
// output preceding the marker (without the newline the marker line starts
// with) and the parsed exit code.
func (s *streamBuffer) findMarker(marker string) (output string, code int, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	from := max(0, s.scanned-len(marker)-markerCodeMaxLen)
	idx := bytes.Index(s.buf[from:], []byte(marker+":"))
	if idx < 0 {
		s.scanned = len(s.buf)
		return "", 0, false
	}
	idx += from
	rest := s.buf[idx+len(marker)+1:]
	eol := bytes.IndexByte(rest, '\n')
	if eol < 0 {
		s.scanned = idx
		return "", 0, false
	}
	//nolint:errcheck // Exit code parsing failure is reported as 0
	_, _ = fmt.Sscanf(strings.TrimSpace(string(rest[:eol])), "%d", &code)

	out := s.buf[:idx]
	out = bytes.TrimSuffix(out, []byte("\n"))
	out = bytes.TrimSuffix(out, []byte("\r"))
	return string(out), code, true
}

// snapshot returns the current buffer contents and terminal read error
func (s *streamBuffer) snapshot() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf), s.err
}

// text returns the current buffer contents
func (s *streamBuffer) text() string {
	out, _ := s.snapshot() //nolint:errcheck // Only the contents are needed
	return out
}

// reset discards buffered output
func (s *streamBuffer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = nil
	s.scanned = 0
}

// NewShellExecutor creates a new persistent shell executor
func NewShellExecutor(client *ssh.Client, timeout time.Duration) (*ShellExecutor, error) {
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	executor := &ShellExecutor{
		client:         client,
		commandTimeout: timeout,
	}
	shell, err := startShell(client)
	if err != nil {
		return nil, err
	}
	executor.shell = shell
	return executor, nil
}

// startShell opens a new session running the user's shell and waits until
// the shell is ready to accept commands
func startShell(client *ssh.Client) (*shellSession, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	//nolint:errcheck // Non-fatal error, TERM variable is optional
	_ = session.Setenv("TERM", "dumb")

	stdin, err := session.StdinPipe()
	if err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := session.Shell(); err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = session.Close()
		return nil, fmt.Errorf("failed to start shell: %w", err)
	}

	notify := make(chan struct{}, 1)
	shell := &shellSession{
		session: session,
		stdin:   stdin,
		stdout:  newStreamBuffer(stdoutPipe, notify),
		stderr:  newStreamBuffer(stderrPipe, notify),
		notify:  notify,
	}

	// Set up the shell for clean output, then wait for a ready marker so
	// that banners and init noise never leak into the first command.
	marker := newMarker()
	init := strings.Join([]string{
		"stty -echo 2>/dev/null || true",           // Disable echo if available
		"export PS1='' 2>/dev/null || true",        // Empty prompt if available
		"unset PROMPT_COMMAND 2>/dev/null || true", // Disable prompt command
		"set +o vi 2>/dev/null || true",            // Disable vi mode
		markerCommand(marker, "0", ""),
	}, "\n") + "\n"
	if _, err := stdin.Write([]byte(init)); err != nil {
		shell.close()
		return nil, fmt.Errorf("failed to initialize shell: %w", err)
	}

	timer := time.NewTimer(shellReadyTimeout)
	defer timer.Stop()
	for {
		if _, _, found := shell.stdout.findMarker(marker); found {
			shell.stdout.reset()
			shell.stderr.reset()
			return shell, nil
		}
		if _, err := shell.stdout.snapshot(); err != nil {
			shell.close()
			return nil, fmt.Errorf("shell exited during initialization: %w", err)
		}
		select {
		case <-notify:
		case <-timer.C:
			shell.close()
			return nil, fmt.Errorf("shell did not become ready within %s", shellReadyTimeout)
		}
	}
}

// close terminates the shell session; safe to call from several goroutines.
// Closing the session is enough: stdin.Close (CloseWrite) races with a
// concurrent Write inside x/crypto/ssh.
func (s *shellSession) close() {
	s.closeOnce.Do(func() {
		//nolint:errcheck // Best effort cleanup
		_ = s.session.Close()
	})
}

// markerCounter makes markers unique even within the same nanosecond
var markerCounter atomic.Uint64

// newMarker returns a unique completion marker
func newMarker() string {
	return fmt.Sprintf("%s%d_%d__", markerPrefix, time.Now().UnixNano(), markerCounter.Add(1))
}

// markerCommand returns a shell line printing "\n<marker>:<code>" on the given
// redirection. The marker is passed to printf in two halves so that an echo of
// the command line itself (e.g. with a PTY) never contains the marker.
func markerCommand(marker, code, redirect string) string {
	return fmt.Sprintf("printf '\\n%%s%%s:%%s\\n' '%s' '%s' \"%s\"%s",
		marker[:len(markerPrefix)], marker[len(markerPrefix):], code, redirect)
}

// prepareCommand wraps the command so that it cannot consume the shell's own
// input (stdin is /dev/null) and is followed by completion markers on stderr
// and stdout carrying its exit code
func prepareCommand(command, marker string) string {
	return "{\n" + command + "\n} </dev/null\n" +
		"__mcp_ec=$?\n" +
		markerCommand(marker, "$__mcp_ec", " >&2") + "\n" +
		markerCommand(marker, "$__mcp_ec", "") + "\n"
}

// Execute runs a command in the persistent shell and returns the result
// Deprecated: Use ExecuteWithOptions instead
func (e *ShellExecutor) Execute(command string) (*CommandResult, error) {
	return e.ExecuteWithOptions(command, ExecuteOptions{})
}

// allocatePty allocates a PTY for the session if not already allocated
func (e *ShellExecutor) allocatePty(shell *shellSession, opts ExecuteOptions) error {
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
	if err := shell.session.RequestPty("xterm-256color", int(cols), int(rows), modes); err != nil {
		return fmt.Errorf("failed to request PTY: %w", err)
	}
	e.isPty = true
	e.ptyCols = cols
	e.ptyRows = rows
	return nil
}

// currentShell returns a shell ready for the next command, replacing a dead
// shell or one still busy with a timed-out command. Must be called with mu held.
func (e *ShellExecutor) currentShell() (*shellSession, string, error) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	if e.closed {
		return nil, "", fmt.Errorf("shell executor is closed")
	}

	var warning string
	if e.shell != nil && e.pending != "" {
		if _, _, found := e.shell.stdout.findMarker(e.pending); found {
			// The timed-out command has finished meanwhile; its output is stale.
			e.pending = ""
		} else {
			e.shell.close()
			e.shell = nil
			warning = "the previous command timed out and was still running, so a new shell was started " +
				"(working directory and environment variables were reset)"
		}
	}
	if e.shell != nil {
		if _, err := e.shell.stdout.snapshot(); err != nil {
			e.shell.close()
			e.shell = nil
			warning = "the previous shell had exited, so a new shell was started " +
				"(working directory and environment variables were reset)"
		}
	}
	if e.shell == nil {
		e.pending = ""
		e.isPty = false
		shell, err := startShell(e.client)
		if err != nil {
			return nil, "", fmt.Errorf("shell session lost and could not be restarted "+
				"(the SSH connection may be dead; reconnect with ssh_connect): %w", err)
		}
		e.shell = shell
	}

	// Drop anything printed since the last command, e.g. by background jobs.
	e.shell.stdout.reset()
	e.shell.stderr.reset()
	return e.shell, warning, nil
}

// ExecuteWithOptions runs a command in the persistent shell with additional options
func (e *ShellExecutor) ExecuteWithOptions(command string, opts ExecuteOptions) (*CommandResult, error) {
	if strings.Contains(command, markerPrefix) {
		return nil, fmt.Errorf("command contains forbidden delimiter pattern '%s'", markerPrefix)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	shell, warning, err := e.currentShell()
	if err != nil {
		return nil, err
	}

	if err := e.allocatePty(shell, opts); err != nil {
		return nil, err
	}

	if opts.UseLoginShell {
		command = "bash -l -c " + escapeForShell(command)
	}

	marker := newMarker()
	if _, err := shell.stdin.Write([]byte(prepareCommand(command, marker))); err != nil {
		e.discardShell(shell)
		return nil, fmt.Errorf("failed to write command: %w", err)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = e.commandTimeout
	}

	result := e.collectOutput(shell, marker, timeout)
	result.Warning = joinWarnings(warning, result.Warning)
	return processOutput(result, opts), nil
}

// discardShell closes the shell so the next command starts a fresh one
func (e *ShellExecutor) discardShell(shell *shellSession) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	shell.close()
	if e.shell == shell {
		e.shell = nil
	}
}

// collectOutput waits until the command's markers show up on stdout and
// stderr, the shell dies, or the timeout expires
func (e *ShellExecutor) collectOutput(shell *shellSession, marker string, timeout time.Duration) *CommandResult {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var (
		stdout     string
		exitCode   int
		stdoutDone bool
		grace      <-chan time.Time
	)

	for {
		if !stdoutDone {
			stdout, exitCode, stdoutDone = shell.stdout.findMarker(marker)
			if stdoutDone {
				// The stderr marker is written first, so it is normally already
				// here; only wait briefly in case fd 2 was redirected for good.
				grace = time.After(stderrGracePeriod)
			}
		}
		if stdoutDone {
			if stderr, _, found := shell.stderr.findMarker(marker); found {
				return &CommandResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}
			}
		} else if out, err := shell.stdout.snapshot(); err != nil {
			stderr := shell.stderr.text()
			e.discardShell(shell)
			return &CommandResult{
				Stdout:   out,
				Stderr:   stderr,
				ExitCode: -1,
				Warning: fmt.Sprintf("the shell session ended before the command completed (%v); "+
					"a new shell will be started for the next command", err),
			}
		}

		select {
		case <-shell.notify:
		case <-grace:
			stderr := shell.stderr.text()
			return &CommandResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}
		case <-timer.C:
			out := shell.stdout.text()
			stderr := shell.stderr.text()
			e.pending = marker
			return &CommandResult{
				Stdout:   out,
				Stderr:   stderr,
				ExitCode: -1,
				TimedOut: true,
				Warning: fmt.Sprintf("command timed out after %s and may still be running; "+
					"if it has not finished by the next command, a new shell will be started", timeout),
			}
		}
	}
}

// joinWarnings combines non-empty warnings
func joinWarnings(warnings ...string) string {
	var parts []string
	for _, w := range warnings {
		if w != "" {
			parts = append(parts, w)
		}
	}
	return strings.Join(parts, "; ")
}

// processOutput handles signal detection, binary detection, and truncation
func processOutput(result *CommandResult, opts ExecuteOptions) *CommandResult {
	stdoutStr := strings.TrimSpace(result.Stdout)
	result.Stderr = strings.TrimSpace(result.Stderr)

	// Shells report death by signal N as exit status 128+N
	if result.ExitCode > 128 && result.ExitCode <= 128+maxSignal {
		result.Signal, result.SignalName = mapExitErrorToSignal(result.ExitCode)
	}

	if opts.MaxBytes > 0 && len(stdoutStr) > opts.MaxBytes {
		sampleSize := min(opts.MaxBytes, binarySampleSize)
		if detectBinary([]byte(stdoutStr[:sampleSize])) {
			result.BinaryOutput = true
		}
	}

	result.Stdout = truncateOutput(stdoutStr, opts.MaxLines, opts.MaxBytes)
	return result
}

// escapeForShell wraps a command string in single quotes for safe shell execution
func escapeForShell(cmd string) string {
	return "'" + strings.ReplaceAll(cmd, "'", "'\\''") + "'"
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

// Close closes the shell executor. It does not wait for a running command:
// closing the session makes that command return immediately.
func (e *ShellExecutor) Close() error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	e.closed = true
	if e.shell != nil {
		e.shell.close()
		e.shell = nil
	}
	return nil
}
