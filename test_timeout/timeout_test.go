package main

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/fy/coffyg/subp"
)

func main() {
	// Set up logging
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

	// Create temp worker script
	workerScript := `
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	// Signal that we're ready
	json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"type": "ready"})
	
	// Process commands
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var cmd map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
			continue
		}
		
		// Simulate a FAST 50ms SSR render
		time.Sleep(50 * time.Millisecond)
		
		// Echo back the command
		cmd["type"] = "response"
		json.NewEncoder(os.Stdout).Encode(cmd)
	}
}
`
	// Write worker script
	os.WriteFile("test_worker.go", []byte(workerScript), 0644)
	exec.Command("go", "build", "-o", "test_worker", "test_worker.go").Run()

	// Create a small pool (typical for SSR)
	workers := 4
	pool := subp.NewProcessPool(
		"test_ssr",
		workers,
		&logger,
		".",
		"./test_worker",
		[]string{},
		500*time.Millisecond,  // Very short worker timeout to reproduce issue
		1*time.Second,         // Communication timeout
		5*time.Second,         // Init timeout
	)

	if err := pool.WaitForReady(); err != nil {
		fmt.Printf("Pool failed to initialize: %v\n", err)
		return
	}

	// Simulate a "burst" of concurrent SSR requests (more than workers)
	concurrentRequests := 20
	successCount := 0
	timeoutCount := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	
	fmt.Printf("Sending %d concurrent requests to %d workers with 50ms processing time...\n", 
		concurrentRequests, workers)
	
	start := time.Now()
	
	// Launch all requests at once
	for i := 0; i < concurrentRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			
			cmd := map[string]interface{}{
				"req": fmt.Sprintf("SSR request %d", idx),
			}
			
			_, err := pool.SendCommand(cmd)
			mu.Lock()
			if err != nil {
				timeoutCount++
				fmt.Printf("Request %d failed: %v\n", idx, err)
			} else {
				successCount++
			}
			mu.Unlock()
		}(i)
	}
	
	wg.Wait()
	elapsed := time.Since(start)
	
	fmt.Printf("\nResults:\n")
	fmt.Printf("- Total requests: %d\n", concurrentRequests)
	fmt.Printf("- Successful: %d (%.1f%%)\n", successCount, float64(successCount)*100/float64(concurrentRequests))
	fmt.Printf("- Timeouts: %d (%.1f%%)\n", timeoutCount, float64(timeoutCount)*100/float64(concurrentRequests))
	fmt.Printf("- Total time: %s\n", elapsed)
	fmt.Printf("- Theoretical minimum time with %d workers: %s\n", 
		workers, time.Duration(concurrentRequests/workers*50)*time.Millisecond)
	
	// Export all workers
	exports := pool.ExportAll()
	fmt.Printf("\nWorker states:\n")
	for i, exp := range exports {
		fmt.Printf("Worker %d: Ready=%v, Requests=%d, Restarts=%d, Latency=%dms\n", 
			i, exp.IsReady, exp.RequestsHandled, exp.Restarts, exp.Latency)
	}
	
	pool.StopAll()
}