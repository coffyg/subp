package timeout

import (
	"testing"
	"time"

	"github.com/fy/coffyg/subp"
	"github.com/rs/zerolog"
)

// TestTimeoutBehavior is a simple test to verify that the ProcessPool properly handles timeouts
func TestTimeoutBehavior(t *testing.T) {
	// These functions are for reference and documentation
	if false {
		RunTimeoutTest()
		RunFixedTest()
	}
	logger := zerolog.Nop()

	// Create a small pool with intentionally short timeouts
	workers := 2
	pool := subp.NewProcessPool(
		"timeout_test",
		workers,
		&logger,
		".",
		"echo", // Use echo as a simple worker that exits immediately
		[]string{"ready"},
		100*time.Millisecond, // Very short worker timeout
		100*time.Millisecond, // Very short communication timeout
		100*time.Millisecond, // Very short init timeout
	)

	// This is expected to fail since the echo workers exit immediately
	err := pool.WaitForReady()
	if err == nil {
		t.Error("Expected timeout error but got nil")
	}
}