package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"

	"net"
	"os/exec"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testServer is a minimal in-process SSH server that runs /bin/sh for shell
// requests, so the executor can be exercised against a real shell.
type testServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	wg       sync.WaitGroup
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &testServer{listener: listener, config: config}
	srv.wg.Add(1)
	go srv.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		srv.wg.Wait()
	})
	return srv
}

func (s *testServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *testServer) handleConn(conn net.Conn) {
	_, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go handleSession(ch, chReqs)
	}
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.Type != "shell" {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)
		go func() {
			cmd := exec.CommandContext(context.Background(), "/bin/sh")
			cmd.Stdout = ch
			cmd.Stderr = ch.Stderr()
			// Copy stdin ourselves: exec.Cmd.Wait would otherwise wait for the
			// client to close stdin instead of returning when the shell exits,
			// unlike sshd.
			stdin, err := cmd.StdinPipe()
			if err != nil {
				_ = ch.Close()
				return
			}
			go func() {
				_, _ = io.Copy(stdin, ch)
				_ = stdin.Close()
			}()
			status := uint32(0)
			if err := cmd.Run(); err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					status = uint32(exitErr.ExitCode()) //nolint:gosec // test helper
				} else {
					status = 255
				}
			}
			payload := make([]byte, 4)
			binary.BigEndian.PutUint32(payload, status)
			_, _ = ch.SendRequest("exit-status", false, payload)
			_ = ch.Close()
		}()
	}
}

// dial connects a client to the test server
func (s *testServer) dial(t *testing.T) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", s.listener.Addr().String(), &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test server
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// newTestExecutor returns an executor connected to a fresh test server
func newTestExecutor(t *testing.T, timeout time.Duration) *ShellExecutor {
	t.Helper()
	srv := newTestServer(t)
	exec, err := NewShellExecutor(srv.dial(t), timeout)
	if err != nil {
		t.Fatalf("NewShellExecutor: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}
