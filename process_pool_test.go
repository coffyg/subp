// process_pool_test.go
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
	pool := NewProcessPool(
		"parallelTest",
		4,
		&logger,
		tmpDir,
		workerBinaryPath,
		[]string{},
		2*time.Second,
		2*time.Second,
		2*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	numCommands := 100
	results := make([]int, numCommands)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(numCommands)

	for i := 0; i < numCommands; i++ {
		go func(idx int) {
			defer wg.Done()
			cmdData := map[string]interface{}{
				"data": fmt.Sprintf("parallel test %d", idx),
			}
			resp, err := pool.SendCommand(cmdData)
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
