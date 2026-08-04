package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// newTestKeyPEM generates an unencrypted OpenSSH ed25519 private key in PEM form.
func newTestKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(block)
}

// newTestKeyPEMWithPassphrase generates an encrypted OpenSSH ed25519 private key.
func newTestKeyPEMWithPassphrase(t *testing.T, passphrase string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatalf("marshal encrypted key: %v", err)
	}
	return pem.EncodeToMemory(block)
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDiscoverKeyFiles(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		dirs     []string
		emptyDir bool
		noDir    bool
		want     []string
	}{
		{
			name:  "no keys",
			files: nil,
			want:  nil,
		},
		{
			name:  "single ed25519 key",
			files: []string{"id_ed25519"},
			want:  []string{"id_ed25519"},
		},
		{
			name:  "preference order is respected",
			files: []string{"id_rsa", "id_ed25519", "id_dsa", "id_ecdsa"},
			want:  []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_dsa"},
		},
		{
			name:  "public keys and unrelated files ignored",
			files: []string{"id_ed25519", "id_ed25519.pub", "known_hosts", "config", "authorized_keys"},
			want:  []string{"id_ed25519"},
		},
		{
			name:  "directory named like a key is skipped",
			files: []string{"id_rsa"},
			dirs:  []string{"id_ed25519"},
			want:  []string{"id_rsa"},
		},
		{
			name:  "missing directory yields nothing",
			noDir: true,
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), ".ssh")
			if !tt.noDir {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}
			for _, f := range tt.files {
				writeFile(t, filepath.Join(dir, f), []byte("stub"))
			}
			for _, d := range tt.dirs {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}

			got := DiscoverKeyFiles(dir)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i, name := range tt.want {
				if want := filepath.Join(dir, name); got[i] != want {
					t.Errorf("index %d: got %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

func TestDiscoverKeyFilesEmptyDir(t *testing.T) {
	if got := DiscoverKeyFiles(""); got != nil {
		t.Fatalf("expected nil for empty dir, got %v", got)
	}
}

func TestParseSigner(t *testing.T) {
	plain := newTestKeyPEM(t)
	encrypted := newTestKeyPEMWithPassphrase(t, "hunter2")

	tests := []struct {
		name       string
		data       []byte
		passphrase string
		wantErr    error
		wantAnyErr bool
	}{
		{name: "unencrypted key", data: plain},
		{name: "unencrypted key ignores passphrase", data: plain, passphrase: "unused"},
		{name: "encrypted key with correct passphrase", data: encrypted, passphrase: "hunter2"},
		{name: "encrypted key without passphrase", data: encrypted, wantErr: ErrKeyEncrypted},
		{name: "encrypted key with wrong passphrase", data: encrypted, passphrase: "nope", wantAnyErr: true},
		{name: "garbage data", data: []byte("not a key"), wantAnyErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer, err := parseSigner(tt.data, tt.passphrase)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("got err %v, want %v", err, tt.wantErr)
				}
			case tt.wantAnyErr:
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if signer == nil {
					t.Fatal("expected a signer")
				}
			}
		})
	}
}

func TestBuildAuthMethods(t *testing.T) {
	plain := newTestKeyPEM(t)
	encrypted := newTestKeyPEMWithPassphrase(t, "hunter2")

	tests := []struct {
		name string
		// files written into the fake ~/.ssh
		files map[string][]byte
		// explicit key file name (resolved inside the fake ~/.ssh)
		explicitKey string
		password    string
		wantMethods int
		wantErr     bool
		wantUsed    []string
	}{
		{
			name:        "password only",
			password:    "secret",
			wantMethods: 1,
			wantUsed:    []string{"password"},
		},
		{
			name:        "explicit unencrypted key",
			files:       map[string][]byte{"mykey": plain},
			explicitKey: "mykey",
			wantMethods: 1,
		},
		{
			name:        "explicit encrypted key with passphrase",
			files:       map[string][]byte{"mykey": encrypted},
			explicitKey: "mykey",
			password:    "hunter2",
			wantMethods: 2, // publickey + password
		},
		{
			name:        "explicit encrypted key without passphrase errors",
			files:       map[string][]byte{"mykey": encrypted},
			explicitKey: "mykey",
			wantErr:     true,
		},
		{
			name:        "explicit missing key errors",
			explicitKey: "nope",
			wantErr:     true,
		},
		{
			name:        "discovered default key",
			files:       map[string][]byte{"id_ed25519": plain},
			wantMethods: 1,
		},
		{
			name:        "encrypted discovered key is skipped, not fatal",
			files:       map[string][]byte{"id_ed25519": encrypted, "id_rsa": plain},
			wantMethods: 1,
		},
		{
			name:    "all discovered keys unusable and no password errors",
			files:   map[string][]byte{"id_ed25519": encrypted},
			wantErr: true,
		},
		{
			name:        "unusable discovered key falls back to password",
			files:       map[string][]byte{"id_ed25519": []byte("garbage")},
			password:    "secret",
			wantMethods: 1,
			wantUsed:    []string{"password"},
		},
		{
			name:    "nothing at all errors",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range tt.files {
				writeFile(t, filepath.Join(dir, name), data)
			}

			opts := AuthOptions{
				Password: tt.password,
				SSHDir:   dir,
				UseAgent: false,
			}
			if tt.explicitKey != "" {
				opts.PrivateKeyPath = filepath.Join(dir, tt.explicitKey)
			}

			methods, cleanup, used, err := buildAuthMethods(opts)
			if cleanup != nil {
				defer cleanup()
			}

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got methods=%v used=%v", len(methods), used)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(methods) != tt.wantMethods {
				t.Fatalf("got %d auth methods (used=%v), want %d", len(methods), used, tt.wantMethods)
			}
			if tt.wantUsed != nil {
				if len(used) != len(tt.wantUsed) {
					t.Fatalf("got used=%v, want %v", used, tt.wantUsed)
				}
				for i, u := range tt.wantUsed {
					if used[i] != u {
						t.Errorf("used[%d] = %q, want %q", i, used[i], u)
					}
				}
			}
		})
	}
}

func TestBuildAuthMethodsAgentUnavailable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "id_ed25519"), newTestKeyPEM(t))

	// A socket path that does not exist must not break the connection setup.
	methods, cleanup, _, err := buildAuthMethods(AuthOptions{
		SSHDir:      dir,
		UseAgent:    true,
		AgentSocket: filepath.Join(dir, "nonexistent.sock"),
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("got %d methods, want 1 (key only)", len(methods))
	}
}
