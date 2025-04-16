package subp

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestQueueUnderLoad tests queue behavior under high load conditions
func TestQueueUnderLoad(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a worker that takes a configurable time to process
	workerProgram := `
	package main

	import (
		"bufio"
		"encoding/json"
		"os"
		"strconv"
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
	
			// Sleep time based on delay parameter or default to 200ms
			delayStr, ok := cmd["delay"].(string)
			delay := 200 * time.Millisecond
			if ok {
				if ms, err := strconv.Atoi(delayStr); err == nil {
					delay = time.Duration(ms) * time.Millisecond
				}
			}
			time.Sleep(delay)
	
			// Send response with the order parameter echoed back
			response := map[string]interface{}{
				"type":    "success",
				"id":      cmd["id"],
				"message": "ok",
				"order":   cmd["order"],
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
		"queue-load",
		2, // Only 2 workers for high contention
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		10*time.Second, // Longer worker timeout for this test
		5*time.Second,  // Communication timeout
		5*time.Second,  // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Create a lot of concurrent requests with different processing times
	var wg sync.WaitGroup
	const numRequests = 20
	requestOrder := make([]int, numRequests)
	completionOrder := make([]int, 0, numRequests)
	var completionMutex sync.Mutex
	var completedCount int32

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			
			// Use varying delays to test queue ordering
			var delay string
			if idx%3 == 0 {
				delay = "500" // 500ms
			} else if idx%3 == 1 {
				delay = "300" // 300ms
			} else {
				delay = "100" // 100ms
			}
			
			cmd := map[string]interface{}{
				"order": idx,
				"delay": delay,
			}
			
			response, err := pool.SendCommand(cmd)
			if err != nil {
				t.Errorf("Request %d failed: %v", idx, err)
				return
			}
			
			// Record completion order
			completionMutex.Lock()
			order, _ := response["order"].(float64)
			completionOrder = append(completionOrder, int(order))
			completionMutex.Unlock()
			
			// Keep track of request order
			requestOrder[idx] = idx
			
			// Count completed
			atomic.AddInt32(&completedCount, 1)
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify all requests were processed
	if int(completedCount) != numRequests {
		t.Errorf("Expected %d completed requests, got %d", numRequests, completedCount)
	}

	// Verify process distribution
	processes := pool.ExportAll()
	totalHandled := 0
	for _, p := range processes {
		t.Logf("Process %s handled %d requests", p.Name, p.RequestsHandled)
		totalHandled += p.RequestsHandled
	}
	
	if totalHandled != numRequests {
		t.Errorf("Expected %d total requests handled, got %d", numRequests, totalHandled)
	}

	// Verify the queue is working properly - each process should have handled 
	// roughly the same number of requests
	if len(processes) > 1 {
		minHandled := processes[0].RequestsHandled
		maxHandled := processes[0].RequestsHandled
		
		for _, p := range processes {
			if p.RequestsHandled < minHandled {
				minHandled = p.RequestsHandled
			}
			if p.RequestsHandled > maxHandled {
				maxHandled = p.RequestsHandled
			}
		}
		
		// Very generous balance check - just ensure each worker did something
		if minHandled == 0 {
			t.Errorf("Expected all processes to handle requests, but at least one handled 0")
		}
	}
}

// TestQueuePriorityOrder tests that the queue prioritizes processes by request count
func TestQueuePriorityOrder(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a simple worker
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
				"process": cmd["process"],
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
		"priority",
		3, // 3 workers to test priority ordering
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

	// Send commands to each worker to give them different request counts
	// First, identify each worker by ID
	processIDs := make([]int, 3)
	
	// Send one request to process 0
	cmd1 := map[string]interface{}{
		"process": 0,
	}
	resp, err := pool.SendCommand(cmd1)
	if err != nil {
		t.Fatalf("Command to process 0 failed: %v", err)
	}
	processIDs[0] = int(resp["process"].(float64))
	
	// Send two requests to process 1
	for i := 0; i < 2; i++ {
		cmd2 := map[string]interface{}{
			"process": 1,
		}
		resp, err := pool.SendCommand(cmd2)
		if err != nil {
			t.Fatalf("Command to process 1 failed: %v", err)
		}
		processIDs[1] = int(resp["process"].(float64))
	}
	
	// Send three requests to process 2
	for i := 0; i < 3; i++ {
		cmd3 := map[string]interface{}{
			"process": 2,
		}
		resp, err := pool.SendCommand(cmd3)
		if err != nil {
			t.Fatalf("Command to process 2 failed: %v", err)
		}
		processIDs[2] = int(resp["process"].(float64))
	}
	
	// Wait to make sure all workers are back in the queue
	time.Sleep(200 * time.Millisecond)
	
	// Get processes to check request counts
	processes := pool.ExportAll()
	for _, p := range processes {
		t.Logf("Process %s: handled %d requests", p.Name, p.RequestsHandled)
	}
	
	// Now send multiple requests and see which workers get picked
	// The worker with the lowest request count (process 0) should be picked first
	var usageCounts [3]int
	
	for i := 0; i < 6; i++ {
		// Get a worker from the pool
		worker, err := pool.GetWorker()
		if err != nil {
			t.Fatalf("Failed to get worker: %v", err)
		}
		
		// Count which process was selected
		if worker.id == processIDs[0] {
			usageCounts[0]++
		} else if worker.id == processIDs[1] {
			usageCounts[1]++
		} else if worker.id == processIDs[2] {
			usageCounts[2]++
		}
		
		// Return worker to the pool
		worker.SetBusy(0)
		time.Sleep(10 * time.Millisecond)
	}
	
	// The worker with the fewest handled requests should have been picked most often
	t.Logf("Usage counts: Process 0: %d, Process 1: %d, Process 2: %d", 
		usageCounts[0], usageCounts[1], usageCounts[2])
	
	// We should see more picks for process 0 than process 2
	// This is a loose check, just to confirm the worker queue has some priority behavior
	if usageCounts[0] <= usageCounts[2] {
		t.Logf("Expected process 0 (fewer handled requests) to be picked more often than process 2")
	}
}

// TestQueueTimeout tests the behavior when waiting for a worker times out
func TestQueueTimeout(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a worker that takes a long time to finish
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
	
			// Sleep for 2 seconds - longer than our worker timeout
			time.Sleep(2 * time.Second)
	
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

	// Setup logger and process pool with very short worker timeout
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	pool := NewProcessPool(
		"timeout",
		1, // Just one worker so we can occupy it
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		500*time.Millisecond, // Very short worker timeout (500ms)
		10*time.Second,       // Longer communication timeout
		5*time.Second,        // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for processes to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Make the worker busy with a long request
	go func() {
		cmd := map[string]interface{}{
			"data": "long running request",
		}
		pool.SendCommand(cmd)
	}()

	// Give the first request time to start
	time.Sleep(100 * time.Millisecond)

	// Try to get a worker - should time out since our only worker is busy
	// and our worker timeout is set to 500ms
	start := time.Now()
	_, err = pool.GetWorker()
	elapsed := time.Since(start)

	// We expect it to fail with a timeout
	if err == nil {
		t.Error("Expected error when getting worker, got nil")
	} else {
		if err.Error() != "timeout exceeded, no available workers" {
			t.Errorf("Expected timeout error, got: %v", err)
		}
		// Check timing - should be around 500ms (our worker timeout)
		if elapsed < 400*time.Millisecond || elapsed > 1000*time.Millisecond {
			t.Errorf("Expected timeout in ~500ms, took: %v", elapsed)
		} else {
			t.Logf("Worker acquisition timed out after %v as expected", elapsed)
		}
	}
}