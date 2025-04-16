#!/bin/bash
set -e

# Check if we're actually running benchmarks
RUN_BENCHMARKS=false
if [ "$1" == "--run" ]; then
    RUN_BENCHMARKS=true
fi

echo "=== Process Pool Benchmark Suite ==="
echo "Benchmarking v0.0.4 version"

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
YELLOW='\033[0;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Get current timestamp for the benchmark run
timestamp=$(date +"%Y%m%d%H%M%S")
result_dir="bench_results"
mkdir -p "$result_dir"
result_file="$result_dir/bench_${timestamp}.txt"

echo -e "${GREEN}=== Benchmark Instructions ===${NC}"
echo -e "To run benchmarks for the process pool, use the following commands:"
echo -e ""
echo -e "${CYAN}1. Basic benchmarks:${NC}"
echo -e "   ${YELLOW}go test -bench=. -benchmem${NC}"
echo -e ""
echo -e "${CYAN}2. Focused benchmarks for specific functions:${NC}"
echo -e "   ${YELLOW}go test -bench=BenchmarkGetWorker -benchmem${NC}"
echo -e "   ${YELLOW}go test -bench=BenchmarkSendCommand -benchmem${NC}"
echo -e ""
echo -e "${CYAN}3. Run benchmarks via this script:${NC}"
echo -e "   ${YELLOW}./bench.sh --run${NC}"
echo -e ""

# If --run flag is provided, create benchmark file and run benchmarks
if [ "$RUN_BENCHMARKS" = true ]; then
    echo -e "${GREEN}=== Running Benchmarks ===${NC}"
    echo -e "${YELLOW}Creating benchmark file...${NC}"
    
    # Remove any existing benchmark files
    rm -f process_pool_bench_template.go process_pool_bench_test.go
    
    # Create a simplified benchmark file that just tests GetWorker
    cat > process_pool_bench_test.go << 'EOF'
package subp

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// setupBenchProcessPool creates a process pool for benchmarking
func setupBenchProcessPool(b *testing.B, numWorkers int) (*ProcessPool, string, func()) {
	// Create temp dir for testing
	tmpDir, err := os.MkdirTemp("", "processpool_bench")
	if err != nil {
		b.Fatal(err)
	}

	// Create a logger that writes to io.Discard
	logger := zerolog.New(os.Stdout).Output(io.Discard)

	// Create a simple worker program - responds immediately for benchmarking
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
	
			// Send response immediately (no delay for benchmarking)
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
	workerPath := filepath.Join(tmpDir, "bench_worker.go")
	if err := os.WriteFile(workerPath, []byte(workerProgram), 0644); err != nil {
		b.Fatal(err)
	}

	binaryPath := filepath.Join(tmpDir, "bench_worker")
	cmd := exec.Command("go", "build", "-o", binaryPath, workerPath)
	if err := cmd.Run(); err != nil {
		b.Fatalf("Failed to compile worker: %v", err)
	}

	// Create process pool
	pool := NewProcessPool(
		"bench-pool",
		numWorkers,
		&logger,
		tmpDir,
		binaryPath,
		[]string{},
		2*time.Second, // Worker timeout
		2*time.Second, // Communication timeout
		2*time.Second, // Initialization timeout
	)

	// Wait for processes to be ready
	err = pool.WaitForReady()
	if err != nil {
		b.Fatalf("Failed to initialize pool: %v", err)
	}

	// Return cleanup function
	cleanup := func() {
		pool.StopAll()
		os.RemoveAll(tmpDir)
	}

	return pool, tmpDir, cleanup
}

// BenchmarkGetWorker measures the performance of getting a worker from the pool
func BenchmarkGetWorker(b *testing.B) {
	pool, _, cleanup := setupBenchProcessPool(b, 5)
	defer cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		worker, err := pool.GetWorker()
		if err != nil {
			b.Fatalf("Failed to get worker: %v", err)
		}
		worker.SetBusy(0) // Release the worker immediately
	}
}

// BenchmarkSendCommand measures the performance of sending a command to a worker
func BenchmarkSendCommand(b *testing.B) {
	pool, _, cleanup := setupBenchProcessPool(b, 5)
	defer cleanup()

	// Get a worker
	worker, err := pool.GetWorker()
	if err != nil {
		b.Fatalf("Failed to get worker: %v", err)
	}
	defer worker.SetBusy(0) // Make sure we release the worker

	// Prepare command
	cmd := map[string]interface{}{
		"data": "benchmark test",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := worker.SendCommand(cmd)
		if err != nil {
			b.Fatalf("Command failed: %v", err)
		}
	}
	b.StopTimer()
}
EOF
    
    echo -e "${GREEN}Benchmark file created.${NC}"
    
    # Run each benchmark and record results
    echo "Running benchmarks and saving results to $result_file"
    echo "Timestamp: $(date)" | tee -a "$result_file"
    echo "----------------------------------------" | tee -a "$result_file"
    
    # Run BenchmarkGetWorker with a very short benchtime
    echo -e "\n${CYAN}Running BenchmarkGetWorker${NC}"
    go test -bench=BenchmarkGetWorker -benchmem -benchtime=0.5s | tee -a "$result_file"
    
    # Run BenchmarkSendCommand with a very short benchtime
    echo -e "\n${CYAN}Running BenchmarkSendCommand${NC}"
    go test -bench=BenchmarkSendCommand -benchmem -benchtime=0.5s | tee -a "$result_file"
    
    echo -e "\n${GREEN}Benchmark results saved to ${result_file}${NC}"
    echo -e "Use these results as a baseline for comparing optimizations."
    
    # Clean up
    echo -e "\n${YELLOW}Cleaning up benchmark files...${NC}"
    rm -f process_pool_bench_test.go
else
    echo -e "${YELLOW}To run benchmarks, use:${NC} ./bench.sh --run"
fi

echo -e "\n${CYAN}=== Potential Optimization Areas ===${NC}"
echo -e "Based on benchmark analysis, consider optimizing:"
echo -e "1. Worker acquisition time (BenchmarkGetWorker)"
echo -e "2. Command serialization/deserialization (BenchmarkSendCommand)"
echo -e "3. Queue management for better parallelism"
echo -e "4. Process startup time"
echo -e "5. Fixing race conditions in the ProcessPQ operations"
echo -e ""

echo -e "\n${GREEN}=== Benchmarking completed! ===${NC}"
echo "The subprocess pool implementation (v0.0.4) is ready for optimization."