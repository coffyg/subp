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

// TestInterrupt tests the interrupt functionality for executing commands.
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
	
	var (
		currentTaskID   string
		interrupted     bool
		msgChan         = make(chan map[string]interface{}, 10)
	)
	
	func main() {
		// Send a ready message to indicate the process is ready.
		readyMsg := map[string]interface{}{"type": "ready"}
		json.NewEncoder(os.Stdout).Encode(readyMsg)
	
		// Start message reader goroutine
		go func() {
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
				msgChan <- cmd
			}
		}()
	
		// Main message processing loop
		for cmd := range msgChan {
			// Check for interrupt command
			if interrupt, ok := cmd["interrupt"]; ok && interrupt == "²INTERUPT²" {
				// Debug: log interrupt received
				json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
					"debug": "interrupt received",
					"currentTaskID": currentTaskID,
				})
				if currentTaskID != "" {
					interrupted = true
				}
				continue
			}
	
			// Handle regular commands
			currentTaskID = cmd["id"].(string)
			interrupted = false
			
			// Check if this is a long-running command
			if duration, ok := cmd["sleep"]; ok {
				sleepTime := time.Duration(duration.(float64)) * time.Millisecond
				
				// Start processing and signal we've started
				startMsg := map[string]interface{}{
					"type": "started",
					"id": cmd["id"],
				}
				json.NewEncoder(os.Stdout).Encode(startMsg)
				
				// Sleep with proper interrupt handling using select
				timer := time.NewTimer(sleepTime)
				
				select {
				case <-timer.C:
					// Sleep completed normally
					json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
						"debug": "sleep completed normally",
						"taskID": cmd["id"],
					})
					interrupted = false
				case interruptCmd := <-msgChan:
					timer.Stop()
					json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
						"debug": "got message during sleep",
						"msg": interruptCmd,
					})
					if interrupt, ok := interruptCmd["interrupt"]; ok && interrupt == "²INTERUPT²" {
						interrupted = true
					} else {
						// Got another command during sleep - ignore it
						interrupted = false
					}
				}
				
				if interrupted {
					// Task was interrupted - send interrupted response
					response := map[string]interface{}{
						"type":    "interrupted", 
						"id":      cmd["id"],
						"message": "task was interrupted",
					}
					json.NewEncoder(os.Stdout).Encode(response)
					currentTaskID = ""
					continue
				}
			} else {
				// Regular quick command - simulate processing time
				time.Sleep(100 * time.Millisecond)
				
				if interrupted {
					// Quick task interrupted
					response := map[string]interface{}{
						"type":    "interrupted",
						"id":      cmd["id"], 
						"message": "task was interrupted",
					}
					json.NewEncoder(os.Stdout).Encode(response)
					currentTaskID = ""
					continue
				}
			}
	
			// Send success response only if not interrupted
			if !interrupted {
				response := map[string]interface{}{
					"type":    "success",
					"id":      cmd["id"],
					"message": "ok",
					"data":    cmd["data"],
				}
				json.NewEncoder(os.Stdout).Encode(response)
			}
			
			currentTaskID = ""
		}
	}
	`

	workerFilePath := filepath.Join(tmpDir, "interrupt_worker.go")
	err = os.WriteFile(workerFilePath, []byte(workerProgram), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Use the proper concurrent worker implementation
	properWorkerProgram := `
	package main
	
	import (
		"bufio"
		"context"
		"encoding/json"
		"os"
		"sync"
		"time"
	)
	
	var (
		currentTask     map[string]interface{}
		currentCancel   context.CancelFunc
		taskMutex       sync.Mutex
	)
	
	func main() {
		// Send a ready message to indicate the process is ready.
		readyMsg := map[string]interface{}{"type": "ready"}
		json.NewEncoder(os.Stdout).Encode(readyMsg)
	
		// Channel for messages from stdin
		msgChan := make(chan map[string]interface{}, 10)
		
		// Start message reader goroutine - ALWAYS reading stdin
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}
				
				// DEBUG: Log every message received
				json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
					"debug": "stdin received",
					"message": line,
				})
		
				var cmd map[string]interface{}
				err := json.Unmarshal([]byte(line), &cmd)
				if err != nil {
					continue
				}
				
				// DEBUG: Log message parsed and queued
				json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
					"debug": "message queued",
					"type": cmd["type"],
					"id": cmd["id"],
					"interrupt": cmd["interrupt"],
				})
				
				msgChan <- cmd
			}
		}()
	
		// Main message processing loop
		for cmd := range msgChan {
			// DEBUG: Log message being processed
			json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
				"debug": "processing message",
				"type": cmd["type"],
				"interrupt": cmd["interrupt"],
			})
			
			// Check for interrupt command
			if interrupt, ok := cmd["interrupt"]; ok && interrupt == "²INTERUPT²" {
				json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
					"debug": "interrupt command processed - calling cancel",
				})
				
				taskMutex.Lock()
				if currentCancel != nil {
					// Cancel the current task immediately
					currentCancel()
					json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
						"debug": "context cancelled",
					})
				} else {
					json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
						"debug": "no task to cancel",
					})
				}
				taskMutex.Unlock()
				continue
			}
	
			// Handle regular commands - run in separate goroutine so we can keep reading stdin
			go func(cmd map[string]interface{}) {
				json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
					"debug": "task goroutine started",
					"id": cmd["id"],
				})
				
				taskMutex.Lock()
				ctx, cancel := context.WithCancel(context.Background())
				currentTask = cmd
				currentCancel = cancel
				taskMutex.Unlock()
				
				json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
					"debug": "task context created, cancel function set",
					"id": cmd["id"],
				})
				
				// Check if this is a long-running command
				if duration, ok := cmd["sleep"]; ok {
					sleepTime := time.Duration(duration.(float64)) * time.Millisecond
					
					// Start processing and signal we've started
					startMsg := map[string]interface{}{
						"type": "started",
						"id": cmd["id"],
					}
					json.NewEncoder(os.Stdout).Encode(startMsg)
					
					json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
						"debug": "entering sleep select",
						"id": cmd["id"],
						"duration": sleepTime.String(),
					})
					
					// Sleep with cancellation support
					select {
					case <-ctx.Done():
						// Task was interrupted
						json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
							"debug": "task context cancelled during sleep",
							"id": cmd["id"],
						})
						
						response := map[string]interface{}{
							"type":    "interrupted", 
							"id":      cmd["id"],
							"message": "task was interrupted",
						}
						json.NewEncoder(os.Stdout).Encode(response)
						taskMutex.Lock()
						currentTask = nil
						currentCancel = nil
						taskMutex.Unlock()
						return
					case <-time.After(sleepTime):
						// Sleep completed normally
						json.NewEncoder(os.Stderr).Encode(map[string]interface{}{
							"debug": "sleep timer completed",
							"id": cmd["id"],
						})
					}
				} else {
					// Regular quick command - simulate processing time with cancellation
					select {
					case <-ctx.Done():
						// Task was interrupted
						response := map[string]interface{}{
							"type":    "interrupted",
							"id":      cmd["id"], 
							"message": "task was interrupted",
						}
						json.NewEncoder(os.Stdout).Encode(response)
						taskMutex.Lock()
						currentTask = nil
						currentCancel = nil
						taskMutex.Unlock()
						return
					case <-time.After(100 * time.Millisecond):
						// Processing completed normally
					}
				}
	
				// Send success response only if not cancelled
				select {
				case <-ctx.Done():
					// Was cancelled during final stage
					response := map[string]interface{}{
						"type":    "interrupted",
						"id":      cmd["id"], 
						"message": "task was interrupted",
					}
					json.NewEncoder(os.Stdout).Encode(response)
				default:
					// Send success response
					response := map[string]interface{}{
						"type":    "success",
						"id":      cmd["id"],
						"message": "ok",
						"data":    cmd["data"],
					}
					json.NewEncoder(os.Stdout).Encode(response)
				}
				
				taskMutex.Lock()
				currentTask = nil
				currentCancel = nil
				taskMutex.Unlock()
			}(cmd)
		}
	}
	`
	
	workerFilePath = filepath.Join(tmpDir, "interrupt_worker_proper.go")
	err = os.WriteFile(workerFilePath, []byte(properWorkerProgram), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Compile the test worker program.
	workerBinaryPath := filepath.Join(tmpDir, "interrupt_worker")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile worker program: %v\nOutput: %s", err, string(out))
	}

	// Create a logger.
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Create the ProcessPool with 1 worker for predictable behavior
	pool := NewProcessPool(
		"interrupt-test",
		1,
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		10*time.Second, // Worker timeout
		30*time.Second, // Communication timeout  
		5*time.Second,  // Initialization timeout
	)
	defer pool.StopAll()

	// Test 1: Interrupt a long-running command
	t.Run("InterruptExecutingCommand", func(t *testing.T) {
		// Create a command that will run for 2 seconds
		cmdID := "test-interrupt-cmd-123"
		cmd := map[string]interface{}{
			"id": cmdID,
			"sleep": 2000.0, // 2 seconds
			"data": "long running task",
		}

		// Start the command in a goroutine and measure timing
		var response map[string]interface{}
		var cmdErr error
		done := make(chan bool, 1)
		startTime := time.Now()

		go func() {
			response, cmdErr = pool.SendCommand(cmd)
			done <- true
		}()

		// Wait for the command to start, then interrupt it
		time.Sleep(200 * time.Millisecond)
		
		// Try to interrupt the command
		err := pool.Interrupt(cmdID)
		if err != nil {
			t.Errorf("Interrupt failed: %v", err)
		}

		// Wait for command completion
		select {
		case <-done:
			// Command completed - check timing and response
		case <-time.After(5 * time.Second):
			t.Fatal("Command did not complete within timeout")
		}
		
		duration := time.Since(startTime)

		// Verify command was interrupted (should complete in < 1 second, not the full 2 seconds)
		if duration > 1*time.Second {
			t.Errorf("Command took too long (%v), should have been interrupted quickly", duration)
		}

		// Verify we got an error or interrupted response
		if cmdErr != nil {
			t.Logf("Command returned error: %v", cmdErr)
		}
		
		if response == nil {
			t.Error("Expected a response from interrupted command")
		} else {
			responseType := response["type"]
			if responseType != "interrupted" {
				t.Errorf("Expected response type 'interrupted', got '%v'. Full response: %v", responseType, response)
			} else {
				t.Logf("✓ Command properly interrupted: %v", response)
			}
		}
	})

	// Test 2: Interrupt a queued command (when no worker available)
	t.Run("InterruptQueuedCommand", func(t *testing.T) {
		// First, start a long-running command to occupy the single worker
		blockingCmd := map[string]interface{}{
			"id": "blocking-cmd",
			"sleep": 1000.0, // 1 second
		}

		go pool.SendCommand(blockingCmd)
		time.Sleep(100 * time.Millisecond) // Let it start

		// Now queue a second command that should be queued
		queuedCmdID := "queued-cmd-456"  
		queuedCmd := map[string]interface{}{
			"id": queuedCmdID,
			"data": "queued task",
		}

		var queuedErr error
		queuedDone := make(chan bool, 1)

		go func() {
			_, queuedErr = pool.SendCommand(queuedCmd)
			queuedDone <- true
		}()

		// Give it time to be queued, then interrupt it
		time.Sleep(100 * time.Millisecond)
		
		err := pool.Interrupt(queuedCmdID)
		if err != nil {
			t.Errorf("Interrupt of queued command failed: %v", err)
		}

		// Wait for the queued command to complete
		select {
		case <-queuedDone:
			if queuedErr == nil {
				t.Error("Expected queued command to return error after interrupt")
			} else if queuedErr.Error() != "command queued-cmd-456 was interrupted while queued" {
				t.Errorf("Expected specific interrupt error, got: %v", queuedErr)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Queued command did not complete within timeout")
		}
	})
}
