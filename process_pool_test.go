// nyxsub_test.go

package subp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestProcessPool tests the ProcessPool and Process functionality.
func TestProcessPool(t *testing.T) {
	// Create a temporary directory for the test worker program.
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir) // Clean up the temporary directory.

	// Write the test worker program to a file.
	workerProgram := `
	package main
	
	import (
		"bufio"
		"encoding/json"
		"os"
		"time"
	)
	
	func main() {
		// Send a ready message to indicate the process is ready.
		readyMsg := map[string]interface{}{"type": "ready"}
		json.NewEncoder(os.Stdout).Encode(readyMsg)
	
		// Read commands from stdin.
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
	
			var cmd map[string]interface{}
			err := json.Unmarshal([]byte(line), &cmd)
			if err != nil {
				// Send an error message if the JSON is invalid.
				errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
				if id, ok := cmd["id"]; ok {
					errMsg["id"] = id
				}
				json.NewEncoder(os.Stdout).Encode(errMsg)
				continue
			}
	
			// Simulate processing time.
			time.Sleep(100 * time.Millisecond)
	
			// Send a success response.
			response := map[string]interface{}{
				"type":    "success",
				"id":      cmd["id"],
				"message": "ok",
				"data":    cmd["data"],
			}
			json.NewEncoder(os.Stdout).Encode(response)
		}
	}
	`

	workerFilePath := filepath.Join(tmpDir, "test_worker.go")
	err = os.WriteFile(workerFilePath, []byte(workerProgram), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Compile the test worker program.
	workerBinaryPath := filepath.Join(tmpDir, "test_worker")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile worker program: %v\nOutput: %s", err, string(out))
	}

	// Create a logger.
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool.
	pool := NewProcessPool(
		"test",
		2, // Number of processes in the pool.
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second, // Worker timeout.
		2*time.Second, // Communication timeout.
		2*time.Second, // Initialization timeout.
	)

	// Wait for the pool to be ready.
	err = pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send commands to the pool and check the responses.
	for i := 0; i < 5; i++ {
		cmd := map[string]interface{}{
			"data": fmt.Sprintf("test data %d", i),
		}
		response, err := pool.SendCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}

		// Check the response type.
		if response["type"] != "success" {
			t.Errorf("Expected response type 'success', got '%v'", response["type"])
		}

		// Check the response message.
		if response["message"] != "ok" {
			t.Errorf("Expected message 'ok', got '%v'", response["message"])
		}

		// Check the response data.
		if response["data"] != cmd["data"] {
			t.Errorf("Expected data '%v', got '%v'", cmd["data"], response["data"])
		}
	}

	// Stop the process pool.
	pool.StopAll()
}

// TestSequentialStartup tests that processes start sequentially, not in parallel.
func TestSequentialStartup(t *testing.T) {
	// Create a temporary directory for the test worker program.
	tmpDir, err := os.MkdirTemp("", "sequentialstartup_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a test worker program that takes time to become ready
	workerProgram := `
	package main
	
	import (
		"encoding/json"
		"os"
		"time"
	)
	
	func main() {
		// Sleep for 200ms to simulate startup time
		time.Sleep(200 * time.Millisecond)
		
		// Send a ready message to indicate the process is ready.
		readyMsg := map[string]interface{}{"type": "ready"}
		json.NewEncoder(os.Stdout).Encode(readyMsg)
		
		// Keep the process alive briefly for testing
		time.Sleep(100 * time.Millisecond)
	}
	`

	// Write the worker program to a Go file.
	workerFile := filepath.Join(tmpDir, "worker.go")
	if err := os.WriteFile(workerFile, []byte(workerProgram), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a logger.
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Record start time
	start := time.Now()

	// Create a process pool with 3 processes
	const numProcesses = 3
	pool := NewProcessPool(
		"sequential-test",
		numProcesses,
		&logger,
		tmpDir,
		"go",
		[]string{"run", "worker.go"},
		5*time.Second,   // worker timeout
		10*time.Second,  // communication timeout  
		30*time.Second,  // init timeout (generous for sequential startup)
	)
	defer pool.StopAll()

	// Measure total time taken
	totalTime := time.Since(start)

	// Since each process takes ~200ms to become ready and they start sequentially,
	// the total time should be at least numProcesses * 200ms
	expectedMinTime := time.Duration(numProcesses) * 200 * time.Millisecond
	
	if totalTime < expectedMinTime {
		t.Errorf("NewProcessPool returned too quickly. Expected at least %v, got %v", expectedMinTime, totalTime)
		t.Errorf("This suggests processes started in parallel rather than sequentially")
	}

	// Verify all processes are ready (they should be since NewProcessPool waited)
	exports := pool.ExportAll()
	readyCount := 0
	for _, exp := range exports {
		if exp.IsReady {
			readyCount++
		}
	}

	if readyCount != numProcesses {
		t.Errorf("Expected %d processes to be ready, got %d", numProcesses, readyCount)
	}

	// Also verify the upper bound - shouldn't take too much longer than expected
	// (allowing some overhead for process creation)
	expectedMaxTime := expectedMinTime + 1*time.Second
	if totalTime > expectedMaxTime {
		t.Errorf("NewProcessPool took too long. Expected at most %v, got %v", expectedMaxTime, totalTime)
	}
}

// TestInterrupt tests the interrupt functionality.
func TestInterrupt(t *testing.T) {
	// Create a temporary directory for the test worker program.
	tmpDir, err := os.MkdirTemp("", "interrupt_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a test worker program that can handle interrupts and long-running commands
	workerProgram := `
	package main
	
	import (
		"bufio"
		"encoding/json"
		"os"
		"time"
	)
	
	func main() {
		// Send a ready message to indicate the process is ready.
		readyMsg := map[string]interface{}{"type": "ready"}
		json.NewEncoder(os.Stdout).Encode(readyMsg)
		
		// Read commands from stdin.
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
			
			// Handle interrupt message
			if interrupt, ok := cmd["interrupt"]; ok {
				response := map[string]interface{}{
					"type": "interrupt_received",
					"interrupt": interrupt,
				}
				json.NewEncoder(os.Stdout).Encode(response)
				continue
			}
			
			// Handle regular commands
			if cmd["type"] == "long_command" {
				// Simulate a long-running command that can be interrupted
				for i := 0; i < 100; i++ {
					time.Sleep(50 * time.Millisecond)
				}
			}
			
			// Send response
			response := map[string]interface{}{
				"type": "success",
				"id":   cmd["id"],
				"message": "ok",
			}
			json.NewEncoder(os.Stdout).Encode(response)
		}
	}
	`

	// Write the worker program to a Go file.
	workerFile := filepath.Join(tmpDir, "worker.go")
	if err := os.WriteFile(workerFile, []byte(workerProgram), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a logger.
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create a process pool with 1 process
	pool := NewProcessPool(
		"interrupt-test",
		1,
		&logger,
		tmpDir,
		"go",
		[]string{"run", "worker.go"},
		5*time.Second,   // worker timeout
		10*time.Second,  // communication timeout  
		30*time.Second,  // init timeout
	)
	defer pool.StopAll()

	// Start a long-running command in a goroutine
	longCmdID := "test-command-123"
	cmdDone := make(chan bool)
	
	go func() {
		cmd := map[string]interface{}{
			"id":   longCmdID,
			"type": "long_command",
		}
		_, _ = pool.SendCommand(cmd)
		cmdDone <- true
	}()
	
	// Give the command a moment to start
	time.Sleep(100 * time.Millisecond)
	
	// Try to interrupt the command
	interruptErr := pool.Interrupt(longCmdID)
	if interruptErr != nil {
		t.Errorf("Expected interrupt to succeed, got error: %v", interruptErr)
	}
	
	// The interrupt should have been sent, but the original command may still complete
	// We can't easily test the interrupt reception in this setup since the worker
	// continues processing the original command, so let's test error cases instead
	
	// Wait for the command to complete
	select {
	case <-cmdDone:
		// Command completed (expected)
	case <-time.After(6 * time.Second):
		t.Error("Long command did not complete within expected time")
	}
	
	// Test interrupting a non-existent command
	nonExistentErr := pool.Interrupt("non-existent-id")
	if nonExistentErr == nil {
		t.Error("Expected error when interrupting non-existent command")
	}
	
	expectedErrMsg := "no command found with ID: non-existent-id"
	if nonExistentErr.Error() != expectedErrMsg {
		t.Errorf("Expected error message '%s', got '%s'", expectedErrMsg, nonExistentErr.Error())
	}
	
	// Test interrupting a queued command (if we can create that scenario)
	// Create a pool with only 1 worker to force queueing
	smallPool := NewProcessPool(
		"queue-test",
		1,
		&logger,
		tmpDir,
		"go",
		[]string{"run", "worker.go"},
		5*time.Second,
		10*time.Second,
		30*time.Second,
	)
	defer smallPool.StopAll()
	
	// Start a long command that will block the only worker
	go func() {
		blockingCmd := map[string]interface{}{
			"id":   "blocking-cmd",
			"type": "long_command",
		}
		smallPool.SendCommand(blockingCmd)
	}()
	
	// Give it a moment to start
	time.Sleep(100 * time.Millisecond)
	
	// Now try to send another command with predefined ID that should queue
	queuedCmdDone := make(chan bool)
	go func() {
		queuedCmd := map[string]interface{}{
			"id":   "queued-cmd-123",
			"type": "main",
			"data": "this will be queued",
		}
		_, _ = smallPool.SendCommand(queuedCmd)
		queuedCmdDone <- true
	}()
	
	// Give it a moment to queue
	time.Sleep(50 * time.Millisecond)
	
	// Try to interrupt the queued command
	queueInterruptErr := smallPool.Interrupt("queued-cmd-123")
	if queueInterruptErr == nil {
		t.Error("Expected error when interrupting queued command")
	}
	
	expectedQueueErr := "command queued-cmd-123 is queued, cannot interrupt until executing"
	if queueInterruptErr.Error() != expectedQueueErr {
		t.Errorf("Expected queue error '%s', got '%s'", expectedQueueErr, queueInterruptErr.Error())
	}
}
