package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denysvitali/ssh-mcp/pkg/ssh"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
)

// Handlers manages MCP tool handlers for SSH operations
type Handlers struct {
	manager *ssh.Manager
	logger  *logrus.Logger
}

// NewHandlers creates a new handlers instance
func NewHandlers(manager *ssh.Manager, logger *logrus.Logger) *Handlers {
	if manager == nil {
		panic("ssh.Manager cannot be nil")
	}
	if logger == nil {
		panic("logger cannot be nil")
	}
	return &Handlers{
		manager: manager,
		logger:  logger,
	}
}

// ConnectInput is the input for the ssh_connect tool
type ConnectInput struct {
	ConnectionID   string `json:"connection_id" jsonschema:"Unique identifier for this connection"`
	Host           string `json:"host" jsonschema:"Remote host address (hostname or IP)"`
	Port           int    `json:"port,omitempty" jsonschema:"SSH port (default: 22)"`
	Username       string `json:"username" jsonschema:"SSH username"`
	Password       string `json:"password,omitempty" jsonschema:"SSH password (used for authentication or as passphrase for encrypted private keys)"`
	PrivateKeyPath string `json:"private_key_path,omitempty" jsonschema:"Path to SSH private key file (optional if using password)"`
}

// ExecuteInput is the input for the ssh_execute and ssh_execute_async tools
type ExecuteInput struct {
	ConnectionID  string `json:"connection_id" jsonschema:"Connection identifier"`
	Command       string `json:"command" jsonschema:"Command to execute"`
	MaxLines      int    `json:"max_lines,omitempty" jsonschema:"Maximum number of output lines (0 = unlimited)"`
	MaxBytes      int    `json:"max_bytes,omitempty" jsonschema:"Maximum number of output bytes (0 = unlimited)"`
	UseLoginShell bool   `json:"use_login_shell,omitempty" jsonschema:"Use login shell to source profiles (default: false)"`
	EnablePTY     bool   `json:"enable_pty,omitempty" jsonschema:"Allocate PTY for interactive apps like top, htop (default: false)"`
	PtyCols       uint   `json:"pty_cols,omitempty" jsonschema:"PTY columns (default: 80)"`
	PtyRows       uint   `json:"pty_rows,omitempty" jsonschema:"PTY rows (default: 24)"`
}

// JobInput is the input for job-scoped tools
type JobInput struct {
	JobID string `json:"job_id" jsonschema:"Job identifier"`
}

// ConnectionInput is the input for tools that only take a connection identifier
type ConnectionInput struct {
	ConnectionID string `json:"connection_id" jsonschema:"Connection identifier"`
}

// EmptyInput is the input for tools that take no parameters
type EmptyInput struct{}

// toOptions converts the tool input into SSH execute options, applying defaults
func (in ExecuteInput) toOptions() ssh.ExecuteOptions {
	opts := ssh.ExecuteOptions{
		MaxLines:      in.MaxLines,
		MaxBytes:      in.MaxBytes,
		UseLoginShell: in.UseLoginShell,
		EnablePTY:     in.EnablePTY,
		PtyCols:       in.PtyCols,
		PtyRows:       in.PtyRows,
	}
	if opts.PtyCols == 0 {
		opts.PtyCols = 80
	}
	if opts.PtyRows == 0 {
		opts.PtyRows = 24
	}
	return opts
}

// textResult builds a successful text result
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// errorResult builds an error result reported back to the model
func errorResult(format string, args ...interface{}) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// jsonResult marshals the response map into a text result
func (h *Handlers) jsonResult(response map[string]interface{}) *mcp.CallToolResult {
	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return errorResult("Internal error: failed to marshal response: %v", err)
	}
	return textResult(string(jsonResponse))
}

// validateConnectionID validates the connection ID format
func validateConnectionID(id string) error {
	if id == "" {
		return fmt.Errorf("connection_id cannot be empty")
	}
	if len(id) > 128 {
		return fmt.Errorf("connection_id too long (max 128 characters)")
	}
	// Only allow alphanumeric, dash, and underscore
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return fmt.Errorf("connection_id contains invalid characters (only alphanumeric, dash, underscore allowed)")
		}
	}
	return nil
}

// validatePort validates the port number
func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", port)
	}
	return nil
}

// validateCommand validates the command string
func validateCommand(cmd string) error {
	if cmd == "" {
		return fmt.Errorf("command cannot be empty")
	}
	if len(cmd) > 1048576 { // 1MB limit
		return fmt.Errorf("command too long (max 1MB)")
	}
	return nil
}

// validateAuthMethod validates authentication method is provided
func validateAuthMethod(password, privateKeyPath string) error {
	if password == "" && privateKeyPath == "" {
		return fmt.Errorf("either 'password' or 'private_key_path' must be provided")
	}
	return nil
}

// HandleConnect handles the ssh_connect tool
func (h *Handlers) HandleConnect(_ context.Context, _ *mcp.CallToolRequest, in ConnectInput) (*mcp.CallToolResult, any, error) {
	if err := validateConnectionID(in.ConnectionID); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	if strings.TrimSpace(in.Host) == "" {
		return errorResult("host cannot be empty"), nil, nil
	}

	if strings.TrimSpace(in.Username) == "" {
		return errorResult("username cannot be empty"), nil, nil
	}

	port := in.Port
	if port == 0 {
		port = 22
	}
	if err := validatePort(port); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	if err := validateAuthMethod(in.Password, in.PrivateKeyPath); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": in.ConnectionID,
		"host":          in.Host,
		"port":          port,
		"username":      in.Username,
	}).Info("Attempting SSH connection")

	if connErr := h.manager.Connect(in.ConnectionID, in.Host, port, in.Username, in.Password, in.PrivateKeyPath); connErr != nil {
		h.logger.WithError(connErr).Error("Failed to establish SSH connection")
		return errorResult("Failed to connect: %v", connErr), nil, nil
	}

	h.logger.Info("SSH connection established successfully")

	return h.jsonResult(map[string]interface{}{
		"success":       true,
		"connection_id": in.ConnectionID,
		"host":          in.Host,
		"port":          port,
		"username":      in.Username,
		"message":       "SSH connection established successfully",
	}), nil, nil
}

// HandleExecute handles the ssh_execute tool
func (h *Handlers) HandleExecute(_ context.Context, _ *mcp.CallToolRequest, in ExecuteInput) (*mcp.CallToolResult, any, error) {
	if err := validateConnectionID(in.ConnectionID); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}
	if err := validateCommand(in.Command); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	opts := in.toOptions()

	h.logger.WithFields(logrus.Fields{
		"connection_id": in.ConnectionID,
		"command":       in.Command,
		"max_lines":     opts.MaxLines,
		"max_bytes":     opts.MaxBytes,
		"enable_pty":    opts.EnablePTY,
	}).Debug("Executing SSH command")

	result, err := h.manager.ExecuteWithOptions(in.ConnectionID, in.Command, opts)
	if err != nil {
		h.logger.WithError(err).Error("Failed to execute SSH command")
		return errorResult("Failed to execute command: %v", err), nil, nil
	}

	h.logger.WithFields(logrus.Fields{
		"exit_code": result.ExitCode,
	}).Debug("Command executed successfully")

	return h.jsonResult(map[string]interface{}{
		"success":       true,
		"stdout":        result.Stdout,
		"stderr":        result.Stderr,
		"exit_code":     result.ExitCode,
		"signal":        result.Signal,
		"signal_name":   result.SignalName,
		"timed_out":     result.TimedOut,
		"binary_output": result.BinaryOutput,
	}), nil, nil
}

// HandleExecuteAsync handles the ssh_execute_async tool
func (h *Handlers) HandleExecuteAsync(_ context.Context, _ *mcp.CallToolRequest, in ExecuteInput) (*mcp.CallToolResult, any, error) {
	if err := validateConnectionID(in.ConnectionID); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}
	if err := validateCommand(in.Command); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": in.ConnectionID,
		"command":       in.Command,
	}).Debug("Submitting async SSH command")

	jobID, err := h.manager.ExecuteAsync(in.ConnectionID, in.Command, in.toOptions())
	if err != nil {
		h.logger.WithError(err).Error("Failed to submit async SSH command")
		return errorResult("Failed to submit async command: %v", err), nil, nil
	}

	h.logger.WithFields(logrus.Fields{
		"job_id": jobID,
	}).Debug("Async SSH command submitted")

	return h.jsonResult(map[string]interface{}{
		"success": true,
		"job_id":  jobID,
		"status":  ssh.JobStatusPending,
		"message": "Job submitted successfully",
	}), nil, nil
}

// HandleJobStatus handles the ssh_job_status tool
func (h *Handlers) HandleJobStatus(_ context.Context, _ *mcp.CallToolRequest, in JobInput) (*mcp.CallToolResult, any, error) {
	h.logger.WithFields(logrus.Fields{
		"job_id": in.JobID,
	}).Debug("Getting job status")

	job, err := h.manager.GetJob(in.JobID)
	if err != nil {
		h.logger.WithError(err).Error("Job not found")
		return errorResult("Job not found: %v", err), nil, nil
	}

	job.Lock()
	status := job.Status
	result := job.Result
	created := job.Created
	completedAt := job.CompletedAt
	command := job.Command
	connectionID := job.ConnectionID
	job.Unlock()

	response := map[string]interface{}{
		"success":       true,
		"job_id":        in.JobID,
		"status":        status,
		"connection_id": connectionID,
		"command":       command,
		"created":       created.Format("2006-01-02T15:04:05"),
	}

	if completedAt != nil {
		response["completed_at"] = completedAt.Format("2006-01-02T15:04:05")
	}

	if result != nil && (status == ssh.JobStatusCompleted || status == ssh.JobStatusFailed || status == ssh.JobStatusCanceled) {
		response["result"] = map[string]interface{}{
			"stdout":        result.Stdout,
			"stderr":        result.Stderr,
			"exit_code":     result.ExitCode,
			"signal":        result.Signal,
			"signal_name":   result.SignalName,
			"timed_out":     result.TimedOut,
			"binary_output": result.BinaryOutput,
		}
	}

	return h.jsonResult(response), nil, nil
}

// HandleJobCancel handles the ssh_job_cancel tool
func (h *Handlers) HandleJobCancel(_ context.Context, _ *mcp.CallToolRequest, in JobInput) (*mcp.CallToolResult, any, error) {
	h.logger.WithFields(logrus.Fields{
		"job_id": in.JobID,
	}).Debug("Canceling job")

	if cancelErr := h.manager.CancelJob(in.JobID); cancelErr != nil {
		h.logger.WithError(cancelErr).Error("Failed to cancel job")
		return errorResult("Failed to cancel job: %v", cancelErr), nil, nil
	}

	h.logger.Info("Job canceled successfully")

	return h.jsonResult(map[string]interface{}{
		"success": true,
		"job_id":  in.JobID,
		"message": "Job canceled successfully",
	}), nil, nil
}

// HandleJobList handles the ssh_job_list tool
func (h *Handlers) HandleJobList(_ context.Context, _ *mcp.CallToolRequest, in ConnectionInput) (*mcp.CallToolResult, any, error) {
	if err := validateConnectionID(in.ConnectionID); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": in.ConnectionID,
	}).Debug("Listing jobs")

	jobs := h.manager.ListJobs(in.ConnectionID)

	jobList := make([]map[string]interface{}, len(jobs))
	for i, job := range jobs {
		job.Lock()
		jobInfo := map[string]interface{}{
			"job_id":  job.ID,
			"status":  job.Status,
			"command": job.Command,
			"created": job.Created.Format("2006-01-02T15:04:05"),
		}
		if job.CompletedAt != nil {
			jobInfo["completed_at"] = job.CompletedAt.Format("2006-01-02T15:04:05")
		}
		job.Unlock()
		jobList[i] = jobInfo
	}

	return h.jsonResult(map[string]interface{}{
		"success":       true,
		"connection_id": in.ConnectionID,
		"jobs":          jobList,
		"count":         len(jobs),
	}), nil, nil
}

// HandleClose handles the ssh_close tool
func (h *Handlers) HandleClose(_ context.Context, _ *mcp.CallToolRequest, in ConnectionInput) (*mcp.CallToolResult, any, error) {
	if err := validateConnectionID(in.ConnectionID); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": in.ConnectionID,
	}).Info("Closing SSH connection")

	if closeErr := h.manager.Close(in.ConnectionID); closeErr != nil {
		h.logger.WithError(closeErr).Error("Failed to close SSH connection")
		return errorResult("Failed to close connection: %v", closeErr), nil, nil
	}

	h.logger.Info("SSH connection closed successfully")

	return h.jsonResult(map[string]interface{}{
		"success":       true,
		"connection_id": in.ConnectionID,
		"message":       "SSH connection closed successfully",
	}), nil, nil
}

// HandleList handles the ssh_list tool
func (h *Handlers) HandleList(_ context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, any, error) {
	h.logger.Debug("Listing active SSH connections")

	connections := h.manager.List()

	h.logger.WithFields(logrus.Fields{
		"count": len(connections),
	}).Debug("Retrieved connection list")

	connList := make([]map[string]interface{}, len(connections))
	for i, conn := range connections {
		connList[i] = map[string]interface{}{
			"connection_id": conn.ID,
			"host":          conn.Host,
			"port":          conn.Port,
			"username":      conn.Username,
			"created":       conn.Created.Format("2006-01-02 15:04:05"),
		}
	}

	return h.jsonResult(map[string]interface{}{
		"success":     true,
		"connections": connList,
		"count":       len(connections),
	}), nil, nil
}
