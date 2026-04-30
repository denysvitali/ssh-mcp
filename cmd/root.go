package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	allowedHosts   string
	logLevel       string
	logFile        string
	commandTimeout time.Duration

	// Styles for terminal output
	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF0000")).
			Bold(true)
)

// ServerFunc is the function to run the server (set by main.go)
var ServerFunc func() error

// rootCmd represents the base command
var rootCmd = &cobra.Command{
	Use:   "mcp-ssh",
	Short: "MCP SSH Server - Connect to remote hosts via SSH through MCP protocol",
	Long: `MCP SSH Server is an MCP (Model Context Protocol) server that enables
AI agents to establish and manage SSH connections to remote hosts.

The server maintains persistent SSH sessions, allowing environment variables
and working directory changes to persist across command executions.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if ServerFunc != nil {
			return ServerFunc()
		}
		return nil
	},
}

// Execute runs the root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, errorStyle.Render(fmt.Sprintf("Error: %v", err)))
		os.Exit(1)
	}
}

func init() {
	// Define flags
	rootCmd.PersistentFlags().StringVar(&allowedHosts, "allowed-hosts", "",
		"Comma-separated list of allowed hosts (supports glob patterns, e.g., '*.example.com,10.0.*')")
	//nolint:errcheck // Flag name is hardcoded, this should never fail
	_ = rootCmd.MarkPersistentFlagRequired("allowed-hosts")

	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info",
		"Log level (trace, debug, info, warn, error, fatal, panic)")

	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", "",
		"Log file path (default: stderr)")

	rootCmd.PersistentFlags().DurationVar(&commandTimeout, "command-timeout", 30*time.Second,
		"Default timeout for SSH command execution (e.g., 30s, 1m, 5m)")

	// Set up cobra completion
	rootCmd.CompletionOptions.DisableDefaultCmd = true
}

// GetAllowedHosts returns the allowed hosts flag value
func GetAllowedHosts() string {
	return allowedHosts
}

// GetLogLevel returns the log level flag value
func GetLogLevel() string {
	return logLevel
}

// GetLogFile returns the log file flag value
func GetLogFile() string {
	return logFile
}

// GetCommandTimeout returns the command timeout flag value
func GetCommandTimeout() time.Duration {
	return commandTimeout
}

// SetupLogger configures the logrus logger and returns a cleanup function
func SetupLogger() (*logrus.Logger, func() error, error) {
	logger := logrus.New()

	// Parse log level
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid log level: %w", err)
	}
	logger.SetLevel(level)

	var cleanup func() error

	// Set output
	if logFile != "" {
		// #nosec G304 - Log file path is provided by user via CLI flag
		file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open log file: %w", err)
		}
		logger.SetOutput(file)
		cleanup = func() error {
			return file.Close()
		}
	} else {
		logger.SetOutput(os.Stderr)
		cleanup = func() error {
			return nil
		}
	}

	// Set formatter
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	return logger, cleanup, nil
}
