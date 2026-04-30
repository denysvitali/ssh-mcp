package ssh

import (
	"testing"
)

func TestNewHostValidator(t *testing.T) {
	tests := []struct {
		name          string
		allowedHosts  string
		expectError   bool
		errorContains string
	}{
		{
			name:         "single host",
			allowedHosts: "example.com",
			expectError:  false,
		},
		{
			name:         "multiple hosts",
			allowedHosts: "example.com,test.com,localhost",
			expectError:  false,
		},
		{
			name:         "wildcard domain",
			allowedHosts: "*.example.com",
			expectError:  false,
		},
		{
			name:         "IP wildcard",
			allowedHosts: "192.168.1.*",
			expectError:  false,
		},
		{
			name:         "mixed patterns",
			allowedHosts: "*.example.com,192.168.*,localhost",
			expectError:  false,
		},
		{
			name:          "empty string",
			allowedHosts:  "",
			expectError:   true,
			errorContains: "no allowed hosts specified",
		},
		{
			name:          "only spaces",
			allowedHosts:  "   ",
			expectError:   true,
			errorContains: "no valid host patterns provided",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator, err := NewHostValidator(tt.allowedHosts)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got nil")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error containing %q but got %q", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if validator == nil {
					t.Errorf("expected non-nil validator")
				}
			}
		})
	}
}

func TestHostValidator_Validate(t *testing.T) {
	tests := []struct {
		name         string
		allowedHosts string
		testHost     string
		expectError  bool
	}{
		// Exact match tests
		{
			name:         "exact match single host",
			allowedHosts: "example.com",
			testHost:     "example.com",
			expectError:  false,
		},
		{
			name:         "exact match multiple hosts",
			allowedHosts: "example.com,test.com",
			testHost:     "test.com",
			expectError:  false,
		},
		{
			name:         "no match",
			allowedHosts: "example.com",
			testHost:     "evil.com",
			expectError:  true,
		},

		// Wildcard tests
		{
			name:         "wildcard subdomain match",
			allowedHosts: "*.example.com",
			testHost:     "www.example.com",
			expectError:  false,
		},
		{
			name:         "wildcard subdomain match deep",
			allowedHosts: "*.example.com",
			testHost:     "api.staging.example.com",
			expectError:  false,
		},
		{
			name:         "wildcard subdomain no match parent",
			allowedHosts: "*.example.com",
			testHost:     "example.com",
			expectError:  true,
		},
		{
			name:         "wildcard subdomain no match different domain",
			allowedHosts: "*.example.com",
			testHost:     "example.org",
			expectError:  true,
		},

		// IP wildcard tests
		{
			name:         "IP wildcard last octet",
			allowedHosts: "192.168.1.*",
			testHost:     "192.168.1.100",
			expectError:  false,
		},
		{
			name:         "IP wildcard last octet no match",
			allowedHosts: "192.168.1.*",
			testHost:     "192.168.2.100",
			expectError:  true,
		},
		{
			name:         "IP wildcard multiple octets",
			allowedHosts: "192.168.*",
			testHost:     "192.168.1.100",
			expectError:  false,
		},
		{
			name:         "IP wildcard full match",
			allowedHosts: "10.*",
			testHost:     "10.0.0.1",
			expectError:  false,
		},

		// Edge cases
		{
			name:         "empty host",
			allowedHosts: "example.com",
			testHost:     "",
			expectError:  true,
		},
		{
			name:         "localhost",
			allowedHosts: "localhost",
			testHost:     "localhost",
			expectError:  false,
		},
		{
			name:         "case sensitivity",
			allowedHosts: "example.com",
			testHost:     "EXAMPLE.COM",
			expectError:  true, // Glob matching is case-sensitive
		},

		// Mixed patterns
		{
			name:         "mixed patterns match first",
			allowedHosts: "*.example.com,192.168.*,localhost",
			testHost:     "www.example.com",
			expectError:  false,
		},
		{
			name:         "mixed patterns match second",
			allowedHosts: "*.example.com,192.168.*,localhost",
			testHost:     "192.168.1.1",
			expectError:  false,
		},
		{
			name:         "mixed patterns match third",
			allowedHosts: "*.example.com,192.168.*,localhost",
			testHost:     "localhost",
			expectError:  false,
		},
		{
			name:         "mixed patterns no match",
			allowedHosts: "*.example.com,192.168.*,localhost",
			testHost:     "evil.com",
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator, err := NewHostValidator(tt.allowedHosts)
			if err != nil {
				t.Fatalf("failed to create validator: %v", err)
			}

			err = validator.Validate(tt.testHost)
			if tt.expectError && err == nil {
				t.Errorf("expected error but got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestHostValidator_ValidateConcurrent(t *testing.T) {
	validator, err := NewHostValidator("*.example.com,192.168.*")
	if err != nil {
		t.Fatalf("failed to create validator: %v", err)
	}

	// Test concurrent validation
	hosts := []string{
		"www.example.com",
		"api.example.com",
		"192.168.1.1",
		"192.168.2.2",
	}

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for _, host := range hosts {
				_ = validator.Validate(host)
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && len(substr) > 0 && containsHelper(s, substr)))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
