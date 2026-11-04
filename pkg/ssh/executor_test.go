package ssh

import (
	"strings"
	"testing"
	"time"
)

func mustExec(t *testing.T, e *ShellExecutor, cmd string) *CommandResult {
	t.Helper()
	res, err := e.Execute(cmd)
	if err != nil {
		t.Fatalf("Execute(%q): %v", cmd, err)
	}
	return res
}

func TestExecuteBasicOutputAndExitCode(t *testing.T) {
	e := newTestExecutor(t, 5*time.Second)

	res := mustExec(t, e, "echo hello; echo world")
	if res.Stdout != "hello\nworld" || res.ExitCode != 0 {
		t.Fatalf("got stdout=%q exit=%d", res.Stdout, res.ExitCode)
	}

	res = mustExec(t, e, "false")
	if res.ExitCode != 1 {
		t.Fatalf("expected exit 1, got %d", res.ExitCode)
	}
}

func TestExecuteCapturesStderr(t *testing.T) {
	e := newTestExecutor(t, 5*time.Second)

	res := mustExec(t, e, "echo out; echo err >&2; exit_code_is() { return 3; }; exit_code_is")
	if res.Stdout != "out" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "err" {
		t.Fatalf("stderr = %q", res.Stderr)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d", res.ExitCode)
	}
}

func TestExecuteOutputWithoutTrailingNewline(t *testing.T) {
	e := newTestExecutor(t, 5*time.Second)

	res := mustExec(t, e, "printf 'a:b:c'")
	if res.Stdout != "a:b:c" || res.ExitCode != 0 {
		t.Fatalf("got stdout=%q exit=%d", res.Stdout, res.ExitCode)
	}

	res = mustExec(t, e, "printf 'x'; false")
	if res.Stdout != "x" || res.ExitCode != 1 {
		t.Fatalf("got stdout=%q exit=%d", res.Stdout, res.ExitCode)
	}
}

func TestExecuteStdinReadingCommandDoesNotEatDelimiter(t *testing.T) {
	e := newTestExecutor(t, 3*time.Second)

	res := mustExec(t, e, "cat; echo after")
	if res.TimedOut || res.Stdout != "after" {
		t.Fatalf("got stdout=%q timedOut=%v", res.Stdout, res.TimedOut)
	}
}

func TestExecuteStatePersists(t *testing.T) {
	e := newTestExecutor(t, 5*time.Second)

	mustExec(t, e, "cd /tmp; export MCP_TEST_VAR=42")
	res := mustExec(t, e, "pwd; echo $MCP_TEST_VAR")
	if res.Stdout != "/tmp\n42" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestExecuteRecoversAfterTimeout(t *testing.T) {
	e := newTestExecutor(t, 1*time.Second)

	res := mustExec(t, e, "echo started; sleep 3; echo late")
	if !res.TimedOut {
		t.Fatalf("expected timeout, got %+v", res)
	}
	if !strings.Contains(res.Stdout, "started") {
		t.Fatalf("expected partial output, got %q", res.Stdout)
	}

	// Following commands must still work and must not see stale output.
	for i := 0; i < 3; i++ {
		res = mustExec(t, e, "echo next")
		if res.TimedOut || res.Stdout != "next" || res.ExitCode != 0 {
			t.Fatalf("iteration %d: got %+v", i, res)
		}
	}
}

func TestExecuteDiscardsLateOutputOfTimedOutCommand(t *testing.T) {
	e := newTestExecutor(t, 1*time.Second)

	res := mustExec(t, e, "sleep 1.5; echo late")
	if !res.TimedOut {
		t.Fatalf("expected timeout, got %+v", res)
	}
	time.Sleep(1 * time.Second) // let the timed-out command finish

	res = mustExec(t, e, "echo fresh")
	if res.Stdout != "fresh" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestExecuteShellExit(t *testing.T) {
	e := newTestExecutor(t, 5*time.Second)

	res := mustExec(t, e, "echo bye; exit 7")
	if res.TimedOut || res.Stdout != "bye" || !strings.Contains(res.Warning, "shell session ended") {
		t.Fatalf("expected shell-exit to be reported, got %+v", res)
	}

	// The executor must start a fresh shell for the next command.
	res = mustExec(t, e, "echo alive")
	if res.Stdout != "alive" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestExecuteLargeStderrDoesNotDeadlock(t *testing.T) {
	e := newTestExecutor(t, 10*time.Second)

	res := mustExec(t, e, "i=0; while [ $i -lt 40000 ]; do echo 'some fairly long line of stderr output here' >&2; i=$((i+1)); done; echo done")
	if res.TimedOut || res.Stdout != "done" {
		t.Fatalf("got stdout=%q timedOut=%v", res.Stdout, res.TimedOut)
	}
	if len(res.Stderr) < 1_000_000 {
		t.Fatalf("stderr too short: %d bytes", len(res.Stderr))
	}
}

func TestExecuteBackgroundJobDoesNotHideOutput(t *testing.T) {
	e := newTestExecutor(t, 5*time.Second)

	res := mustExec(t, e, "echo before; (sleep 1; echo bg) >/dev/null 2>&1 & echo started")
	if res.Stdout != "before\nstarted" || res.ExitCode != 0 {
		t.Fatalf("got stdout=%q exit=%d", res.Stdout, res.ExitCode)
	}
}

func TestExecuteKeepsStateWhenTimedOutCommandFinished(t *testing.T) {
	e := newTestExecutor(t, 1*time.Second)

	mustExec(t, e, "cd /tmp")
	if res := mustExec(t, e, "sleep 1.5"); !res.TimedOut {
		t.Fatalf("expected timeout, got %+v", res)
	}
	time.Sleep(1 * time.Second)

	res := mustExec(t, e, "pwd")
	if res.Stdout != "/tmp" || res.Warning != "" {
		t.Fatalf("got stdout=%q warning=%q", res.Stdout, res.Warning)
	}
}

func TestExecuteReportsRestartAfterStuckCommand(t *testing.T) {
	e := newTestExecutor(t, 1*time.Second)

	if res := mustExec(t, e, "sleep 30"); !res.TimedOut {
		t.Fatalf("expected timeout, got %+v", res)
	}
	res := mustExec(t, e, "echo ok")
	if res.Stdout != "ok" || !strings.Contains(res.Warning, "new shell") {
		t.Fatalf("got stdout=%q warning=%q", res.Stdout, res.Warning)
	}
}

func TestExecutePerCommandTimeout(t *testing.T) {
	e := newTestExecutor(t, 1*time.Second)

	res, err := e.ExecuteWithOptions("sleep 1.5; echo done", ExecuteOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || res.Stdout != "done" {
		t.Fatalf("got %+v", res)
	}
}

func TestExecuteReportsSignal(t *testing.T) {
	e := newTestExecutor(t, 5*time.Second)

	res := mustExec(t, e, "sh -c 'kill -9 $$'")
	if res.ExitCode != 137 || res.SignalName != "SIGKILL" {
		t.Fatalf("got exit=%d signal=%q", res.ExitCode, res.SignalName)
	}
}

func TestCloseInterruptsRunningCommand(t *testing.T) {
	e := newTestExecutor(t, 30*time.Second)

	done := make(chan *CommandResult, 1)
	go func() {
		res, _ := e.Execute("sleep 20")
		done <- res
	}()
	time.Sleep(300 * time.Millisecond)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not interrupt the running command")
	}
	if _, err := e.Execute("echo x"); err == nil {
		t.Fatal("expected error after Close")
	}
}
