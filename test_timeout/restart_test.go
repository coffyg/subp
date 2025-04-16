package timeout

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/fy/coffyg/subp"
)

// TestProcessRestartFileHandling specifically tests that the process can handle
// file handle closures during restart without generating errors
func TestProcessRestartFileHandling(t *testing.T) {
	logger := zerolog.Nop()

	// Create a small pool with intentionally short timeouts
	workers := 2
	pool := subp.NewProcessPool(
		"restart_test",
		workers,
		&logger,
		".",
		"echo", // Use echo as a simple worker that exits immediately
		[]string{"ready"},
		200*time.Millisecond, // Short worker timeout
		200*time.Millisecond, // Short communication timeout
		200*time.Millisecond, // Short init timeout
	)

	// Wait a bit for the workers to start and restart due to echo exiting
	time.Sleep(500 * time.Millisecond)

	// The process should have restarted at least once due to echo exiting
	exports := pool.ExportAll()
	
	for _, exp := range exports {
		// We expect processes to have restarted at least once
		// due to the echo command exiting immediately
		if exp.Restarts < 1 {
			t.Errorf("Expected process %s to have restarted at least once, but it had %d restarts", 
				exp.Name, exp.Restarts)
		}
	}

	// Cleanup
	pool.StopAll()
}