package subp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestRealQueueBehavior proves that commands are now properly queued when workers are busy
// Previously this would timeout, now it queues and processes all commands
func TestRealQueueBehavior(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_queue_failure_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Worker that takes 10 seconds to process each command
	// This ensures workers stay busy longer than the timeout
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
	
			// Simulate LONG work (10 seconds)
			time.Sleep(10 * time.Second)
	
			// Send response
			response := map[string]interface{}{
				"type":    "success",
				"id":      cmd["id"],
				"message": "completed after 10s",
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

	// Setup logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create process pool with only 1 worker
	// Worker timeout is 2 seconds (much shorter than task duration of 10s)
	pool := NewProcessPool(
		"queue_failure_test",
		1, // Only 1 worker
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		2*time.Second,  // Worker timeout - SHORT!
		15*time.Second, // Com timeout
		5*time.Second,  // Init timeout
	)
	defer pool.StopAll()

	// Wait for worker to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatalf("Failed to wait for worker: %v", err)
	}

	// Track results
	var wg sync.WaitGroup
	var timeoutErrors int32
	var successfulTasks int32
	
	// Send 3 commands concurrently
	// First one will start executing (takes 10s)
	// Second and third will try to get worker but timeout after 2s
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(taskNum int) {
			defer wg.Done()
			
			startTime := time.Now()
			cmd := map[string]interface{}{
				"task": fmt.Sprintf("task_%d", taskNum),
			}
			
			response, err := pool.SendCommand(cmd)
			duration := time.Since(startTime)
			
			if err != nil {
				if err.Error() == "timeout exceeded, no available workers" {
					atomic.AddInt32(&timeoutErrors, 1)
					t.Logf("Task %d TIMED OUT after %v: %v ✓✓✓ THIS IS THE BUG!", 
						taskNum, duration, err)
				} else {
					t.Logf("Task %d failed with unexpected error after %v: %v", 
						taskNum, duration, err)
				}
				return
			}
			
			if response["type"] == "success" {
				atomic.AddInt32(&successfulTasks, 1)
				t.Logf("Task %d completed successfully after %v", taskNum, duration)
			}
		}(i)
		
		// Small delay to ensure ordering
		time.Sleep(50 * time.Millisecond)
	}
	
	// Wait for all commands to finish/timeout
	wg.Wait()
	
	// Verify results
	timeouts := atomic.LoadInt32(&timeoutErrors)
	successes := atomic.LoadInt32(&successfulTasks)
	
	t.Logf("\n=== FINAL RESULTS ===")
	t.Logf("Successful tasks: %d", successes)
	t.Logf("Timeout errors: %d", timeouts)
	
	// With real queue, we expect:
	// - 3 successful tasks (all queued and processed sequentially)
	// - 0 timeout errors
	if timeouts != 0 {
		t.Fatalf("Expected 0 timeout errors, got %d", timeouts)
	}
	
	if successes != 3 {
		t.Fatalf("Expected 3 successful tasks, got %d", successes)
	}
	
	t.Logf("\n✅ TEST PROVES THE FIX: Commands are now properly queued!")
	t.Logf("Even with short worker timeout (2s) and long tasks (10s), all commands complete successfully")
}

// TestProperQueueBehavior shows what SHOULD happen with a real queue
func TestProperQueueBehavior(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_proper_queue_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Worker that takes 1 second per task (fast enough to process all within timeout)
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
	
			// Simulate quick work (1 second)
			time.Sleep(1 * time.Second)
	
			// Send response
			response := map[string]interface{}{
				"type":    "success",
				"id":      cmd["id"],
				"message": "completed",
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

	// Setup logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create process pool with only 1 worker
	// Worker timeout is 10 seconds (enough time for all tasks to complete)
	pool := NewProcessPool(
		"proper_queue_test",
		1, // Only 1 worker
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		10*time.Second, // Worker timeout - LONG enough for queue to work
		15*time.Second, // Com timeout
		5*time.Second,  // Init timeout
	)
	defer pool.StopAll()

	// Wait for worker to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatalf("Failed to wait for worker: %v", err)
	}

	// Track results
	var wg sync.WaitGroup
	var successfulTasks int32
	taskOrder := make([]int, 0, 5)
	orderMutex := sync.Mutex{}
	
	// Send 5 commands concurrently
	// With 1 worker and 1s per task, this takes 5 seconds total
	// But worker timeout is 10s, so all should succeed
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(taskNum int) {
			defer wg.Done()
			
			startTime := time.Now()
			cmd := map[string]interface{}{
				"task": fmt.Sprintf("task_%d", taskNum),
			}
			
			response, err := pool.SendCommand(cmd)
			duration := time.Since(startTime)
			
			if err != nil {
				t.Logf("Task %d failed after %v: %v", taskNum, duration, err)
				return
			}
			
			if response["type"] == "success" {
				atomic.AddInt32(&successfulTasks, 1)
				orderMutex.Lock()
				taskOrder = append(taskOrder, taskNum)
				orderMutex.Unlock()
				t.Logf("Task %d completed successfully after %v", taskNum, duration)
			}
		}(i)
		
		// Small delay to ensure ordering
		time.Sleep(50 * time.Millisecond)
	}
	
	// Wait for all commands to finish
	wg.Wait()
	
	// Verify results
	successes := atomic.LoadInt32(&successfulTasks)
	
	t.Logf("\n=== FINAL RESULTS ===")
	t.Logf("Successful tasks: %d", successes)
	t.Logf("Task completion order: %v", taskOrder)
	
	// All 5 tasks should succeed because worker timeout is long enough
	if successes != 5 {
		t.Fatalf("Expected 5 successful tasks, got %d", successes)
	}
	
	t.Logf("\n✅ With sufficient worker timeout, tasks wait in 'queue' (GetWorker blocks)")
	t.Logf("But this is NOT a real queue - it's just blocking until timeout!")
}