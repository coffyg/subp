// process_pool_test.go
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

func TestProcessPool(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "processpool_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	workerProgram := `
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

func main() {
	json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"type": "ready"})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var cmd map[string]interface{}
		err := json.Unmarshal([]byte(line), &cmd)
		if err != nil {
			errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
			if id, ok := cmd["id"]; ok {
				errMsg["id"] = id
			}
			json.NewEncoder(os.Stdout).Encode(errMsg)
			continue
		}
		time.Sleep(100 * time.Millisecond)
		resp := map[string]interface{}{
			"type":    "success",
			"id":      cmd["id"],
			"message": "ok",
			"data":    cmd["data"],
		}
		json.NewEncoder(os.Stdout).Encode(resp)
	}
}
`
	workerFilePath := filepath.Join(tmpDir, "test_worker.go")
	if err := os.WriteFile(workerFilePath, []byte(workerProgram), 0644); err != nil {
		t.Fatal(err)
	}

	workerBinaryPath := filepath.Join(tmpDir, "test_worker")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile worker program: %v\nOutput: %s", err, string(out))
	}

	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
	pool := NewProcessPool(
		"test",
		2,
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second,
		2*time.Second,
		2*time.Second,
	)

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		cmdData := map[string]interface{}{
			"data": fmt.Sprintf("test data %d", i),
		}
		resp, err := pool.SendCommand(cmdData)
		if err != nil {
			t.Fatal(err)
		}
		if resp["type"] != "success" {
			t.Errorf("Expected 'success', got '%v'", resp["type"])
		}
		if resp["message"] != "ok" {
			t.Errorf("Expected 'ok', got '%v'", resp["message"])
		}
		if resp["data"] != cmdData["data"] {
			t.Errorf("Expected '%v', got '%v'", cmdData["data"], resp["data"])
		}
	}

	pool.StopAll()
}

func TestProcessPoolSpeed(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "processpool_speed")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	workerProgram := `
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

func main() {
	json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"type": "ready"})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var cmd map[string]interface{}
		err := json.Unmarshal([]byte(line), &cmd)
		if err != nil {
			errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
			if id, ok := cmd["id"]; ok {
				errMsg["id"] = id
			}
			json.NewEncoder(os.Stdout).Encode(errMsg)
			continue
		}
		time.Sleep(50 * time.Millisecond)
		resp := map[string]interface{}{
			"type":    "success",
			"id":      cmd["id"],
			"message": "ok",
			"data":    cmd["data"],
		}
		json.NewEncoder(os.Stdout).Encode(resp)
	}
}
`
	workerFilePath := filepath.Join(tmpDir, "test_worker_speed.go")
	if err := os.WriteFile(workerFilePath, []byte(workerProgram), 0644); err != nil {
		t.Fatal(err)
	}

	workerBinaryPath := filepath.Join(tmpDir, "test_worker_speed")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile speed worker program: %v\nOutput: %s", err, string(out))
	}

	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
	pool := NewProcessPool(
		"speedTest",
		3,
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		3*time.Second,
		3*time.Second,
		3*time.Second,
	)

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	numCommands := 50
	var wg sync.WaitGroup
	wg.Add(numCommands)

	for i := 0; i < numCommands; i++ {
		go func(idx int) {
			defer wg.Done()
			cmdData := map[string]interface{}{
				"data": fmt.Sprintf("speed test %d", idx),
			}
			_, err := pool.SendCommand(cmdData)
			if err != nil {
				t.Errorf("Command %d failed: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("Processed %d commands in %v with 3 processes", numCommands, elapsed)
	pool.StopAll()
}

func TestProcessPoolParallel(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "processpool_parallel")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	workerProgram := `
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

func main() {
	json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"type": "ready"})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var cmd map[string]interface{}
		err := json.Unmarshal([]byte(line), &cmd)
		if err != nil {
			errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
			if id, ok := cmd["id"]; ok {
				errMsg["id"] = id
			}
			json.NewEncoder(os.Stdout).Encode(errMsg)
			continue
		}
		time.Sleep(20 * time.Millisecond)
		resp := map[string]interface{}{
			"type":    "success",
			"id":      cmd["id"],
			"message": "ok",
			"data":    cmd["data"],
		}
		json.NewEncoder(os.Stdout).Encode(resp)
	}
}
`
	workerFilePath := filepath.Join(tmpDir, "test_worker_parallel.go")
	if err := os.WriteFile(workerFilePath, []byte(workerProgram), 0644); err != nil {
		t.Fatal(err)
	}

	workerBinaryPath := filepath.Join(tmpDir, "test_worker_parallel")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile parallel worker program: %v\nOutput: %s", err, string(out))
	}

	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
	
	// Use more workers and longer timeouts for stability
	pool := NewProcessPool(
		"parallelTest",
		4, // Few workers to test proper queueing behavior
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		10*time.Second,  // Long timeout to allow for queue processing
		10*time.Second,  // Long timeout
		5*time.Second,   // Initialization timeout
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Use 100 commands to test robustness
	numCommands := 100
	results := make([]int, numCommands)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(numCommands)

	// Don't limit concurrency - test full load
	concurrent := make(chan struct{}, numCommands)
	
	for i := 0; i < numCommands; i++ {
		go func(idx int) {
			defer wg.Done()
			
			// Track concurrent executions (for monitoring only)
			concurrent <- struct{}{}
			inFlight := len(concurrent)
			defer func() { <-concurrent }()
			
			cmdData := map[string]interface{}{
				"data": fmt.Sprintf("parallel test %d", idx),
			}
			resp, err := pool.SendCommand(cmdData)
			_ = inFlight // Use the variable to avoid compiler warnings
			if err != nil {
				t.Errorf("Error in parallel command %d: %v", idx, err)
				return
			}
			if resp["type"] != "success" {
				t.Errorf("Expected 'success', got %v", resp["type"])
				return
			}
			mu.Lock()
			results[idx] = idx
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	for i := 0; i < numCommands; i++ {
		if results[i] != i {
			t.Errorf("Result mismatch at index %d: got %d, expected %d", i, results[i], i)
		}
	}
}

func BenchmarkProcessPool(b *testing.B) {
	tmpDir, err := os.MkdirTemp("", "processpool_bench")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	workerProgram := `
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

func main() {
	json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"type": "ready"})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var cmd map[string]interface{}
		err := json.Unmarshal([]byte(line), &cmd)
		if err != nil {
			errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
			if id, ok := cmd["id"]; ok {
				errMsg["id"] = id
			}
			json.NewEncoder(os.Stdout).Encode(errMsg)
			continue
		}
		// Minimal sleep to better isolate process pool performance
		time.Sleep(1 * time.Millisecond)
		resp := map[string]interface{}{
			"type":    "success",
			"id":      cmd["id"],
			"message": "ok",
			"data":    cmd["data"],
		}
		json.NewEncoder(os.Stdout).Encode(resp)
	}
}
`
	workerFilePath := filepath.Join(tmpDir, "test_worker_bench.go")
	if err := os.WriteFile(workerFilePath, []byte(workerProgram), 0644); err != nil {
		b.Fatal(err)
	}

	workerBinaryPath := filepath.Join(tmpDir, "test_worker_bench")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		b.Fatalf("Failed to compile benchmark worker program: %v\nOutput: %s", err, string(out))
	}

	// Create a silent logger
	logger := zerolog.Nop()
	
	// Create pool with different worker counts to benchmark scaling
	numWorkers := 8
	pool := NewProcessPool(
		"benchTest",
		numWorkers,
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		10*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	// Reset timer to exclude setup overhead
	b.ResetTimer()
	
	// Run parallel benchmark
	b.RunParallel(func(pb *testing.PB) {
		counter := 0
		for pb.Next() {
			counter++
			cmdData := map[string]interface{}{
				"data": fmt.Sprintf("benchmark test %d", counter),
			}
			_, err := pool.SendCommand(cmdData)
			if err != nil {
				b.Fatalf("Error in benchmark command: %v", err)
			}
		}
	})
}

func TestProcessPoolHighLoad(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "processpool_highload")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	workerProgram := `
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

func main() {
	json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"type": "ready"})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var cmd map[string]interface{}
		err := json.Unmarshal([]byte(line), &cmd)
		if err != nil {
			errMsg := map[string]interface{}{"type": "error", "message": "invalid JSON"}
			if id, ok := cmd["id"]; ok {
				errMsg["id"] = id
			}
			json.NewEncoder(os.Stdout).Encode(errMsg)
			continue
		}
		// Use very short sleep to simulate fast processing
		time.Sleep(10 * time.Millisecond)
		resp := map[string]interface{}{
			"type":    "success",
			"id":      cmd["id"],
			"message": "ok",
			"data":    cmd["data"],
		}
		json.NewEncoder(os.Stdout).Encode(resp)
	}
}
`
	workerFilePath := filepath.Join(tmpDir, "test_worker_highload.go")
	if err := os.WriteFile(workerFilePath, []byte(workerProgram), 0644); err != nil {
		t.Fatal(err)
	}

	workerBinaryPath := filepath.Join(tmpDir, "test_worker_highload")
	cmd := exec.Command("go", "build", "-o", workerBinaryPath, workerFilePath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile highload worker program: %v\nOutput: %s", err, string(out))
	}

	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
	
	// Create pool with limited workers to test queueing behavior
	numWorkers := 8
	pool := NewProcessPool(
		"highloadTest",
		numWorkers,
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		15*time.Second,  // Longer timeout to handle massive queue
		5*time.Second,   // Command timeout
		5*time.Second,   // Initialization timeout
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	// Test with extremely high concurrency to stress the queue
	numCommands := 1000
	results := make([]bool, numCommands)
	var wg sync.WaitGroup
	wg.Add(numCommands)

	// Track metrics
	var successCount, errorCount int32
	start := time.Now()

	// Launch all requests simultaneously
	for i := 0; i < numCommands; i++ {
		go func(idx int) {
			defer wg.Done()
			
			cmdData := map[string]interface{}{
				"data": fmt.Sprintf("highload test %d", idx),
			}
			resp, err := pool.SendCommand(cmdData)
			if err != nil {
				atomic.AddInt32(&errorCount, 1)
				t.Logf("Error in highload command %d: %v", idx, err)
				return
			}
			if resp["type"] != "success" {
				atomic.AddInt32(&errorCount, 1)
				t.Logf("Expected success, got %v in command %d", resp["type"], idx)
				return
			}
			atomic.AddInt32(&successCount, 1)
			results[idx] = true
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)
	
	t.Logf("Highload test results:")
	t.Logf("- Total requests: %d", numCommands)
	t.Logf("- Successful: %d (%.2f%%)", successCount, float64(successCount)/float64(numCommands)*100)
	t.Logf("- Failed: %d (%.2f%%)", errorCount, float64(errorCount)/float64(numCommands)*100)
	t.Logf("- Time elapsed: %v", elapsed)
	t.Logf("- Throughput: %.2f requests/second", float64(numCommands)/elapsed.Seconds())
	t.Logf("- Workers: %d", numWorkers)
	
	successfulRequests := 0
	for i := 0; i < numCommands; i++ {
		if results[i] {
			successfulRequests++
		}
	}
	
	if successfulRequests != numCommands {
		t.Errorf("Expected all %d requests to succeed, but only %d succeeded", numCommands, successfulRequests)
	}
}
