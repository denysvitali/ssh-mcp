package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denysvitali/ssh-mcp/pkg/ssh"
	"github.com/mark3labs/mcp-go/mcp"
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
func (h *Handlers) HandleConnect(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract parameters
	connectionID, err := req.RequireString("connection_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate connection ID
	if validationErr := validateConnectionID(connectionID); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	host, err := req.RequireString("host")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate host is not empty after trim
	if strings.TrimSpace(host) == "" {
		return mcp.NewToolResultError("host cannot be empty"), nil
	}

	username, err := req.RequireString("username")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate username is not empty after trim
	if strings.TrimSpace(username) == "" {
		return mcp.NewToolResultError("username cannot be empty"), nil
	}

	// Optional parameters
	port := int(req.GetFloat("port", 22))
	if validationErr := validatePort(port); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	password := req.GetString("password", "")
	privateKeyPath := req.GetString("private_key_path", "")

	// Validate authentication method
	if validationErr := validateAuthMethod(password, privateKeyPath); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": connectionID,
		"host":          host,
		"port":          port,
		"username":      username,
	}).Info("Attempting SSH connection")

	// Establish connection
	if connErr := h.manager.Connect(connectionID, host, port, username, password, privateKeyPath); connErr != nil {
		h.logger.WithError(connErr).Error("Failed to establish SSH connection")
		return mcp.NewToolResultError(fmt.Sprintf("Failed to connect: %v", connErr)), nil
	}

	h.logger.Info("SSH connection established successfully")

	// Return success response
	response := map[string]interface{}{
		"success":       true,
		"connection_id": connectionID,
		"host":          host,
		"port":          port,
		"username":      username,
		"message":       "SSH connection established successfully",
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return mcp.NewToolResultError(fmt.Sprintf("Internal error: failed to marshal response: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonResponse)), nil
}

// HandleExecute handles the ssh_execute tool
func (h *Handlers) HandleExecute(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract parameters
	connectionID, err := req.RequireString("connection_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate connection ID
	if validationErr := validateConnectionID(connectionID); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	command, err := req.RequireString("command")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate command
	if validationErr := validateCommand(command); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	// Optional execution options
	opts := ssh.ExecuteOptions{
		MaxLines:      int(req.GetFloat("max_lines", 0)),
		MaxBytes:      int(req.GetFloat("max_bytes", 0)),
		UseLoginShell: req.GetBool("use_login_shell", false),
		EnablePTY:     req.GetBool("enable_pty", false),
		PtyCols:       uint(req.GetFloat("pty_cols", 80)),
		PtyRows:       uint(req.GetFloat("pty_rows", 24)),
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": connectionID,
		"command":       command,
		"max_lines":     opts.MaxLines,
		"max_bytes":     opts.MaxBytes,
		"enable_pty":    opts.EnablePTY,
	}).Debug("Executing SSH command")

	// Execute command with options
	result, err := h.manager.ExecuteWithOptions(connectionID, command, opts)
	if err != nil {
		h.logger.WithError(err).Error("Failed to execute SSH command")
		return mcp.NewToolResultError(fmt.Sprintf("Failed to execute command: %v", err)), nil
	}

	h.logger.WithFields(logrus.Fields{
		"exit_code": result.ExitCode,
	}).Debug("Command executed successfully")

	// Return result
	response := map[string]interface{}{
		"success":       true,
		"stdout":        result.Stdout,
		"stderr":        result.Stderr,
		"exit_code":     result.ExitCode,
		"signal":        result.Signal,
		"signal_name":   result.SignalName,
		"timed_out":     result.TimedOut,
		"binary_output": result.BinaryOutput,
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return mcp.NewToolResultError(fmt.Sprintf("Internal error: failed to marshal response: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonResponse)), nil
}

// HandleExecuteAsync handles the ssh_execute_async tool
func (h *Handlers) HandleExecuteAsync(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract parameters
	connectionID, err := req.RequireString("connection_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate connection ID
	if validationErr := validateConnectionID(connectionID); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	command, err := req.RequireString("command")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate command
	if validationErr := validateCommand(command); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	// Optional execution options
	opts := ssh.ExecuteOptions{
		MaxLines:      int(req.GetFloat("max_lines", 0)),
		MaxBytes:      int(req.GetFloat("max_bytes", 0)),
		UseLoginShell: req.GetBool("use_login_shell", false),
		EnablePTY:     req.GetBool("enable_pty", false),
		PtyCols:       uint(req.GetFloat("pty_cols", 80)),
		PtyRows:       uint(req.GetFloat("pty_rows", 24)),
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": connectionID,
		"command":       command,
	}).Debug("Submitting async SSH command")

	// Execute async command
	jobID, err := h.manager.ExecuteAsync(connectionID, command, opts)
	if err != nil {
		h.logger.WithError(err).Error("Failed to submit async SSH command")
		return mcp.NewToolResultError(fmt.Sprintf("Failed to submit async command: %v", err)), nil
	}

	h.logger.WithFields(logrus.Fields{
		"job_id": jobID,
	}).Debug("Async SSH command submitted")

	// Return job ID
	response := map[string]interface{}{
		"success": true,
		"job_id":  jobID,
		"status":  ssh.JobStatusPending,
		"message": "Job submitted successfully",
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return mcp.NewToolResultError(fmt.Sprintf("Internal error: failed to marshal response: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonResponse)), nil
}

// HandleJobStatus handles the ssh_job_status tool
func (h *Handlers) HandleJobStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract job_id parameter
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	h.logger.WithFields(logrus.Fields{
		"job_id": jobID,
	}).Debug("Getting job status")

	// Get job
	job, err := h.manager.GetJob(jobID)
	if err != nil {
		h.logger.WithError(err).Error("Job not found")
		return mcp.NewToolResultError(fmt.Sprintf("Job not found: %v", err)), nil
	}

	job.Lock()
	status := job.Status
	result := job.Result
	created := job.Created
	completedAt := job.CompletedAt
	command := job.Command
	connectionID := job.ConnectionID
	job.Unlock()

	// Build response
	response := map[string]interface{}{
		"success":       true,
		"job_id":        jobID,
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

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return mcp.NewToolResultError(fmt.Sprintf("Internal error: failed to marshal response: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonResponse)), nil
}

// HandleJobCancel handles the ssh_job_cancel tool
func (h *Handlers) HandleJobCancel(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract job_id parameter
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	h.logger.WithFields(logrus.Fields{
		"job_id": jobID,
	}).Debug("Canceling job")

	// Cancel job
	if cancelErr := h.manager.CancelJob(jobID); cancelErr != nil {
		h.logger.WithError(cancelErr).Error("Failed to cancel job")
		return mcp.NewToolResultError(fmt.Sprintf("Failed to cancel job: %v", cancelErr)), nil
	}

	h.logger.Info("Job canceled successfully")

	// Return success response
	response := map[string]interface{}{
		"success": true,
		"job_id":  jobID,
		"message": "Job canceled successfully",
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return mcp.NewToolResultError(fmt.Sprintf("Internal error: failed to marshal response: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonResponse)), nil
}

// HandleJobList handles the ssh_job_list tool
func (h *Handlers) HandleJobList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract connection_id parameter
	connectionID, err := req.RequireString("connection_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate connection ID
	if validationErr := validateConnectionID(connectionID); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": connectionID,
	}).Debug("Listing jobs")

	// Get jobs
	jobs := h.manager.ListJobs(connectionID)

	// Convert to response format
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

	response := map[string]interface{}{
		"success:":      true,
		"connection_id": connectionID,
		"jobs":          jobList,
		"count":         len(jobs),
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return mcp.NewToolResultError(fmt.Sprintf("Internal error: failed to marshal response: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonResponse)), nil
}

// HandleClose handles the ssh_close tool
func (h *Handlers) HandleClose(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract parameters
	connectionID, err := req.RequireString("connection_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Validate connection ID
	if validationErr := validateConnectionID(connectionID); validationErr != nil {
		return mcp.NewToolResultError(validationErr.Error()), nil
	}

	h.logger.WithFields(logrus.Fields{
		"connection_id": connectionID,
	}).Info("Closing SSH connection")

	// Close connection
	if closeErr := h.manager.Close(connectionID); closeErr != nil {
		h.logger.WithError(closeErr).Error("Failed to close SSH connection")
		return mcp.NewToolResultError(fmt.Sprintf("Failed to close connection: %v", closeErr)), nil
	}

	h.logger.Info("SSH connection closed successfully")

	// Return success response
	response := map[string]interface{}{
		"success":       true,
		"connection_id": connectionID,
		"message":       "SSH connection closed successfully",
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return mcp.NewToolResultError(fmt.Sprintf("Internal error: failed to marshal response: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonResponse)), nil
}

// HandleList handles the ssh_list tool
func (h *Handlers) HandleList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	h.logger.Debug("Listing active SSH connections")

	// Get list of connections
	connections := h.manager.List()

	h.logger.WithFields(logrus.Fields{
		"count": len(connections),
	}).Debug("Retrieved connection list")

	// Convert to response format
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

	response := map[string]interface{}{
		"success":     true,
		"connections": connList,
		"count":       len(connections),
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal response")
		return mcp.NewToolResultError(fmt.Sprintf("Internal error: failed to marshal response: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonResponse)), nil
}
