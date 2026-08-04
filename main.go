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
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
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
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "mcp-ssh",
		Version: Version,
	}, nil)

	// Register tools
	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "ssh_connect",
		Description: "Establish an SSH connection to a remote host",
	}, handlers.HandleConnect)

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "ssh_execute",
		Description: "Execute a command on an active SSH connection. Environment variables and working directory persist between commands.",
	}, handlers.HandleExecute)

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "ssh_execute_async",
		Description: "Execute a command asynchronously and get a job ID for polling status",
	}, handlers.HandleExecuteAsync)

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "ssh_job_status",
		Description: "Get the status of an asynchronous job",
	}, handlers.HandleJobStatus)

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "ssh_job_cancel",
		Description: "Cancel a running job",
	}, handlers.HandleJobCancel)

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "ssh_job_list",
		Description: "List all jobs for a connection",
	}, handlers.HandleJobList)

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "ssh_close",
		Description: "Close an active SSH connection",
	}, handlers.HandleClose)

	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "ssh_list",
		Description: "List all active SSH connections",
	}, handlers.HandleList)

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
	if err := mcpServer.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		logger.WithError(err).Error("Server error")
		return err
	}

	logger.Info("MCP SSH Server stopped")
	return nil
}
