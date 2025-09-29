package subp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestProcessMetrics verifies success rate and timing metrics are tracked correctly
func TestProcessMetrics(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "metrics_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Worker that can succeed or fail based on task type
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
	
		// Process commands
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
	
			var cmd map[string]interface{}
			err := json.Unmarshal([]byte(line), &cmd)
			if err != nil {
				continue
			}
			
			// Check for interrupt signal
			if _, isInterrupt := cmd["interrupt"]; isInterrupt {
				continue
			}
			
			// Simulate different outcomes based on task type
			switch cmd["task"] {
			case "success":
				// Successful response
				response := map[string]interface{}{
					"type": "success",
					"id":   cmd["id"],
					"task": cmd["task"],
				}
				json.NewEncoder(os.Stdout).Encode(response)
				
			case "fail":
				// Exit abruptly to trigger failure/timeout
				os.Exit(1)
				
			case "slow":
				// Slow but successful
				time.Sleep(100 * time.Millisecond)
				response := map[string]interface{}{
					"type": "success",
					"id":   cmd["id"],
					"task": cmd["task"],
				}
				json.NewEncoder(os.Stdout).Encode(response)
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

	// Create pool with 1 worker to easily test metrics
	pool := NewProcessPool(
		"metrics-test",
		1,
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		5*time.Second,  // worker timeout
		1*time.Second,  // communication timeout (short for fail test)
		5*time.Second,  // init timeout
	)
	defer pool.StopAll()

	// Wait for pool to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Track timing for verification
	startTime := time.Now()

	// Send successful commands
	for i := 0; i < 3; i++ {
		cmd := map[string]interface{}{
			"task": "success",
			"type": "main",
		}
		resp, err := pool.SendCommand(cmd)
		if err != nil {
			t.Errorf("Success command %d failed: %v", i, err)
		}
		if resp != nil && resp["type"] != "success" {
			t.Errorf("Expected success response, got %v", resp["type"])
		}
		time.Sleep(50 * time.Millisecond) // Small delay between commands
	}

	// Get metrics after successes
	exports := pool.ExportAll()
	if len(exports) != 1 {
		t.Fatalf("Expected 1 export, got %d", len(exports))
	}

	process := exports[0]

	// Verify success tracking
	if process.RequestsHandled != 3 {
		t.Errorf("Expected 3 requests handled, got %d", process.RequestsHandled)
	}

	if process.SuccessRate != 1.0 {
		t.Errorf("Expected 100%% success rate after only successes, got %f", process.SuccessRate)
	}

	if process.LastActiveTime == nil {
		t.Error("LastActiveTime should be set after successful commands")
	} else if process.LastActiveTime.Before(startTime) {
		t.Error("LastActiveTime should be after test start time")
	}

	if process.LastErrorTime != nil {
		t.Error("LastErrorTime should be nil after only successful commands")
	}

	// Send a failing command
	failCmd := map[string]interface{}{
		"task": "fail",
		"type": "main",
	}
	_, err = pool.SendCommand(failCmd)
	if err == nil {
		t.Error("Expected failure command to fail")
	}

	// Wait for restart to complete
	time.Sleep(100 * time.Millisecond)
	pool.WaitForReady()

	// Get metrics after failure
	exports = pool.ExportAll()
	process = exports[0]

	// Verify failure tracking
	expectedSuccessRate := 3.0 / 4.0 // 3 successes, 1 failure
	if process.SuccessRate < expectedSuccessRate-0.01 || process.SuccessRate > expectedSuccessRate+0.01 {
		t.Errorf("Expected ~75%% success rate after 3 successes and 1 failure, got %f", process.SuccessRate)
	}

	if process.LastErrorTime == nil {
		t.Error("LastErrorTime should be set after failed command")
	} else if process.LastErrorTime.Before(startTime) {
		t.Error("LastErrorTime should be after test start time")
	}

	if process.LastActiveTime == nil {
		t.Error("LastActiveTime should still be set from previous successes")
	}

	// Verify LastErrorTime is after LastActiveTime (since failure came after successes)
	if process.LastErrorTime != nil && process.LastActiveTime != nil {
		if process.LastErrorTime.Before(*process.LastActiveTime) {
			t.Error("LastErrorTime should be after LastActiveTime since failure came after successes")
		}
	}

	// Send another success to update LastActiveTime
	successCmd := map[string]interface{}{
		"task": "success",
		"type": "main",
	}
	resp, err := pool.SendCommand(successCmd)
	if err != nil {
		t.Errorf("Final success command failed: %v", err)
	}
	if resp != nil && resp["type"] != "success" {
		t.Errorf("Expected success response, got %v", resp["type"])
	}

	// Final metrics check
	exports = pool.ExportAll()
	process = exports[0]

	// Success rate should be 4/5 = 80%
	expectedFinalRate := 4.0 / 5.0
	if process.SuccessRate < expectedFinalRate-0.01 || process.SuccessRate > expectedFinalRate+0.01 {
		t.Errorf("Expected 80%% success rate after 4 successes and 1 failure, got %f", process.SuccessRate)
	}

	// LastActiveTime should be updated and after LastErrorTime
	if process.LastActiveTime == nil {
		t.Error("LastActiveTime should be set after final success")
	} else if process.LastErrorTime != nil && process.LastActiveTime.Before(*process.LastErrorTime) {
		t.Error("LastActiveTime should be after LastErrorTime since success came after failure")
	}

	t.Logf("Test completed successfully:")
	t.Logf("- Success Rate: %.2f%%", process.SuccessRate*100)
	t.Logf("- Last Active: %v", process.LastActiveTime)
	t.Logf("- Last Error: %v", process.LastErrorTime)
	t.Logf("- Total Handled: %d", process.RequestsHandled)
	t.Logf("- Restarts: %d", process.Restarts)
}