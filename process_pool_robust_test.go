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

// TestBasicSendReceive tests basic command sending and receiving
func TestBasicSendReceive(t *testing.T) {
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

	// Setup logger and process pool
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"basic",
		2,
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

	// Send commands
	for i := 0; i < 5; i++ {
		cmd := map[string]interface{}{
			"data": fmt.Sprintf("test data %d", i),
		}
		resp, err := pool.SendCommand(cmd)
		if err != nil {
			t.Fatalf("Command %d failed: %v", i, err)
		}
		if resp["type"] != "success" {
			t.Errorf("Expected success response, got: %v", resp["type"])
		}
		if resp["data"] != cmd["data"] {
			t.Errorf("Data mismatch: sent %v, got %v", cmd["data"], resp["data"])
		}
	}
}

// TestConcurrentCommands tests handling multiple concurrent commands
func TestConcurrentCommands(t *testing.T) {
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

	// Setup logger and process pool
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"concurrent",
		4, // Use 4 workers
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

	// Send concurrent commands
	var wg sync.WaitGroup
	errCh := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := map[string]interface{}{
				"data": fmt.Sprintf("concurrent data %d", i),
			}
			resp, err := pool.SendCommand(cmd)
			if err != nil {
				errCh <- fmt.Errorf("Command %d failed: %v", i, err)
				return
			}
			if resp["type"] != "success" {
				errCh <- fmt.Errorf("Command %d: expected success, got %v", i, resp["type"])
			}
			if resp["data"] != cmd["data"] {
				errCh <- fmt.Errorf("Command %d: data mismatch", i)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		t.Error(err)
	}

	// Verify workload distribution
	processes := pool.ExportAll()
	totalHandled := 0
	for _, p := range processes {
		totalHandled += p.RequestsHandled
	}
	if totalHandled != 10 {
		t.Errorf("Expected 10 commands handled, got %d", totalHandled)
	}
}

// TestSlowCommands tests handling slow commands
func TestSlowCommands(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a worker program that takes longer to process
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
	
			// Process for 1s (slower)
			time.Sleep(1 * time.Second)
	
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
		"slow",
		2, // Just 2 workers
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		3*time.Second,  // Worker timeout
		10*time.Second, // Longer communication timeout
		3*time.Second,  // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()

	// Send 4 commands - should take ~2 seconds (2 rounds of 2 parallel commands)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := map[string]interface{}{
				"data": fmt.Sprintf("slow data %d", i),
			}
			pool.SendCommand(cmd)
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	// Check timing - should be at least 1.8s (allowing some margin)
	if duration < 1800*time.Millisecond {
		t.Errorf("Commands executed too quickly: %v", duration)
	}
}

// TestProcessQueue tests process queue behavior
func TestProcessQueue(t *testing.T) {
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
	
			// Process for 300ms
			time.Sleep(300 * time.Millisecond)
	
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

	// Setup logger and process pool with limited workers
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"queue",
		1, // Just 1 worker to force queueing
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		5*time.Second, // Worker timeout
		5*time.Second, // Communication timeout
		5*time.Second, // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Send multiple commands that need to be queued
	var wg sync.WaitGroup
	errCh := make(chan error, 5)
	
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := map[string]interface{}{
				"data": fmt.Sprintf("queued data %d", i),
			}
			resp, err := pool.SendCommand(cmd)
			if err != nil {
				errCh <- fmt.Errorf("Command %d failed: %v", i, err)
				return
			}
			if resp["type"] != "success" {
				errCh <- fmt.Errorf("Command %d: expected success, got %v", i, resp["type"])
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		t.Error(err)
	}

	// Verify that the single process handled all commands
	processes := pool.ExportAll()
	if len(processes) != 1 {
		t.Fatalf("Expected 1 process, got %d", len(processes))
	}
	if processes[0].RequestsHandled != 5 {
		t.Errorf("Expected 5 commands handled, got %d", processes[0].RequestsHandled)
	}
}

// TestPoolStop tests stopping the process pool
func TestPoolStop(t *testing.T) {
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

	// Setup logger and process pool
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"stop",
		2,
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Send a command to verify it's working
	cmd1 := map[string]interface{}{
		"data": "before stop",
	}
	resp, err := pool.SendCommand(cmd1)
	if err != nil {
		t.Fatalf("Command failed: %v", err)
	}
	if resp["type"] != "success" {
		t.Errorf("Expected success response, got: %v", resp["type"])
	}

	// Stop the pool
	pool.StopAll()

	// Try to send another command
	cmd2 := map[string]interface{}{
		"data": "after stop",
	}
	_, err = pool.SendCommand(cmd2)
	if err == nil {
		t.Error("Expected error after pool stop, got nil")
	}
}