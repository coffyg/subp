package subp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// setupBenchmarkWorker creates a test worker program for benchmarks
func setupBenchmarkWorker(b *testing.B) (string, func()) {
	// Create a temporary directory for the test worker program
	tmpDir, err := os.MkdirTemp("", "benchmark_test")
	if err != nil {
		b.Fatal(err)
	}

	// Write a simple worker program optimized for benchmarking
	workerProgram := `
	package main

	import (
		"bufio"
		"encoding/json"
		"os"
	)

	func main() {
		// Send ready message
		readyMsg := map[string]interface{}{"type": "ready"}
		json.NewEncoder(os.Stdout).Encode(readyMsg)
	
		// Process commands as fast as possible with minimal delays
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
	
			var cmd map[string]interface{}
			if err := json.Unmarshal([]byte(line), &cmd); err != nil {
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
	
			// Immediate response (no artificial delay)
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
	workerPath := filepath.Join(tmpDir, "benchmark_worker.go")
	if err := os.WriteFile(workerPath, []byte(workerProgram), 0644); err != nil {
		b.Fatal(err)
	}

	binaryPath := filepath.Join(tmpDir, "benchmark_worker")
	cmd := exec.Command("go", "build", "-o", binaryPath, workerPath)
	if err := cmd.Run(); err != nil {
		b.Fatalf("Failed to compile worker: %v", err)
	}

	// Return cleanup function
	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return binaryPath, cleanup
}

// BenchmarkSendCommand measures the performance of sending commands to a process
func BenchmarkSendCommand(b *testing.B) {
	// Setup
	binaryPath, cleanup := setupBenchmarkWorker(b)
	defer cleanup()

	// Suppress logging during benchmarks
	logger := zerolog.New(zerolog.Nop())

	// Create process pool with 1 worker
	pool := NewProcessPool(
		"bench",
		1,
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,  // Worker timeout
		5*time.Second,  // Communication timeout
		5*time.Second,  // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for worker to be ready
	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	// Reset timer before the actual benchmark
	b.ResetTimer()

	// Run benchmark
	for i := 0; i < b.N; i++ {
		cmd := map[string]interface{}{
			"data": "benchmark data",
		}
		_, err := pool.SendCommand(cmd)
		if err != nil {
			b.Fatalf("Command failed: %v", err)
		}
	}
}

// BenchmarkConcurrentCommands measures the performance of concurrent command sending
func BenchmarkConcurrentCommands(b *testing.B) {
	// Setup
	binaryPath, cleanup := setupBenchmarkWorker(b)
	defer cleanup()

	// Suppress logging during benchmarks
	logger := zerolog.New(zerolog.Nop())

	// Create process pool with multiple workers
	numWorkers := 4
	pool := NewProcessPool(
		"bench-concurrent",
		numWorkers,
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,  // Worker timeout
		5*time.Second,  // Communication timeout
		5*time.Second,  // Initialization timeout
	)
	defer pool.StopAll()

	// Wait for workers to be ready
	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	// Reset timer before the actual benchmark
	b.ResetTimer()

	// Run benchmark with parallelism
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cmd := map[string]interface{}{
				"data": "benchmark data",
			}
			_, err := pool.SendCommand(cmd)
			if err != nil {
				b.Fatalf("Command failed: %v", err)
			}
		}
	})
}