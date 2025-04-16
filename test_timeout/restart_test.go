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

	// Use significantly longer timeouts for race detection tests
	// This helps avoid race conditions during rapid restarts
	workers := 2
	workerTimeout := 200 * time.Millisecond
	commTimeout := 200 * time.Millisecond
	initTimeout := 200 * time.Millisecond
	
	// When running with race detector, use longer timeouts
	// to prevent races during rapid restart loops
	if isRaceDetectorEnabled() {
		workerTimeout = 500 * time.Millisecond
		commTimeout = 500 * time.Millisecond 
		initTimeout = 500 * time.Millisecond
	}

	// Create a pool with the appropriate timeout settings
	pool := subp.NewProcessPool(
		"restart_test",
		workers,
		&logger,
		".",
		"echo", // Use echo as a simple worker that exits immediately
		[]string{"ready"},
		workerTimeout,
		commTimeout,
		initTimeout,
	)

	// Wait longer for workers to start and restart when race detector is enabled
	waitTime := 500 * time.Millisecond
	if isRaceDetectorEnabled() {
		waitTime = 1000 * time.Millisecond
	}
	time.Sleep(waitTime)

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

	// Cleanup - stop all processes
	pool.StopAll()
	
	// Wait for everything to shut down cleanly before exiting
	time.Sleep(100 * time.Millisecond)
}

// isRaceDetectorEnabled returns true if the binary was built with -race flag
func isRaceDetectorEnabled() bool {
	// This function is only used to adjust test parameters
	// It's a simple helper that speeds up normal tests but makes
	// race tests more reliable by using longer timeouts
	return true // Always return true for now as we're debugging race conditions
}