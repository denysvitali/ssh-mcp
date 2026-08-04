package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// agentDialTimeout bounds how long we wait for the local SSH agent socket.
const agentDialTimeout = 2 * time.Second

// DefaultKeyNames lists the private key file names that are probed in ~/.ssh
// when no explicit key is provided, in preference order.
var DefaultKeyNames = []string{
	"id_ed25519",
	"id_ecdsa",
	"id_rsa",
	"id_dsa",
}

// ErrKeyEncrypted is returned when a private key requires a passphrase that was
// not supplied. Discovered (non-explicit) keys hitting this error are skipped.
var ErrKeyEncrypted = errors.New("private key is encrypted and no passphrase was provided")

// AuthOptions describes how a connection should authenticate.
type AuthOptions struct {
	// Password is used both as a password auth method and as the passphrase
	// for encrypted private keys.
	Password string

	// PrivateKeyPath is an explicit private key file (alias: identity_file).
	PrivateKeyPath string

	// UseAgent enables the SSH agent auth method when SSH_AUTH_SOCK is set.
	UseAgent bool

	// SSHDir overrides the directory scanned for default keys (test hook).
	// Empty means ~/.ssh.
	SSHDir string

	// AgentSocket overrides $SSH_AUTH_SOCK (test hook).
	AgentSocket string
}

// defaultSSHDir returns the directory that holds the user's SSH keys.
func defaultSSHDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ssh")
}

// DiscoverKeyFiles returns the default key files that exist in sshDir, in
// DefaultKeyNames preference order. Directories and unreadable entries are
// ignored. A missing or empty sshDir yields no results.
func DiscoverKeyFiles(sshDir string) []string {
	if sshDir == "" {
		return nil
	}

	found := make([]string, 0, len(DefaultKeyNames))
	for _, name := range DefaultKeyNames {
		path := filepath.Join(sshDir, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		found = append(found, path)
	}
	return found
}

// parseSigner parses a private key, using passphrase when the key is encrypted.
// It returns ErrKeyEncrypted (wrapped) when a passphrase is required but absent.
func parseSigner(keyData []byte, passphrase string) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(keyData)
	if err == nil {
		return signer, nil
	}

	var passErr *ssh.PassphraseMissingError
	if !errors.As(err, &passErr) {
		return nil, err
	}

	if passphrase == "" {
		return nil, ErrKeyEncrypted
	}

	signer, err = ssh.ParsePrivateKeyWithPassphrase(keyData, []byte(passphrase))
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt private key: %w", err)
	}
	return signer, nil
}

// loadSignerFromFile reads and parses a private key file.
func loadSignerFromFile(path, passphrase string) (ssh.Signer, error) {
	// #nosec G304 - key path is user-provided by design
	keyData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key file '%s': %w", path, err)
	}
	return parseSigner(keyData, passphrase)
}

// agentAuthMethod dials the SSH agent socket and returns a public-key callback
// auth method backed by it. Returns nil when no agent is reachable.
func agentAuthMethod(socket string) (ssh.AuthMethod, func() error) {
	if socket == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentDialTimeout)
	defer cancel()

	var dialer net.Dialer
	// #nosec G704 - the socket path is the local SSH_AUTH_SOCK, not remote input
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil
	}
	client := agent.NewClient(conn)
	return ssh.PublicKeysCallback(client.Signers), conn.Close
}

// buildAuthMethods assembles the SSH auth methods for the given options.
// Order: agent (if available) -> explicit key -> discovered default keys ->
// password. It also returns a cleanup func (never nil) and a human-readable
// description of what was used.
func buildAuthMethods(opts AuthOptions) ([]ssh.AuthMethod, func(), []string, error) {
	var (
		methods []ssh.AuthMethod
		closers []func() error
		used    []string
	)
	cleanup := func() {
		for _, c := range closers {
			//nolint:errcheck // Best effort cleanup
			_ = c()
		}
	}

	if opts.UseAgent {
		socket := opts.AgentSocket
		if socket == "" {
			socket = os.Getenv("SSH_AUTH_SOCK")
		}
		if method, closer := agentAuthMethod(socket); method != nil {
			methods = append(methods, method)
			used = append(used, "agent")
			if closer != nil {
				closers = append(closers, closer)
			}
		}
	}

	if opts.PrivateKeyPath != "" {
		signer, err := loadSignerFromFile(opts.PrivateKeyPath, opts.Password)
		if err != nil {
			cleanup()
			if errors.Is(err, ErrKeyEncrypted) {
				return nil, func() {}, nil, fmt.Errorf(
					"private key '%s' is encrypted: provide the passphrase in the 'password' field", opts.PrivateKeyPath)
			}
			return nil, func() {}, nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
		used = append(used, "key:"+opts.PrivateKeyPath)
	} else {
		sshDir := opts.SSHDir
		if sshDir == "" {
			sshDir = defaultSSHDir()
		}
		var signers []ssh.Signer
		for _, path := range DiscoverKeyFiles(sshDir) {
			signer, err := loadSignerFromFile(path, opts.Password)
			if err != nil {
				// Skip unreadable, malformed, or locked keys.
				continue
			}
			signers = append(signers, signer)
			used = append(used, "key:"+path)
		}
		if len(signers) > 0 {
			methods = append(methods, ssh.PublicKeys(signers...))
		}
	}

	if opts.Password != "" {
		methods = append(methods, ssh.Password(opts.Password))
		used = append(used, "password")
	}

	if len(methods) == 0 {
		cleanup()
		return nil, func() {}, nil, fmt.Errorf(
			"no authentication method available: provide 'password' or 'private_key_path', " +
				"start an SSH agent (SSH_AUTH_SOCK), or place a usable key in ~/.ssh")
	}

	return methods, cleanup, used, nil
}
