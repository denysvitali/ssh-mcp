package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/denysvitali/ssh-mcp/cmd"
	"github.com/denysvitali/ssh-mcp/pkg/mcp"
	"github.com/denysvitali/ssh-mcp/pkg/ssh"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/sirupsen/logrus"
)

var (
	Version = "dev"
)

func main() {
	// Set up the server function
	cmd.ServerFunc = runServer

	// Execute cobra command to parse flags
	cmd.Execute()
}

func runServer() error {
	// Setup logger
	logger, logCleanup, err := cmd.SetupLogger()
	if err != nil {
		return fmt.Errorf("failed to setup logger: %w", err)
	}
	defer func() {
		if logErr := logCleanup(); logErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to close log file: %v\n", logErr)
		}
	}()

	logger.Info("Starting MCP SSH Server")

	// Get allowed hosts
	allowedHosts := cmd.GetAllowedHosts()
	if allowedHosts == "" {
		return fmt.Errorf("--allowed-hosts flag is required")
	}

	// Create host validator
	validator, err := ssh.NewHostValidator(allowedHosts)
	if err != nil {
		return fmt.Errorf("failed to create host validator: %w", err)
	}

	logger.WithFields(logrus.Fields{
		"allowed_hosts": allowedHosts,
	}).Info("Host validator initialized")

	// Get command timeout
	commandTimeout := cmd.GetCommandTimeout()
	logger.WithFields(logrus.Fields{
		"command_timeout": commandTimeout,
	}).Info("Command timeout configured")

	// Create SSH manager with configured timeout
	sshManager := ssh.NewManager(validator, commandTimeout)

	// Create MCP handlers
	handlers := mcp.NewHandlers(sshManager, logger)

	// Create MCP server
	mcpServer := server.NewMCPServer(
		"mcp-ssh",
		Version,
		server.WithToolCapabilities(true),
		server.WithLogging(),
		server.WithRecovery(),
	)

	// Define ssh_connect tool
	connectTool := mcpgo.NewTool(
		"ssh_connect",
		mcpgo.WithDescription("Establish an SSH connection to a remote host"),
		mcpgo.WithString("connection_id",
			mcpgo.Required(),
			mcpgo.Description("Unique identifier for this connection"),
		),
		mcpgo.WithString("host",
			mcpgo.Required(),
			mcpgo.Description("Remote host address (hostname or IP)"),
		),
		mcpgo.WithNumber("port",
			mcpgo.Description("SSH port (default: 22)"),
		),
		mcpgo.WithString("username",
			mcpgo.Required(),
			mcpgo.Description("SSH username"),
		),
		mcpgo.WithString("password",
			mcpgo.Description("SSH password (used for authentication or as passphrase for encrypted private keys)"),
		),
		mcpgo.WithString("private_key_path",
			mcpgo.Description("Path to SSH private key file (optional if using password)"),
		),
	)

	// Define ssh_execute tool
	//nolint:dupl // ssh_execute and ssh_execute_async intentionally share similar parameters
	executeTool := mcpgo.NewTool(
		"ssh_execute",
		mcpgo.WithDescription("Execute a command on an active SSH connection. Environment variables and working directory persist between commands."),
		mcpgo.WithString("connection_id",
			mcpgo.Required(),
			mcpgo.Description("Connection identifier"),
		),
		mcpgo.WithString("command",
			mcpgo.Required(),
			mcpgo.Description("Command to execute"),
		),
		mcpgo.WithNumber("max_lines",
			mcpgo.Description("Maximum number of output lines (0 = unlimited)"),
		),
		mcpgo.WithNumber("max_bytes",
			mcpgo.Description("Maximum number of output bytes (0 = unlimited)"),
		),
		mcpgo.WithBoolean("use_login_shell",
			mcpgo.Description("Use login shell to source profiles (default: false)"),
		),
		mcpgo.WithBoolean("enable_pty",
			mcpgo.Description("Allocate PTY for interactive apps like top, htop (default: false)"),
		),
		mcpgo.WithNumber("pty_cols",
			mcpgo.Description("PTY columns (default: 80)"),
		),
		mcpgo.WithNumber("pty_rows",
			mcpgo.Description("PTY rows (default: 24)"),
		),
	)

	// Define ssh_execute_async tool
	//nolint:dupl // ssh_execute and ssh_execute_async intentionally share similar parameters
	executeAsyncTool := mcpgo.NewTool(
		"ssh_execute_async",
		mcpgo.WithDescription("Execute a command asynchronously and get a job ID for polling status"),
		mcpgo.WithString("connection_id",
			mcpgo.Required(),
			mcpgo.Description("Connection identifier"),
		),
		mcpgo.WithString("command",
			mcpgo.Required(),
			mcpgo.Description("Command to execute"),
		),
		mcpgo.WithNumber("max_lines",
			mcpgo.Description("Maximum number of output lines (0 = unlimited)"),
		),
		mcpgo.WithNumber("max_bytes",
			mcpgo.Description("Maximum number of output bytes (0 = unlimited)"),
		),
		mcpgo.WithBoolean("use_login_shell",
			mcpgo.Description("Use login shell to source profiles (default: false)"),
		),
		mcpgo.WithBoolean("enable_pty",
			mcpgo.Description("Allocate PTY for interactive apps (default: false)"),
		),
		mcpgo.WithNumber("pty_cols",
			mcpgo.Description("PTY columns (default: 80)"),
		),
		mcpgo.WithNumber("pty_rows",
			mcpgo.Description("PTY rows (default: 24)"),
		),
	)

	// Define ssh_job_status tool
	jobStatusTool := mcpgo.NewTool(
		"ssh_job_status",
		mcpgo.WithDescription("Get the status of an asynchronous job"),
		mcpgo.WithString("job_id",
			mcpgo.Required(),
			mcpgo.Description("Job identifier returned by ssh_execute_async"),
		),
	)

	// Define ssh_job_cancel tool
	jobCancelTool := mcpgo.NewTool(
		"ssh_job_cancel",
		mcpgo.WithDescription("Cancel a running job"),
		mcpgo.WithString("job_id",
			mcpgo.Required(),
			mcpgo.Description("Job identifier to cancel"),
		),
	)

	// Define ssh_job_list tool
	jobListTool := mcpgo.NewTool(
		"ssh_job_list",
		mcpgo.WithDescription("List all jobs for a connection"),
		mcpgo.WithString("connection_id",
			mcpgo.Required(),
			mcpgo.Description("Connection identifier"),
		),
	)

	// Define ssh_close tool
	closeTool := mcpgo.NewTool(
		"ssh_close",
		mcpgo.WithDescription("Close an active SSH connection"),
		mcpgo.WithString("connection_id",
			mcpgo.Required(),
			mcpgo.Description("Connection identifier to close"),
		),
	)

	// Define ssh_list tool
	listTool := mcpgo.NewTool(
		"ssh_list",
		mcpgo.WithDescription("List all active SSH connections"),
	)

	// Add tools to server
	mcpServer.AddTool(connectTool, handlers.HandleConnect)
	mcpServer.AddTool(executeTool, handlers.HandleExecute)
	mcpServer.AddTool(executeAsyncTool, handlers.HandleExecuteAsync)
	mcpServer.AddTool(jobStatusTool, handlers.HandleJobStatus)
	mcpServer.AddTool(jobCancelTool, handlers.HandleJobCancel)
	mcpServer.AddTool(jobListTool, handlers.HandleJobList)
	mcpServer.AddTool(closeTool, handlers.HandleClose)
	mcpServer.AddTool(listTool, handlers.HandleList)

	logger.Info("MCP tools registered")

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		logger.WithFields(logrus.Fields{
			"signal": sig.String(),
		}).Info("Received shutdown signal")

		// Close all SSH connections
		logger.Info("Closing all SSH connections")
		sshManager.CloseAll()

		cancel()
	}()

	// Start MCP server with stdio transport
	logger.Info("Starting MCP server on stdio transport")
	if err := server.ServeStdio(mcpServer); err != nil {
		logger.WithError(err).Error("Server error")
		return err
	}

	<-ctx.Done()
	logger.Info("MCP SSH Server stopped")
	return nil
}
