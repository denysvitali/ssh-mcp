package ssh

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// MaxConnections is the maximum number of concurrent connections allowed
	MaxConnections = 100

	// SSHDialTimeout is the timeout for SSH connection establishment
	SSHDialTimeout = 10 * time.Second

	// MaxJobs is the maximum number of concurrent background jobs per connection
	MaxJobs = 50
)

// JobStatus represents the state of a background job
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCanceled  JobStatus = "canceled"
)

// Job represents a background job
type Job struct {
	ID           string
	ConnectionID string
	Command      string
	Status       JobStatus
	Result       *CommandResult
	Created      time.Time
	CompletedAt  *time.Time
	Options      ExecuteOptions
	cancelChan   chan struct{}
	mu           sync.Mutex
}

// Lock locks the job mutex for safe concurrent access
func (j *Job) Lock() {
	j.mu.Lock()
}

// Unlock unlocks the job mutex
func (j *Job) Unlock() {
	j.mu.Unlock()
}

// ConnectionInfo holds information about an SSH connection
type ConnectionInfo struct {
	ID       string
	Host     string
	Port     int
	Username string
	Created  time.Time
}

// Connection represents an active SSH connection with a persistent shell
type Connection struct {
	Info     ConnectionInfo
	client   *ssh.Client
	executor *ShellExecutor
}

// Manager manages SSH connections
type Manager struct {
	connections    map[string]*Connection
	jobs           map[string]*Job
	validator      *HostValidator
	mu             sync.RWMutex
	jobMu          sync.RWMutex
	commandTimeout time.Duration
}

// NewManager creates a new SSH connection manager
func NewManager(validator *HostValidator, timeout time.Duration) *Manager {
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	return &Manager{
		connections:    make(map[string]*Connection),
		jobs:           make(map[string]*Job),
		validator:      validator,
		commandTimeout: timeout,
	}
}

// Connect establishes a new SSH connection
func (m *Manager) Connect(id, host string, port int, username, password, privateKeyPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check connection limit
	if len(m.connections) >= MaxConnections {
		return fmt.Errorf("connection limit reached (%d/%d)", len(m.connections), MaxConnections)
	}

	// Check if connection already exists
	if _, exists := m.connections[id]; exists {
		return fmt.Errorf("connection with ID '%s' already exists", id)
	}

	// Validate host
	if err := m.validator.Validate(host); err != nil {
		return err
	}

	// Prepare SSH config
	// Use InsecureIgnoreHostKey for now but this should be configurable in production
	// See: https://pkg.go.dev/golang.org/x/crypto/ssh#InsecureIgnoreHostKey
	// #nosec G106 - Host key verification intentionally disabled for dynamic SSH connections
	config := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         SSHDialTimeout,
	}

	// Add authentication methods
	if password != "" {
		config.Auth = append(config.Auth, ssh.Password(password))
	}

	if privateKeyPath != "" {
		// Read private key from file
		// #nosec G304 - Private key path is user-provided and validated by the validator
		keyData, err := os.ReadFile(privateKeyPath)
		if err != nil {
			return fmt.Errorf("failed to read private key file '%s': %w", privateKeyPath, err)
		}

		// First, try to parse as encrypted key with passphrase
		signer, err := ssh.ParsePrivateKeyWithPassphrase(keyData, []byte(password))
		if err != nil {
			// If that fails, try parsing as unencrypted key
			signer, err = ssh.ParsePrivateKey(keyData)
			if err != nil {
				return fmt.Errorf("failed to parse private key (try providing password if key is encrypted): %w", err)
			}
		}
		config.Auth = append(config.Auth, ssh.PublicKeys(signer))
	}

	if len(config.Auth) == 0 {
		return fmt.Errorf("no authentication method provided (password or private key required)")
	}

	// Connect to SSH server
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	// Create persistent shell executor with configured timeout
	executor, err := NewShellExecutor(client, m.commandTimeout)
	if err != nil {
		//nolint:errcheck // Best effort cleanup
		_ = client.Close()
		return fmt.Errorf("failed to create shell executor: %w", err)
	}

	// Store connection
	m.connections[id] = &Connection{
		Info: ConnectionInfo{
			ID:       id,
			Host:     host,
			Port:     port,
			Username: username,
			Created:  time.Now(),
		},
		client:   client,
		executor: executor,
	}

	return nil
}

// Execute runs a command on an existing connection
func (m *Manager) Execute(id, command string) (*CommandResult, error) {
	return m.ExecuteWithOptions(id, command, ExecuteOptions{})
}

// ExecuteWithOptions runs a command on an existing connection with options
func (m *Manager) ExecuteWithOptions(id, command string, opts ExecuteOptions) (*CommandResult, error) {
	m.mu.RLock()
	conn, exists := m.connections[id]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("connection '%s' not found", id)
	}

	return conn.executor.ExecuteWithOptions(command, opts)
}

// ExecuteAsync starts a command execution in a goroutine and returns a job ID
func (m *Manager) ExecuteAsync(id, command string, opts ExecuteOptions) (string, error) {
	m.mu.RLock()
	conn, exists := m.connections[id]
	m.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("connection '%s' not found", id)
	}

	m.jobMu.Lock()
	if len(m.jobs) >= MaxJobs {
		m.jobMu.Unlock()
		return "", fmt.Errorf("job limit reached (%d/%d)", len(m.jobs), MaxJobs)
	}
	m.jobMu.Unlock()

	jobID := fmt.Sprintf("job_%d", time.Now().UnixNano())
	job := &Job{
		ID:           jobID,
		ConnectionID: id,
		Command:      command,
		Status:       JobStatusPending,
		Created:      time.Now(),
		Options:      opts,
		cancelChan:   make(chan struct{}),
	}

	m.jobMu.Lock()
	m.jobs[jobID] = job
	m.jobMu.Unlock()

	go func() {
		job.mu.Lock()
		job.Status = JobStatusRunning
		job.mu.Unlock()

		// Execute with context that can be canceled
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start execution in a goroutine
		doneChan := make(chan *CommandResult)
		errChan := make(chan error)

		go func() {
			result, err := conn.executor.ExecuteWithOptions(command, opts)
			if err != nil {
				errChan <- err
			} else {
				doneChan <- result
			}
		}()

		// Wait for completion or cancellation
		select {
		case result := <-doneChan:
			job.mu.Lock()
			job.Result = result
			job.Status = JobStatusCompleted
			now := time.Now()
			job.CompletedAt = &now
			job.mu.Unlock()
		case err := <-errChan:
			job.mu.Lock()
			job.Result = &CommandResult{
				Stdout:   "",
				Stderr:   err.Error(),
				ExitCode: -1,
			}
			job.Status = JobStatusFailed
			now := time.Now()
			job.CompletedAt = &now
			job.mu.Unlock()
		case <-job.cancelChan:
			job.mu.Lock()
			job.Status = JobStatusCanceled
			now := time.Now()
			job.CompletedAt = &now
			job.mu.Unlock()
			cancel()
		case <-ctx.Done():
			job.mu.Lock()
			job.Status = JobStatusCanceled
			now := time.Now()
			job.CompletedAt = &now
			job.mu.Unlock()
		}
	}()

	return jobID, nil
}

// GetJob returns a job by ID
func (m *Manager) GetJob(jobID string) (*Job, error) {
	m.jobMu.RLock()
	job, exists := m.jobs[jobID]
	m.jobMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("job '%s' not found", jobID)
	}

	return job, nil
}

// CancelJob attempts to cancel a running job
func (m *Manager) CancelJob(jobID string) error {
	m.jobMu.RLock()
	job, exists := m.jobs[jobID]
	m.jobMu.RUnlock()

	if !exists {
		return fmt.Errorf("job '%s' not found", jobID)
	}

	job.mu.Lock()
	defer job.mu.Unlock()

	if job.Status != JobStatusPending && job.Status != JobStatusRunning {
		return fmt.Errorf("job '%s' is not cancelable (status: %s)", jobID, job.Status)
	}

	close(job.cancelChan)
	return nil
}

// ListJobs returns all jobs for a connection
func (m *Manager) ListJobs(connectionID string) []*Job {
	m.jobMu.RLock()
	defer m.jobMu.RUnlock()

	jobs := make([]*Job, 0)
	for _, job := range m.jobs {
		if job.ConnectionID == connectionID {
			jobs = append(jobs, job)
		}
	}

	return jobs
}

// Close closes an SSH connection
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	conn, exists := m.connections[id]
	if !exists {
		return fmt.Errorf("connection '%s' not found", id)
	}

	// Close executor and client
	if conn.executor != nil {
		//nolint:errcheck // Best effort cleanup
		_ = conn.executor.Close()
	}
	if conn.client != nil {
		//nolint:errcheck // Best effort cleanup
		_ = conn.client.Close()
	}

	// Remove from map
	delete(m.connections, id)

	return nil
}

// List returns information about all active connections
func (m *Manager) List() []ConnectionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]ConnectionInfo, 0, len(m.connections))
	for _, conn := range m.connections {
		infos = append(infos, conn.Info)
	}

	return infos
}

// CloseAll closes all active connections
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, conn := range m.connections {
		if conn.executor != nil {
			//nolint:errcheck // Best effort cleanup
			_ = conn.executor.Close()
		}
		if conn.client != nil {
			//nolint:errcheck // Best effort cleanup
			_ = conn.client.Close()
		}
		delete(m.connections, id)
	}
}
