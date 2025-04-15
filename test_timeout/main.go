package main

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

func main() {
	// Set up logging
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
	
	// Create a simple echo worker
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
			fmt.Fprintf(os.Stderr, "Error parsing command: %v\n", err)
			continue
		}
		
		// Sleep to simulate work
		time.Sleep(100 * time.Millisecond)
		
		// Echo back the command
		cmd["type"] = "response"
		json.NewEncoder(os.Stdout).Encode(cmd)
	}
}
`

	// Write worker script
	err := os.WriteFile("worker.go", []byte(workerScript), 0644)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to write worker script")
	}

	// Compile worker
	logger.Info().Msg("Compiling worker...")
	err = exec.Command("go", "build", "-o", "worker", "worker.go").Run()
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to compile worker")
	}

	fmt.Println("Analysis of timeouts in subp.ProcessPool:")
	fmt.Println("=========================================")
	
	fmt.Println("\n1. In the code, I found three timeouts in the ProcessPool:")
	fmt.Println("   - workerTimeout: How long to wait for a worker to become available")
	fmt.Println("   - comTimeout: How long to wait for a command response") 
	fmt.Println("   - initTimeout: How long to wait for a worker to initialize")
	
	fmt.Println("\n2. The default timeouts used in the test files are:")
	fmt.Println("   - Basic tests: 2-3 seconds for all timeouts")
	fmt.Println("   - High-load tests: 5-15 seconds for worker timeout")
	
	fmt.Println("\n3. Implementation details:")
	fmt.Println("   - The 'timeout exceeded, no available workers' error comes from waitForWorker()")
	fmt.Println("   - This function has been updated to use exponential backoff from 100μs to 10ms")
	fmt.Println("   - This means it may try many times before giving up")
	
	fmt.Println("\n4. Possible solutions in your SSR app:")
	fmt.Println("   a) Check if timeouts are explicitly set and increase them")
	fmt.Println("   b) Increase the number of workers in the pool")
	fmt.Println("   c) Implement a queue in your SSR code to limit concurrent requests")
}