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

// setupPerfWorker creates a test worker program for performance benchmarks
func setupPerfWorker(b *testing.B) (string, func()) {
	tmpDir, err := os.MkdirTemp("", "perf_benchmark_test")
	if err != nil {
		b.Fatal(err)
	}

	// Worker with minimal overhead for accurate measurements
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

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var cmd map[string]interface{}
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			continue
		}

		// Simulate different workloads based on command type
		if workType, ok := cmd["work_type"].(string); ok {
			switch workType {
			case "cpu_light":
				// Minimal CPU work
				sum := 0
				for i := 0; i < 100; i++ {
					sum += i
				}
			case "cpu_heavy":
				// Heavy CPU work
				sum := 0
				for i := 0; i < 1000000; i++ {
					sum += i
				}
			case "io_simulate":
				// Simulate I/O delay
				time.Sleep(10 * time.Millisecond)
			}
		}

		response := map[string]interface{}{
			"type": "success",
			"id":   cmd["id"],
			"data": cmd["data"],
		}
		json.NewEncoder(os.Stdout).Encode(response)
	}
}
`

	workerPath := filepath.Join(tmpDir, "perf_worker.go")
	if err := os.WriteFile(workerPath, []byte(workerProgram), 0644); err != nil {
		b.Fatal(err)
	}

	binaryPath := filepath.Join(tmpDir, "perf_worker")
	cmd := exec.Command("go", "build", "-o", binaryPath, workerPath)
	if err := cmd.Run(); err != nil {
		b.Fatalf("Failed to compile worker: %v", err)
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return binaryPath, cleanup
}

// BenchmarkWorkerAcquisitionLatency measures time to acquire an available worker
func BenchmarkWorkerAcquisitionLatency(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())
	pool := NewProcessPool(
		"bench-acquisition",
		4, // Multiple workers to test queue efficiency
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Measure just the worker acquisition time
		worker, err := pool.GetWorker()
		if err != nil {
			b.Fatal("Failed to get worker:", err)
		}
		worker.SetBusy(0)
	}
}

// BenchmarkWorkerAcquisitionUnderLoad measures acquisition latency when pool is busy
func BenchmarkWorkerAcquisitionUnderLoad(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())
	numWorkers := 4
	pool := NewProcessPool(
		"bench-load",
		numWorkers,
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	// Keep all workers busy except one
	busyWorkers := make([]*Process, numWorkers-1)
	for i := 0; i < numWorkers-1; i++ {
		worker, err := pool.GetWorker()
		if err != nil {
			b.Fatal("Failed to get worker for load generation:", err)
		}
		busyWorkers[i] = worker
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		start := time.Now()
		worker, err := pool.GetWorker()
		acquisitionTime := time.Since(start)
		
		if err != nil {
			b.Fatal("Failed to get worker:", err)
		}
		
		// Track acquisition latency
		if acquisitionTime > 150*time.Millisecond {
			b.Logf("High acquisition latency: %v", acquisitionTime)
		}
		
		worker.SetBusy(0)
	}

	b.StopTimer()
	
	// Release busy workers
	for _, w := range busyWorkers {
		w.SetBusy(0)
	}
}

// BenchmarkQueueOperations measures channel operations performance
func BenchmarkQueueOperations(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())
	pool := NewProcessPool(
		"bench-queue",
		16, // More workers to stress operations
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	// Measure channel send/receive performance
	for i := 0; i < b.N; i++ {
		worker, err := pool.GetWorker()
		if err != nil {
			b.Fatal(err)
		}
		worker.SetBusy(0) // Release immediately
	}
}

// BenchmarkLockContention measures impact of lock contention
func BenchmarkLockContention(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())
	pool := NewProcessPool(
		"bench-locks",
		8,
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	// Run parallel operations to stress locks
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Simulate mixed read/write operations
			if time.Now().UnixNano()%2 == 0 {
				// Read operation
				pool.mutex.RLock()
				_ = len(pool.processes)
				pool.mutex.RUnlock()
			} else {
				// Write operation (worker acquisition)
				worker, err := pool.GetWorker()
				if err == nil && worker != nil {
					worker.SetBusy(0)
				}
			}
		}
	})
}

// BenchmarkJSONOverhead measures JSON encoding/decoding performance
func BenchmarkJSONOverhead(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())
	pool := NewProcessPool(
		"bench-json",
		1,
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	// Test with different payload sizes
	payloads := []struct {
		name string
		data map[string]interface{}
	}{
		{"small", map[string]interface{}{"data": "test"}},
		{"medium", map[string]interface{}{"data": string(make([]byte, 1024))}},
		{"large", map[string]interface{}{"data": string(make([]byte, 10240))}},
	}

	for _, payload := range payloads {
		b.Run(payload.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := pool.SendCommand(payload.data)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkHighThroughput measures maximum sustainable throughput
func BenchmarkHighThroughput(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())
	pool := NewProcessPool(
		"bench-throughput",
		8,
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	// Measure requests per second
	start := time.Now()
	completed := int64(0)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cmd := map[string]interface{}{
				"work_type": "cpu_light",
				"data":      "throughput test",
			}
			_, err := pool.SendCommand(cmd)
			if err != nil {
				b.Fatal(err)
			}
			atomic.AddInt64(&completed, 1)
		}
	})

	duration := time.Since(start)
	rps := float64(completed) / duration.Seconds()
	b.ReportMetric(rps, "reqs/s")
}

// BenchmarkLatencyPercentiles measures latency distribution
func BenchmarkLatencyPercentiles(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())
	pool := NewProcessPool(
		"bench-latency",
		4,
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	latencies := make([]time.Duration, 0, b.N)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		start := time.Now()
		_, err := pool.SendCommand(map[string]interface{}{"data": "latency test"})
		if err != nil {
			b.Fatal(err)
		}
		latencies = append(latencies, time.Since(start))
	}

	// Calculate percentiles (simplified - in real benchmarks use proper statistics)
	if len(latencies) > 0 {
		avg := time.Duration(0)
		for _, l := range latencies {
			avg += l
		}
		avg /= time.Duration(len(latencies))
		b.ReportMetric(float64(avg.Microseconds()), "avg_μs")
	}
}

// BenchmarkWorkerLifecycle measures worker creation/destruction overhead
func BenchmarkWorkerLifecycle(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		pool := NewProcessPool(
			fmt.Sprintf("bench-lifecycle-%d", i),
			1,
			&logger,
			filepath.Dir(binaryPath),
			binaryPath,
			[]string{},
			5*time.Second,
			5*time.Second,
			5*time.Second,
		)

		if err := pool.WaitForReady(); err != nil {
			b.Fatal(err)
		}

		pool.StopAll()
	}
}

// BenchmarkMemoryAllocation tracks memory allocations per operation
func BenchmarkMemoryAllocation(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	logger := zerolog.New(zerolog.Nop())
	pool := NewProcessPool(
		"bench-memory",
		2,
		&logger,
		filepath.Dir(binaryPath),
		binaryPath,
		[]string{},
		5*time.Second,
		5*time.Second,
		5*time.Second,
	)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cmd := map[string]interface{}{"data": "memory test"}
		_, err := pool.SendCommand(cmd)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScalability tests performance with varying worker counts
func BenchmarkScalability(b *testing.B) {
	binaryPath, cleanup := setupPerfWorker(b)
	defer cleanup()

	workerCounts := []int{1, 2, 4, 8, 16}
	
	for _, count := range workerCounts {
		b.Run(fmt.Sprintf("workers-%d", count), func(b *testing.B) {
			logger := zerolog.New(zerolog.Nop())
			pool := NewProcessPool(
				fmt.Sprintf("bench-scale-%d", count),
				count,
				&logger,
				filepath.Dir(binaryPath),
				binaryPath,
				[]string{},
				5*time.Second,
				5*time.Second,
				5*time.Second,
			)
			defer pool.StopAll()

			if err := pool.WaitForReady(); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()

			// Run with concurrency matching worker count
			var wg sync.WaitGroup
			errors := make(chan error, count)
			
			for w := 0; w < count; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < b.N/count; i++ {
						_, err := pool.SendCommand(map[string]interface{}{"data": "scale test"})
						if err != nil {
							errors <- err
							return
						}
					}
				}()
			}
			
			wg.Wait()
			close(errors)
			
			for err := range errors {
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}