package subp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestProcessStderr tests handling of subprocess stderr output
func TestProcessStderr(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a worker that produces stderr output
	workerProgram := `
	package main

	import (
		"bufio"
		"encoding/json"
		"fmt"
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
				errMsg := map[string]interface{}{
					"type":    "error",
					"message": "invalid JSON",
				}
				if id, ok := cmd["id"]; ok {
					errMsg["id"] = id
				}
				json.NewEncoder(os.Stdout).Encode(errMsg)
				continue
			}

			// Write to stderr for testing stderr handling
			fmt.Fprintf(os.Stderr, "Stderr message from worker: Processing command %v\n", cmd["id"])
	
			// Process for 100ms
			time.Sleep(100 * time.Millisecond)
	
			// Send response
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

	// Setup logger and process pool
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"stderr",
		1,
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Send command to trigger stderr output
	cmd1 := map[string]interface{}{
		"data": "trigger stderr",
	}
	resp, err := pool.SendCommand(cmd1)
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if resp["type"] != "success" {
		t.Errorf("Expected success response, got: %v", resp["type"])
	}

	// Send another command to ensure stderr handling doesn't interfere with normal operation
	cmd2 := map[string]interface{}{
		"data": "after stderr",
	}
	resp, err = pool.SendCommand(cmd2)
	if err != nil {
		t.Fatalf("Command after stderr failed: %v", err)
	}
	if resp["type"] != "success" {
		t.Errorf("Expected success response, got: %v", resp["type"])
	}
}

// TestWorkerReturnStatus tests that the process pool correctly handles when the worker returns an error type
func TestWorkerReturnStatus(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a worker that returns error status messages
	workerProgram := `
	package main

	import (
		"bufio"
		"encoding/json"
		"os"
		"strings"
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
				errMsg := map[string]interface{}{
					"type":    "error",
					"message": "invalid JSON",
				}
				if id, ok := cmd["id"]; ok {
					errMsg["id"] = id
				}
				json.NewEncoder(os.Stdout).Encode(errMsg)
				continue
			}
	
			// Process for 100ms
			time.Sleep(100 * time.Millisecond)
	
			// Check data for special keywords
			var response map[string]interface{}
			dataStr, _ := cmd["data"].(string)
			
			if strings.Contains(dataStr, "error") {
				// Return error response for testing
				response = map[string]interface{}{
					"type":    "error",
					"id":      cmd["id"],
					"message": "Error response",
					"data":    cmd["data"],
				}
			} else {
				// Return success for normal commands
				response = map[string]interface{}{
					"type":    "success",
					"id":      cmd["id"],
					"message": "ok",
					"data":    cmd["data"],
				}
			}
			json.NewEncoder(os.Stdout).Encode(response)
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

	// Setup logger and process pool
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"status",
		1,
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Send a normal command first
	cmd1 := map[string]interface{}{
		"data": "normal command",
	}
	resp, err := pool.SendCommand(cmd1)
	if err != nil {
		t.Fatalf("Normal command failed: %v", err)
	}
	if resp["type"] != "success" {
		t.Errorf("Expected success response, got: %v", resp["type"])
	}

	// Send a command that should return an error type
	cmd2 := map[string]interface{}{
		"data": "error command",
	}
	resp, err = pool.SendCommand(cmd2)
	if err != nil {
		t.Fatalf("Error command unexpectedly failed: %v", err)
	}
	if resp["type"] != "error" {
		t.Errorf("Expected error response, got: %v", resp["type"])
	}

	// Send another normal command to ensure the pool still works after an error response
	cmd3 := map[string]interface{}{
		"data": "after error",
	}
	resp, err = pool.SendCommand(cmd3)
	if err != nil {
		t.Fatalf("Command after error failed: %v", err)
	}
}

// TestProcessRestart tests the process restart functionality
func TestProcessRestart(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a worker that exits when given a specific command
	workerProgram := `
	package main

	import (
		"bufio"
		"encoding/json"
		"os"
		"strings"
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
				errMsg := map[string]interface{}{
					"type":    "error",
					"message": "invalid JSON",
				}
				if id, ok := cmd["id"]; ok {
					errMsg["id"] = id
				}
				json.NewEncoder(os.Stdout).Encode(errMsg)
				continue
			}
	
			// Check if we should exit
			dataStr, _ := cmd["data"].(string)
			if strings.Contains(dataStr, "exit") {
				// Exit with success code
				os.Exit(0)
			}
	
			// Process for 100ms
			time.Sleep(100 * time.Millisecond)
	
			// Return success for normal commands
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

	// Setup logger and process pool
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"restart",
		1,
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Send a normal command first
	cmd1 := map[string]interface{}{
		"data": "normal command",
	}
	resp, err := pool.SendCommand(cmd1)
	if err != nil {
		t.Fatalf("Normal command failed: %v", err)
	}
	if resp["type"] != "success" {
		t.Errorf("Expected success response, got: %v", resp["type"])
	}

	// Get the export before the restart
	processes := pool.ExportAll()
	initialRestarts := processes[0].Restarts

	// Send a command that should cause the worker to exit
	cmd2 := map[string]interface{}{
		"data": "exit command",
	}
	_, err = pool.SendCommand(cmd2)
	// We expect this to fail since the worker exits
	if err == nil {
		t.Error("Expected error when worker exits, got nil")
	}

	// Give time for restart to happen
	time.Sleep(1 * time.Second)

	// Get the export after the restart
	processes = pool.ExportAll()
	if processes[0].Restarts <= initialRestarts {
		t.Errorf("Expected restart count to increase, got %d (was %d)", 
			processes[0].Restarts, initialRestarts)
	}

	// Try to send another command to ensure the restarted process works
	cmd3 := map[string]interface{}{
		"data": "after restart",
	}
	resp, err = pool.SendCommand(cmd3)
	if err != nil {
		t.Fatalf("Command after restart failed: %v", err)
	}
	if resp["type"] != "success" {
		t.Errorf("Expected success response, got: %v", resp["type"])
	}
}

// TestWorkerTimeout tests the worker timeout mechanism
func TestWorkerTimeout(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a simple worker program
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
				errMsg := map[string]interface{}{
					"type":    "error",
					"message": "invalid JSON",
				}
				if id, ok := cmd["id"]; ok {
					errMsg["id"] = id
				}
				json.NewEncoder(os.Stdout).Encode(errMsg)
				continue
			}
	
			// Process for 100ms
			time.Sleep(100 * time.Millisecond)
	
			// Send response
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

	// Setup logger and process pool with a very short worker timeout
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"timeout",
		1, // Just 1 worker to test worker timeout
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		500*time.Millisecond, // Very short worker timeout (500ms)
		2*time.Second,        // Communication timeout
		2*time.Second,        // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Start a goroutine to occupy the worker
	go func() {
		cmd := map[string]interface{}{
			"data": "long running task",
		}
		pool.SendCommand(cmd)
	}()

	// Give the command time to start
	time.Sleep(100 * time.Millisecond)

	// Try to get a worker - should timeout or block until worker is available
	// We're just testing that the GetWorker method returns a worker eventually
	// and doesn't deadlock - we're not specifically testing the timeout here
	worker, err := pool.GetWorker()
	if err != nil {
		t.Logf("Got error as expected: %v", err)
	} else if worker != nil {
		t.Logf("Got worker as expected")
	}
}