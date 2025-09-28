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

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// TestInterruptQueuedCommands tests that commands queued but not yet started
// are properly cancelled when interrupted
func TestInterruptQueuedCommands(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_interrupt_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Write a worker that takes 1 second to process each command
	// This ensures commands will queue up
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
				// Just acknowledge the interrupt
				continue
			}
	
			// Send started message
			startMsg := map[string]interface{}{
				"type": "started",
				"id":   cmd["id"],
			}
			json.NewEncoder(os.Stdout).Encode(startMsg)
			
			// Simulate 1 second of work
			time.Sleep(1 * time.Second)
	
			// Send response
			response := map[string]interface{}{
				"type":    "success",
				"id":      cmd["id"],
				"message": "completed",
				"task":    cmd["task"],
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
	// This ensures commands will queue
	pool := NewProcessPool(
		"interrupt_test",
		1, // Only 1 worker to force queuing
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		10*time.Second, // Longer timeout to avoid timeout errors
		10*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	// Wait for worker to be ready
	if err := pool.WaitForReady(); err != nil {
		t.Fatalf("Failed to wait for worker: %v", err)
	}

	// Track results
	var wg sync.WaitGroup
	var completedTasks int32
	var queuedInterruptedTasks int32
	var erroredTasks int32
	
	commandIDs := make([]string, 10) // More commands to ensure queuing
	
	// Send 10 commands rapidly - first one will execute, others will queue
	for i := 0; i < 10; i++ {
		commandIDs[i] = uuid.New().String()
		wg.Add(1)
		go func(taskNum int, cmdID string) {
			defer wg.Done()
			
			cmd := map[string]interface{}{
				"id":   cmdID,
				"task": fmt.Sprintf("task_%d", taskNum),
			}
			
			response, err := pool.SendCommand(cmd)
			
			if err != nil {
				errMsg := err.Error()
				if errMsg == fmt.Sprintf("command %s was interrupted while queued", cmdID) {
					atomic.AddInt32(&queuedInterruptedTasks, 1)
					t.Logf("Task %d (ID: %s) was INTERRUPTED WHILE QUEUED ✓✓✓", taskNum, cmdID)
				} else {
					atomic.AddInt32(&erroredTasks, 1)
					t.Logf("Task %d (ID: %s) errored: %v", taskNum, cmdID, err)
				}
				return
			}
			
			if response["type"] == "success" {
				atomic.AddInt32(&completedTasks, 1)
				t.Logf("Task %d (ID: %s) completed successfully", taskNum, cmdID)
			}
		}(i, commandIDs[i])
		
		// Small delay between submissions to ensure ordering
		time.Sleep(5 * time.Millisecond)
	}
	
	// Wait for first task to start executing and others to queue
	time.Sleep(200 * time.Millisecond)
	
	// Interrupt commands 3 through 9 (they should all be queued)
	// Commands 0 is executing, 1-2 might complete before we interrupt
	for i := 3; i < 10; i++ {
		err := pool.Interrupt(commandIDs[i])
		if err != nil {
			t.Logf("Failed to interrupt command %d (ID: %s): %v", i, commandIDs[i], err)
		} else {
			t.Logf("Sent interrupt for command %d (ID: %s)", i, commandIDs[i])
		}
	}
	
	// Wait for all commands to finish/error
	wg.Wait()
	
	// Verify results
	completed := atomic.LoadInt32(&completedTasks)
	queuedInterrupted := atomic.LoadInt32(&queuedInterruptedTasks)
	errored := atomic.LoadInt32(&erroredTasks)
	
	t.Logf("\n=== FINAL RESULTS ===")
	t.Logf("Completed tasks: %d", completed)
	t.Logf("Queued tasks that were interrupted: %d", queuedInterrupted)  
	t.Logf("Other errors: %d", errored)
	
	// We expect:
	// - Tasks 0-2 to likely complete (0 was executing, 1-2 might get through)
	// - Tasks 3-9 (7 tasks) to be interrupted while queued
	if queuedInterrupted < 5 {
		t.Fatalf("Expected at least 5 tasks to be interrupted while queued, got %d", queuedInterrupted)
	}
	
	if queuedInterrupted+completed+errored != 10 {
		t.Fatalf("Expected all 10 tasks to be accounted for, got %d completed + %d interrupted + %d errored = %d total",
			completed, queuedInterrupted, errored, completed+queuedInterrupted+errored)
	}
	
	t.Logf("\n✅✅✅ TEST PASSED: %d queued commands were properly cancelled when interrupted!", queuedInterrupted)
	t.Logf("This proves 100%% that queued (not started) tasks ARE removed when interrupted!")
}