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

// TestQueueTimeoutTriggersRestart verifies that when queue waits too long for a worker,
// it restarts all workers and continues processing
func TestQueueTimeoutTriggersRestart(t *testing.T) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "processpool_timeout_restart_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Worker that can hang on command
	// First command hangs forever, subsequent commands work normally
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
			
			// First command hangs forever without responding
			if cmd["task"] == "hang" {
				// Don't send any response, just hang forever
				// This simulates a truly stuck worker
				for {
					time.Sleep(1 * time.Hour)
				}
			}
	
			// Normal processing for other commands
			time.Sleep(100 * time.Millisecond)
	
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

	// Create process pool with 1 worker
	// Short worker timeout to trigger restart quickly
	pool := NewProcessPool(
		"timeout_restart_test",
		1, // Only 1 worker
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		2*time.Second,  // Worker timeout - triggers restart after 2s
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
	var hangTask int32
	var normalTasks int32
	restartDetected := false
	
	// Send command that will hang the worker
	wg.Add(1)
	go func() {
		defer wg.Done()
		
		cmd := map[string]interface{}{
			"task": "hang",
		}
		
		startTime := time.Now()
		_, err := pool.SendCommand(cmd)
		duration := time.Since(startTime)
		
		// This should never complete (worker hangs forever)
		if err == nil {
			atomic.AddInt32(&hangTask, 1)
			t.Logf("Hang task completed after %v (unexpected)", duration)
		} else {
			t.Logf("Hang task failed after %v: %v", duration, err)
		}
	}()
	
	// Give hang command time to start and block worker
	time.Sleep(500 * time.Millisecond)
	
	// Send normal commands that will queue up
	// These should trigger timeout->restart->success
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(taskNum int) {
			defer wg.Done()
			
			cmd := map[string]interface{}{
				"task": fmt.Sprintf("normal_%d", taskNum),
			}
			
			startTime := time.Now()
			response, err := pool.SendCommand(cmd)
			duration := time.Since(startTime)
			
			if err != nil {
				t.Logf("Task %d failed after %v: %v", taskNum, duration, err)
				return
			}
			
			if response["type"] == "success" {
				atomic.AddInt32(&normalTasks, 1)
				t.Logf("Task %d completed successfully after %v", taskNum, duration)
				
				// If task took >2s, restart must have happened
				if duration > 2*time.Second {
					restartDetected = true
					t.Logf("Task %d completion time indicates restart occurred ✓", taskNum)
				}
			}
		}(i)
		
		// Small delay between submissions
		time.Sleep(50 * time.Millisecond)
	}
	
	// Wait for timeout to ensure restart triggers
	time.Sleep(3 * time.Second)
	
	// Now send one more task to verify workers are functional after restart
	wg.Add(1)
	go func() {
		defer wg.Done()
		
		cmd := map[string]interface{}{
			"task": "post_restart_verification",
		}
		
		startTime := time.Now()
		response, err := pool.SendCommand(cmd)
		duration := time.Since(startTime)
		
		if err != nil {
			t.Logf("Post-restart task failed after %v: %v", duration, err)
			return
		}
		
		if response["type"] == "success" {
			atomic.AddInt32(&normalTasks, 1)
			t.Logf("Post-restart task completed successfully after %v ✓✓✓", duration)
		}
	}()
	
	// Set reasonable timeout for all tasks
	done := make(chan bool)
	go func() {
		wg.Wait()
		done <- true
	}()
	
	select {
	case <-done:
		// All tasks finished
	case <-time.After(20 * time.Second):
		t.Fatal("Test timed out waiting for tasks to complete")
	}
	
	// Verify results
	hung := atomic.LoadInt32(&hangTask)
	normal := atomic.LoadInt32(&normalTasks)
	
	t.Logf("\n=== FINAL RESULTS ===")
	t.Logf("Hang tasks completed: %d (should be 0)", hung)
	t.Logf("Normal tasks completed: %d (should be 4)", normal)
	t.Logf("Restart detected: %v", restartDetected)
	
	// Verify expectations
	if hung != 0 {
		t.Fatalf("Expected hang task to never complete, but it did")
	}
	
	if normal != 4 {
		t.Fatalf("Expected 4 normal tasks to complete after restart, got %d", normal)
	}
	
	if !restartDetected {
		t.Fatalf("Expected restart to be detected based on task completion times")
	}
	
	t.Logf("\n✅ TEST PASSED: Queue timeout triggered worker restart and recovered!")
	t.Logf("System automatically recovered from hung worker and processed queued commands")
}