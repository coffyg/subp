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

## Streaming commands

For subprocesses that emit multiple frames per command (e.g. TTS engines streaming audio chunks mid-inference), use `SendCommandStreaming`. It returns a receive-only channel that delivers EVERY frame — intermediate and terminal — and is closed by the library once the terminal frame arrives (or on error / idle-timeout / interrupt / process death).

```go
ch, err := pool.SendCommandStreaming(map[string]interface{}{
    "id":   "tts-job-42",
    "text": "The quick brown fox jumps over the lazy dog.",
})
if err != nil {
    return err
}
for frame := range ch {
    switch frame["type"] {
    case "chunk":
        // Intermediate payload (e.g. audio chunk).
        handleAudio(frame["data"])
    case "progress", "started", "info":
        // Optional telemetry.
    case "success", "stream_done":
        // Terminal: channel will close on the next iteration.
    case "error":
        return fmt.Errorf("worker error: %v", frame["message"])
    case "interrupted":
        return fmt.Errorf("interrupted")
    }
}
// ch is closed here.
```

### Frame type taxonomy

The library uses the `type` field to classify every frame:

| Frame type    | Class        | Regular `SendCommand`       | Streaming `SendCommandStreaming` |
|---------------|--------------|-----------------------------|----------------------------------|
| `success`     | terminal     | delivered as final response | delivered then channel closed    |
| `error`       | terminal     | delivered as final response | delivered then channel closed    |
| `interrupted` | terminal     | delivered as final response | delivered then channel closed    |
| `stream_done` | terminal     | delivered as final response | delivered then channel closed    |
| `chunk`       | intermediate | filtered (logged, dropped)  | delivered                        |
| `started`     | intermediate | filtered (logged, dropped)  | delivered                        |
| `progress`    | intermediate | filtered (logged, dropped)  | delivered                        |
| `info`        | intermediate | filtered (logged, dropped)  | delivered                        |

Any unknown `type` is treated as terminal (safer default — delivered to the caller).

### Idle timeout (differs from `SendCommand`)

Streaming sessions can be long (think: TTS for a novel-length text), so `comTimeout` is applied as a **no-activity** timeout instead of a total command timeout:

- The timer resets on every frame received (intermediate OR terminal).
- If no frame arrives within `comTimeout`, the channel is closed, the process is marked not-ready, and a restart is triggered.

This lets you set a reasonable per-chunk deadline without capping total stream duration.

### Interrupt

`pool.Interrupt(cmdID)` works the same for streaming commands as for regular ones. The subprocess is expected to respond with an `interrupted` terminal frame; the library then closes the channel.

### Buffering & back-pressure

The streaming channel is buffered at capacity 256. If the consumer falls behind, the reader goroutine blocks on send — frames are never silently dropped (unlike the non-streaming path, which uses single-slot channels and logs a warning on overflow). Drain promptly.

### Subprocess protocol

Emit one JSON object per line on stdout. Example worker snippet:

```python
import json, sys
def emit(obj): sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()

emit({"type": "ready"})
for line in sys.stdin:
    cmd = json.loads(line)
    cid = cmd["id"]
    emit({"type": "started", "id": cid})
    for i, chunk in enumerate(generate_chunks(cmd["text"])):
        emit({"type": "chunk", "id": cid, "seq": i, "audio_b64": chunk})
    emit({"type": "success", "id": cid})
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