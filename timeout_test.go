package subp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestTimeoutMechanism verifies that timeouts actually work when worker doesn't respond
func TestTimeoutMechanism(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "timeout_mechanism_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Worker that sends ready then completely hangs - no response at all
	workerProgram := `
	package main

	import (
		"bufio"
		"encoding/json"
		"os"
		"time"
	)

	func main() {
		// Send ready message
		readyMsg := map[string]interface{}{"type": "ready"}
		json.NewEncoder(os.Stdout).Encode(readyMsg)
		os.Stdout.Sync() // Force flush
	
		// Process commands
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var cmd map[string]interface{}
			json.Unmarshal([]byte(scanner.Text()), &cmd)
			
			// Just hang forever without sending ANY response
			// No newline, no partial response, NOTHING
			for {
				time.Sleep(1 * time.Hour)
			}
		}
	}
	`

	// Save and compile the worker
	workerPath := filepath.Join(tmpDir, "worker.go")
	if err := os.WriteFile(workerPath, []byte(workerProgram), 0644); err != nil {
		t.Fatal(err)
	}

	binaryPath := filepath.Join(tmpDir, "worker")
	cmd := exec.Command("go", "build", "-o", binaryPath, workerPath)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to compile worker: %v", err)
	}

	// Setup logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create process pool with 1 second timeout
	pool := NewProcessPool(
		"timeout-test",
		1,
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		10*time.Second,    // Worker timeout (not relevant here)
		1*time.Second,     // Com timeout - THIS is what should trigger
		5*time.Second,     // Init timeout
	)
	defer pool.StopAll()

	// Wait for pool to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Send command and measure how long it takes to timeout
	startTime := time.Now()
	cmd1 := map[string]interface{}{
		"id":   "test-timeout",
		"type": "main",
		"task": "hang",
	}
	
	t.Logf("Sending command at %v, expecting timeout after 1 second...", startTime)
	
	_, err1 := pool.SendCommand(cmd1)
	duration := time.Since(startTime)
	
	t.Logf("Command returned after %v", duration)
	
	if err1 == nil {
		t.Fatal("Expected timeout error, got nil")
	}
	
	if err1.Error() != "communication timed out" {
		t.Errorf("Expected 'communication timed out' error, got: %v", err1)
	}
	
	// Check that it actually timed out in approximately the right time
	if duration < 900*time.Millisecond {
		t.Errorf("Timeout happened too fast: %v (expected ~1s)", duration)
	}
	
	if duration > 2*time.Second {
		t.Errorf("Timeout took too long: %v (expected ~1s)", duration)
	}
	
	t.Logf("✓ Timeout mechanism worked correctly in %v", duration)
}