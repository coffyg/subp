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

// TestSerialCommandExecution verifies that commands to the same subprocess are executed serially, not concurrently
func TestSerialCommandExecution(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "serial_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Worker that tracks concurrent executions
	workerProgram := `
	package main
	
	import (
		"bufio"
		"encoding/json"
		"os"
		"sync/atomic"
		"time"
	)
	
	var concurrentCount int32
	var maxConcurrent int32
	
	func main() {
		// Send ready message
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
				continue
			}
			
			// Increment concurrent counter
			current := atomic.AddInt32(&concurrentCount, 1)
			
			// Track max concurrent
			for {
				old := atomic.LoadInt32(&maxConcurrent)
				if current <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, current) {
					break
				}
			}
			
			// Simulate work
			time.Sleep(200 * time.Millisecond)
			
			// Send response with max concurrent observed
			response := map[string]interface{}{
				"type": "success",
				"id": cmd["id"],
				"max_concurrent": atomic.LoadInt32(&maxConcurrent),
				"current_concurrent": current,
			}
			json.NewEncoder(os.Stdout).Encode(response)
			
			// Decrement concurrent counter
			atomic.AddInt32(&concurrentCount, -1)
		}
	}
	`

	workerFilePath := filepath.Join(tmpDir, "serial_worker.go")
	err = os.WriteFile(workerFilePath, []byte(workerProgram), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Compile the test worker
	workerBinaryPath := filepath.Join(tmpDir, "serial_worker")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile worker program: %v\nOutput: %s", err, string(out))
	}

	// Create a logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create pool with just 1 worker to test serial execution on single subprocess
	pool := NewProcessPool(
		"serial-test",
		1, // Single worker!
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	// Wait for pool to be ready
	err = pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send multiple commands concurrently to the same worker
	numCommands := 5
	var wg sync.WaitGroup
	responses := make([]map[string]interface{}, numCommands)
	errors := make([]error, numCommands)

	for i := 0; i < numCommands; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			cmd := map[string]interface{}{
				"id": fmt.Sprintf("cmd-%d", index),
				"data": index,
			}
			responses[index], errors[index] = pool.SendCommand(cmd)
		}(i)
	}

	// Wait for all commands to complete
	wg.Wait()

	// Check results
	maxConcurrentSeen := int32(0)
	for i, resp := range responses {
		if errors[i] != nil {
			t.Errorf("Command %d failed: %v", i, errors[i])
			continue
		}

		if resp["type"] != "success" {
			t.Errorf("Command %d: expected success, got %v", i, resp["type"])
		}

		// Check max concurrent count reported by worker
		if maxVal, ok := resp["max_concurrent"].(float64); ok {
			if int32(maxVal) > maxConcurrentSeen {
				maxConcurrentSeen = int32(maxVal)
			}
		}
	}

	// CRITICAL CHECK: max_concurrent should always be 1 (serial execution)
	if maxConcurrentSeen != 1 {
		t.Errorf("❌ FAILED: Worker saw %d concurrent commands! Expected 1 (serial execution)", maxConcurrentSeen)
		t.Error("This means commands are being sent concurrently to the same subprocess")
	} else {
		t.Logf("✅ SUCCESS: Commands executed serially (max concurrent = %d)", maxConcurrentSeen)
	}
}

// TestMultipleWorkersAllowConcurrency verifies that different workers can execute concurrently
func TestMultipleWorkersAllowConcurrency(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "concurrent_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Worker that reports timing
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
	
		// Read commands
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
			
			startTime := time.Now()
			
			// Simulate work
			time.Sleep(500 * time.Millisecond)
			
			// Send response with timing
			response := map[string]interface{}{
				"type": "success",
				"id": cmd["id"],
				"start_time": startTime.Unix(),
				"end_time": time.Now().Unix(),
			}
			json.NewEncoder(os.Stdout).Encode(response)
		}
	}
	`

	workerFilePath := filepath.Join(tmpDir, "concurrent_worker.go")
	err = os.WriteFile(workerFilePath, []byte(workerProgram), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Compile
	workerBinaryPath := filepath.Join(tmpDir, "concurrent_worker")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile: %v\nOutput: %s", err, string(out))
	}

	// Create logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create pool with 3 workers
	pool := NewProcessPool(
		"concurrent-test",
		3, // Multiple workers!
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	// Wait for ready
	err = pool.WaitForReady()
	if err != nil {
		t.Fatal(err)
	}

	// Send 3 commands concurrently (should use different workers)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			cmd := map[string]interface{}{
				"id": fmt.Sprintf("cmd-%d", index),
			}
			pool.SendCommand(cmd)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// With 3 workers and 500ms tasks, should complete in ~500ms (concurrent)
	// If serial, would take 1500ms
	if elapsed > 800*time.Millisecond {
		t.Errorf("❌ Commands took %v, expected ~500ms (concurrent execution with 3 workers)", elapsed)
	} else {
		t.Logf("✅ Commands completed in %v (concurrent with multiple workers)", elapsed)
	}
}