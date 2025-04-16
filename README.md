# Subprocess Pool

A Go library for managing a pool of subprocesses, with features such as process creation, communication, restart, and workload distribution.

## Features

- Manage a pool of long-running subprocesses
- JSON-based communication with subprocesses
- Automatic process restart on failure
- Fair workload distribution among processes
- Handling of process stdout/stderr
- Graceful process and pool shutdown
- Timeout handling for worker availability and communication

## Test Coverage

The codebase includes comprehensive tests that verify all essential behaviors:

1. **Basic functionality**
   - Process creation and command sending/receiving
   - Process startup and readiness detection
   - Process export and status reporting

2. **Concurrent operation**
   - Multiple concurrent requests handling
   - Load balancing across multiple processes
   - Race condition handling

3. **Robustness**
   - Handling of slow-running processes
   - Error responses from subprocesses
   - Process crashes and automatic restarts
   - Process stderr output handling
   - Command timeouts

4. **Process Pool Management**
   - Process queue behavior and fair distribution
   - Worker acquisition timeouts
   - Pool stopping and resource cleanup

5. **Queue Behavior**
   - Queue under high load conditions
   - Queue priority ordering (load balancing)
   - Worker timeouts when all workers are busy

## Usage

```go
import "github.com/coffyg/subp"

// Create a process pool
pool := subp.NewProcessPool(
    "worker-pool",      // Name prefix for processes
    5,                  // Number of processes in the pool
    &logger,            // zerolog.Logger
    "/path/to/workdir", // Working directory
    "/path/to/binary",  // Path to subprocess binary
    []string{},         // Optional arguments to subprocess
    5*time.Second,      // Worker timeout
    10*time.Second,     // Communication timeout
    5*time.Second,      // Initialization timeout
)
defer pool.StopAll()

// Wait for the pool to be ready
err := pool.WaitForReady()
if err != nil {
    return err
}

// Send a command to a worker
cmd := map[string]interface{}{
    "action": "process",
    "data": "some data",
}
response, err := pool.SendCommand(cmd)
if err != nil {
    return err
}

// Process the response
fmt.Println(response["result"])
```

## Running Tests

To run the full test suite:

```bash
./test.sh
```

To run only the essential behavior tests:

```bash
./run_tests.sh
```