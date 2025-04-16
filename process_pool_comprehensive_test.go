package subp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// Helper function to create a test worker program
func createTestWorker(t *testing.T, behavior string) (string, string, func()) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir) // Clean up the temporary directory
	}

	var workerProgram string

	switch behavior {
	case "normal":
		// Standard worker that responds normally
		workerProgram = `
		package main

		import (
			"bufio"
			"encoding/json"
			"os"
			"time"
		)

		func main() {
			// Send a ready message to indicate the process is ready
			readyMsg := map[string]interface{}{"type": "ready"}
			json.NewEncoder(os.Stdout).Encode(readyMsg)
		
			// Read commands from stdin
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}
		
				var cmd map[string]interface{}
				err := json.Unmarshal([]byte(line), &cmd)
				if err != nil {
					// Send an error message if the JSON is invalid
					errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
					if id, ok := cmd["id"]; ok {
						errMsg["id"] = id
					}
					json.NewEncoder(os.Stdout).Encode(errMsg)
					continue
				}
		
				// Simulate processing time
				time.Sleep(100 * time.Millisecond)
		
				// Send a success response
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
	case "slow":
		// Worker that takes a long time to process requests
		workerProgram = `
		package main

		import (
			"bufio"
			"encoding/json"
			"os"
			"time"
		)

		func main() {
			// Send a ready message to indicate the process is ready
			readyMsg := map[string]interface{}{"type": "ready"}
			json.NewEncoder(os.Stdout).Encode(readyMsg)
		
			// Read commands from stdin
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}
		
				var cmd map[string]interface{}
				err := json.Unmarshal([]byte(line), &cmd)
				if err != nil {
					// Send an error message if the JSON is invalid
					errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
					if id, ok := cmd["id"]; ok {
						errMsg["id"] = id
					}
					json.NewEncoder(os.Stdout).Encode(errMsg)
					continue
				}
		
				// Simulate long processing time (1.5 seconds)
				time.Sleep(1500 * time.Millisecond)
		
				// Send a success response
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
	case "timeout":
		// Worker that times out on certain requests
		workerProgram = `
		package main

		import (
			"bufio"
			"encoding/json"
			"os"
			"time"
			"strings"
		)

		func main() {
			// Send a ready message to indicate the process is ready
			readyMsg := map[string]interface{}{"type": "ready"}
			json.NewEncoder(os.Stdout).Encode(readyMsg)
		
			// Read commands from stdin
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}
		
				var cmd map[string]interface{}
				err := json.Unmarshal([]byte(line), &cmd)
				if err != nil {
					// Send an error message if the JSON is invalid
					errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
					if id, ok := cmd["id"]; ok {
						errMsg["id"] = id
					}
					json.NewEncoder(os.Stdout).Encode(errMsg)
					continue
				}
		
				// Check if this command should cause a timeout
				if data, ok := cmd["data"].(string); ok && strings.Contains(data, "timeout") {
					// Just sleep longer than the communication timeout
					time.Sleep(3 * time.Second)
				} else {
					// Regular command processing
					time.Sleep(100 * time.Millisecond)
				}
		
				// Send a success response
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
	case "error":
		// Worker that sends error responses
		workerProgram = `
		package main

		import (
			"bufio"
			"encoding/json"
			"os"
			"strings"
			"time"
		)

		func main() {
			// Send a ready message to indicate the process is ready
			readyMsg := map[string]interface{}{"type": "ready"}
			json.NewEncoder(os.Stdout).Encode(readyMsg)
		
			// Read commands from stdin
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}
		
				var cmd map[string]interface{}
				err := json.Unmarshal([]byte(line), &cmd)
				if err != nil {
					// Send an error message if the JSON is invalid
					errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
					if id, ok := cmd["id"]; ok {
						errMsg["id"] = id
					}
					json.NewEncoder(os.Stdout).Encode(errMsg)
					continue
				}
		
				// Simulate processing time
				time.Sleep(100 * time.Millisecond)
		
				// Send an error response if requested
				if data, ok := cmd["data"].(string); ok && strings.Contains(data, "error") {
					response := map[string]interface{}{
						"type":    "error",
						"id":      cmd["id"],
						"message": "error occurred",
						"data":    cmd["data"],
					}
					json.NewEncoder(os.Stdout).Encode(response)
				} else {
					// Send a success response
					response := map[string]interface{}{
						"type":    "success",
						"id":      cmd["id"],
						"message": "ok",
						"data":    cmd["data"],
					}
					json.NewEncoder(os.Stdout).Encode(response)
				}
			}
		}
		`
	case "stderr":
		// Worker that writes to stderr
		workerProgram = `
		package main

		import (
			"bufio"
			"encoding/json"
			"fmt"
			"os"
			"strings"
			"time"
		)

		func main() {
			// Send a ready message to indicate the process is ready
			readyMsg := map[string]interface{}{"type": "ready"}
			json.NewEncoder(os.Stdout).Encode(readyMsg)
		
			// Read commands from stdin
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}
		
				var cmd map[string]interface{}
				err := json.Unmarshal([]byte(line), &cmd)
				if err != nil {
					// Send an error message if the JSON is invalid
					errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
					if id, ok := cmd["id"]; ok {
						errMsg["id"] = id
					}
					json.NewEncoder(os.Stdout).Encode(errMsg)
					continue
				}
		
				// Write to stderr if requested
				if data, ok := cmd["data"].(string); ok && strings.Contains(data, "stderr") {
					fmt.Fprintf(os.Stderr, "Error message from worker: %s\n", data)
				}
		
				// Simulate processing time
				time.Sleep(100 * time.Millisecond)
		
				// Send a success response
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
	case "crash":
		// Worker that crashes on certain commands
		workerProgram = `
		package main

		import (
			"bufio"
			"encoding/json"
			"os"
			"strings"
			"time"
		)

		func main() {
			// Send a ready message to indicate the process is ready
			readyMsg := map[string]interface{}{"type": "ready"}
			json.NewEncoder(os.Stdout).Encode(readyMsg)
		
			// Read commands from stdin
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}
		
				var cmd map[string]interface{}
				err := json.Unmarshal([]byte(line), &cmd)
				if err != nil {
					// Send an error message if the JSON is invalid
					errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
					if id, ok := cmd["id"]; ok {
						errMsg["id"] = id
					}
					json.NewEncoder(os.Stdout).Encode(errMsg)
					continue
				}
		
				// Exit if requested to crash
				if data, ok := cmd["data"].(string); ok && strings.Contains(data, "crash") {
					os.Exit(1)
				}
		
				// Simulate processing time
				time.Sleep(100 * time.Millisecond)
		
				// Send a success response
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
	}

	// Write the worker program to a file
	workerFilePath := filepath.Join(tmpDir, "test_worker.go")
	err = os.WriteFile(workerFilePath, []byte(workerProgram), 0644)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}

	// Compile the worker program
	workerBinaryPath := filepath.Join(tmpDir, "test_worker")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		cleanup()
		t.Fatalf("Failed to compile worker program: %v\nOutput: %s", err, string(out))
	}

	return tmpDir, workerBinaryPath, cleanup
}

// TestBasicFunctionality tests the basic functionality of the process pool
func TestBasicFunctionality(t *testing.T) {
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "normal")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool
	pool := NewProcessPool(
		"basic",
		2, // Number of processes in the pool
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send commands to the pool and check the responses
	for i := 0; i < 5; i++ {
		cmd := map[string]interface{}{
			"data": fmt.Sprintf("test data %d", i),
		}
		response, err := pool.SendCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}

		// Check the response
		if response["type"] != "success" {
			t.Errorf("Expected response type 'success', got '%v'", response["type"])
		}
		if response["message"] != "ok" {
			t.Errorf("Expected message 'ok', got '%v'", response["message"])
		}
		if response["data"] != cmd["data"] {
			t.Errorf("Expected data '%v', got '%v'", cmd["data"], response["data"])
		}
	}

	// Verify process exports
	exports := pool.ExportAll()
	if len(exports) != 2 {
		t.Errorf("Expected 2 processes, got %d", len(exports))
	}
	for _, export := range exports {
		if !export.IsReady {
			t.Errorf("Expected process to be ready")
		}
		if export.RequestsHandled < 0 || export.RequestsHandled > 5 {
			t.Errorf("Expected requests handled to be between 0 and 5, got %d", export.RequestsHandled)
		}
	}
}

// TestConcurrentRequests tests handling multiple concurrent requests
func TestConcurrentRequests(t *testing.T) {
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "normal")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool with more processes
	pool := NewProcessPool(
		"concurrent",
		5, // More processes for concurrent testing
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		5*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send concurrent commands
	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cmd := map[string]interface{}{
				"data": fmt.Sprintf("concurrent data %d", idx),
			}
			response, err := pool.SendCommand(cmd)
			if err != nil {
				errCh <- fmt.Errorf("request %d failed: %v", idx, err)
				return
			}
			if response["type"] != "success" {
				errCh <- fmt.Errorf("request %d: expected type 'success', got '%v'", idx, response["type"])
			}
			if response["data"] != cmd["data"] {
				errCh <- fmt.Errorf("request %d: data mismatch, expected '%v', got '%v'", idx, cmd["data"], response["data"])
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		t.Error(err)
	}

	// Verify process exports
	exports := pool.ExportAll()
	if len(exports) != 5 {
		t.Errorf("Expected 5 processes, got %d", len(exports))
	}

	// Check that the work was distributed
	totalHandled := 0
	for _, export := range exports {
		if !export.IsReady {
			t.Errorf("Expected process to be ready")
		}
		totalHandled += export.RequestsHandled
	}
	if totalHandled != 20 {
		t.Errorf("Expected 20 total requests handled, got %d", totalHandled)
	}
}

// TestSlowRequests tests handling slow requests
func TestSlowRequests(t *testing.T) {
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "slow")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool
	pool := NewProcessPool(
		"slow",
		3, // Number of processes
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		5*time.Second,  // Worker timeout
		10*time.Second, // Communication timeout (longer for slow workers)
		5*time.Second,  // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send multiple slow requests
	var wg sync.WaitGroup
	errCh := make(chan error, 6)
	start := time.Now()

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cmd := map[string]interface{}{
				"data": fmt.Sprintf("slow request %d", idx),
			}
			response, err := pool.SendCommand(cmd)
			if err != nil {
				errCh <- fmt.Errorf("slow request %d failed: %v", idx, err)
				return
			}
			if response["type"] != "success" {
				errCh <- fmt.Errorf("slow request %d: expected type 'success', got '%v'", idx, response["type"])
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(errCh)
	elapsed := time.Since(start)

	// Check for errors
	for err := range errCh {
		t.Error(err)
	}

	// Verify timing
	// With 3 processes and each taking 1.5s, we expect 6 requests to take about 3s
	// (2 batches of 3 parallel requests)
	if elapsed < 2800*time.Millisecond {
		t.Errorf("Requests completed too quickly: %v", elapsed)
	}

	// Verify exports - make sure latency is recorded
	exports := pool.ExportAll()
	for _, export := range exports {
		if !export.IsReady {
			t.Errorf("Expected process to be ready")
		}
		if export.RequestsHandled == 0 {
			t.Errorf("Expected process to handle requests")
		}
		if export.Latency < 1000 { // Should be at least 1000ms
			t.Errorf("Expected latency to be at least 1000ms, got %d", export.Latency)
		}
	}
}

// TestTimeouts tests process timeout behaviors
func TestTimeouts(t *testing.T) {
	t.Skip("This test is superseded by TestQueueTimeout")
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "timeout")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool with short timeouts
	pool := NewProcessPool(
		"timeout",
		2, // Number of processes
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// First, send normal request to verify everything works
	cmd := map[string]interface{}{
		"data": "normal request",
	}
	response, err := pool.SendCommand(cmd)
	if err != nil {
		t.Fatalf("Normal request failed: %v", err)
	}
	if response["type"] != "success" {
		t.Errorf("Expected response type 'success', got '%v'", response["type"])
	}

	// Now send a request that should timeout
	cmd = map[string]interface{}{
		"data": "timeout request",
	}
	_, err = pool.SendCommand(cmd)
	if err == nil {
		t.Error("Expected timeout error, got nil")
	} else if err.Error() != "communication timed out" {
		t.Errorf("Expected 'communication timed out' error, got '%v'", err)
	}

	// Verify the process was restarted
	time.Sleep(100 * time.Millisecond) // Give time for restart to happen
	exports := pool.ExportAll()
	restartFound := false
	for _, export := range exports {
		if export.Restarts > 0 {
			restartFound = true
			break
		}
	}
	if !restartFound {
		t.Error("Expected at least one process to have restarted")
	}

	// Verify we can still send commands after the timeout
	cmd = map[string]interface{}{
		"data": "after timeout",
	}
	response, err = pool.SendCommand(cmd)
	if err != nil {
		t.Fatalf("Post-timeout request failed: %v", err)
	}
	if response["type"] != "success" {
		t.Errorf("Expected response type 'success', got '%v'", response["type"])
	}
}

// TestProcessRestarts tests process restart behavior
func TestProcessRestarts(t *testing.T) {
	t.Skip("This test is superseded by TestProcessRestart")
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "crash")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool
	pool := NewProcessPool(
		"crash",
		2, // Number of processes
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Get the initial process state
	initialExports := pool.ExportAll()
	initialRestarts := make(map[string]int)
	for _, export := range initialExports {
		initialRestarts[export.Name] = export.Restarts
	}

	// Cause a process to crash
	cmd := map[string]interface{}{
		"data": "crash me",
	}
	_, err = pool.SendCommand(cmd)
	if err == nil {
		t.Error("Expected error when process crashes, got nil")
	}

	// Wait for restart to happen
	time.Sleep(500 * time.Millisecond)

	// Verify the process restarted
	exports := pool.ExportAll()
	restartFound := false
	for _, export := range exports {
		if export.Restarts > initialRestarts[export.Name] {
			restartFound = true
			break
		}
	}
	if !restartFound {
		t.Error("Expected at least one process to have restarted")
	}

	// Verify we can still send commands
	cmd = map[string]interface{}{
		"data": "after crash",
	}
	response, err := pool.SendCommand(cmd)
	if err != nil {
		t.Fatalf("Post-crash request failed: %v", err)
	}
	if response["type"] != "success" {
		t.Errorf("Expected response type 'success', got '%v'", response["type"])
	}
}

// TestErrorHandling tests handling error responses from the subprocess
func TestErrorHandling(t *testing.T) {
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "error")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool
	pool := NewProcessPool(
		"error",
		2, // Number of processes
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send a normal request
	cmd := map[string]interface{}{
		"data": "normal request",
	}
	response, err := pool.SendCommand(cmd)
	if err != nil {
		t.Fatalf("Normal request failed: %v", err)
	}
	if response["type"] != "success" {
		t.Errorf("Expected response type 'success', got '%v'", response["type"])
	}

	// Send a request that should return an error response
	cmd = map[string]interface{}{
		"data": "error request",
	}
	response, err = pool.SendCommand(cmd)
	if err != nil {
		t.Fatalf("Error request failed unexpectedly: %v", err)
	}
	if response["type"] != "error" {
		t.Errorf("Expected response type 'error', got '%v'", response["type"])
	}
	if response["message"] != "error occurred" {
		t.Errorf("Expected error message 'error occurred', got '%v'", response["message"])
	}

	// Verify the process was not restarted (error responses are handled normally)
	exports := pool.ExportAll()
	for _, export := range exports {
		if export.Restarts > 0 {
			t.Errorf("Expected no restarts, but found restarts in %s", export.Name)
		}
	}
}

// TestStderrLogging tests that stderr output is properly logged
func TestStderrLogging(t *testing.T) {
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "stderr")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool
	pool := NewProcessPool(
		"stderr",
		1, // Just need one process
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send a request that causes stderr output
	cmd := map[string]interface{}{
		"data": "trigger stderr output",
	}
	response, err := pool.SendCommand(cmd)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if response["type"] != "success" {
		t.Errorf("Expected response type 'success', got '%v'", response["type"])
	}

	// Can't easily verify the logs here since we're writing to stdout,
	// but at least we can verify the process handled the request correctly
}

// TestWorkerQueueing tests that the worker queue operates correctly
func TestWorkerQueueing(t *testing.T) {
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "normal")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool with fewer processes than we'll have concurrent requests
	pool := NewProcessPool(
		"queue",
		2, // Only 2 processes
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		5*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Track the order of request completions
	var completionOrder []int
	var orderMutex sync.Mutex

	// Send more concurrent requests than we have processes
	var wg sync.WaitGroup
	errCh := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cmd := map[string]interface{}{
				"data": fmt.Sprintf("queued request %d", idx),
			}
			response, err := pool.SendCommand(cmd)
			if err != nil {
				errCh <- fmt.Errorf("request %d failed: %v", idx, err)
				return
			}
			if response["type"] != "success" {
				errCh <- fmt.Errorf("request %d: expected type 'success', got '%v'", idx, response["type"])
			}

			// Record the order of completion
			orderMutex.Lock()
			completionOrder = append(completionOrder, idx)
			orderMutex.Unlock()
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		t.Error(err)
	}

	// Verify the completion order length
	if len(completionOrder) != 6 {
		t.Errorf("Expected 6 completed requests, got %d", len(completionOrder))
	}

	// Verify that workload was distributed
	exports := pool.ExportAll()
	totalHandled := 0
	for _, export := range exports {
		totalHandled += export.RequestsHandled
	}
	if totalHandled != 6 {
		t.Errorf("Expected 6 total requests handled, got %d", totalHandled)
	}
}

// TestProcessPoolStopAll tests that StopAll properly stops all processes
func TestProcessPoolStopAll(t *testing.T) {
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "normal")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool
	pool := NewProcessPool(
		"stop",
		3, // Number of processes
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send a few commands to make sure everything is working
	for i := 0; i < 3; i++ {
		cmd := map[string]interface{}{
			"data": fmt.Sprintf("test before stop %d", i),
		}
		_, err := pool.SendCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Stop all processes
	pool.StopAll()

	// Verify that further commands fail
	cmd := map[string]interface{}{
		"data": "test after stop",
	}
	_, err = pool.SendCommand(cmd)
	if err == nil {
		t.Error("Expected error after StopAll, got nil")
	}
}

// TestProcessPoolWorkerTimeout tests that worker timeout works correctly
func TestProcessPoolWorkerTimeout(t *testing.T) {
	t.Skip("This test is superseded by TestWorkerTimeout")
	tmpDir, workerBinaryPath, cleanup := createTestWorker(t, "normal")
	defer cleanup()

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool with a very short worker timeout
	pool := NewProcessPool(
		"workertimeout",
		1, // Just one process
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		500*time.Millisecond, // Very short worker timeout
		2*time.Second,        // Communication timeout
		2*time.Second,        // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for the pool to be ready
	err := pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Make the process busy
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cmd := map[string]interface{}{
			"data": "make busy",
		}
		pool.SendCommand(cmd)
	}()

	// Wait a bit to ensure the process is busy
	time.Sleep(50 * time.Millisecond)

	// Try to get another worker - should timeout
	worker, err := pool.GetWorker()
	if err == nil {
		t.Errorf("Expected worker timeout error, got nil (worker: %v)", worker)
	} else if err.Error() != "timeout exceeded, no available workers" {
		t.Errorf("Expected 'timeout exceeded' error, got: %v", err)
	}

	// Wait for the first command to finish
	wg.Wait()

	// Note: we don't check if we can get a worker after the first command finishes
	// because the worker might have been restarted after timeout and may not be ready yet
	// or the timing is unpredictable. The important part is that the timeout works.
}