// process_pool_streaming_test.go
//
// Tests for SendCommandStreaming on both Process and ProcessPool.
// See process_pool.go:SendCommandStreaming for semantics.

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

// streamingWorkerSource is a subprocess that understands a few command shapes
// so the various streaming tests can share one binary:
//
//	{"id":..., "chunks": N, "terminal": "success"|"stream_done"|"error", "sleep_between_ms": M}
//	    -> emit N "chunk" frames, then a terminal frame of the requested type.
//	{"id":..., "chunks": N, "fail_after": K}
//	    -> emit K "chunk" frames then an "error" terminal frame.
//	{"id":..., "sleep": MS}
//	    -> emit "started" frame, sleep, then "success" (supports interrupt via ²INTERUPT²).
//	{"id":..., "stream_then_hang": N}
//	    -> emit N "chunk" frames then hang (no terminal) — used for idle-timeout test.
//	{"id":..., "rapid_chunks": N}
//	    -> emit N "chunk" frames as fast as possible, then "stream_done".
//	{"id":..., "stream_interruptible": MS}
//	    -> emit "chunk" every 50ms up to MS; on interrupt emit "interrupted" terminal.
const streamingWorkerSource = `
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
	currentCancel context.CancelFunc
	taskMu        sync.Mutex
	stdoutMu      sync.Mutex
	enc           = json.NewEncoder(os.Stdout)
)

func emit(m map[string]interface{}) {
	stdoutMu.Lock()
	enc.Encode(m)
	stdoutMu.Unlock()
}

func main() {
	emit(map[string]interface{}{"type": "ready"})

	msgChan := make(chan map[string]interface{}, 64)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var cmd map[string]interface{}
			if err := json.Unmarshal([]byte(line), &cmd); err != nil {
				continue
			}
			msgChan <- cmd
		}
	}()

	for cmd := range msgChan {
		if interrupt, ok := cmd["interrupt"]; ok && interrupt == "²INTERUPT²" {
			taskMu.Lock()
			if currentCancel != nil {
				currentCancel()
			}
			taskMu.Unlock()
			continue
		}

		id := cmd["id"]

		// Handle directives.
		if n, ok := cmd["chunks"].(float64); ok {
			failAfter, hasFail := cmd["fail_after"]
			terminal, hasTerm := cmd["terminal"].(string)
			sleepBetween := 0
			if s, ok := cmd["sleep_between_ms"].(float64); ok {
				sleepBetween = int(s)
			}
			total := int(n)
			for i := 0; i < total; i++ {
				if hasFail {
					if fa, ok := failAfter.(float64); ok && i >= int(fa) {
						emit(map[string]interface{}{"type": "error", "id": id, "message": "fail"})
						goto next
					}
				}
				emit(map[string]interface{}{"type": "chunk", "id": id, "seq": i})
				if sleepBetween > 0 {
					time.Sleep(time.Duration(sleepBetween) * time.Millisecond)
				}
			}
			if !hasTerm {
				terminal = "success"
			}
			emit(map[string]interface{}{"type": terminal, "id": id, "message": "done"})
		next:
			continue
		}

		if n, ok := cmd["rapid_chunks"].(float64); ok {
			total := int(n)
			for i := 0; i < total; i++ {
				emit(map[string]interface{}{"type": "chunk", "id": id, "seq": i})
			}
			emit(map[string]interface{}{"type": "stream_done", "id": id})
			continue
		}

		if n, ok := cmd["stream_then_hang"].(float64); ok {
			total := int(n)
			for i := 0; i < total; i++ {
				emit(map[string]interface{}{"type": "chunk", "id": id, "seq": i})
			}
			// Hang (no terminal). Sleep a long time; outer process will be killed on restart.
			time.Sleep(60 * time.Second)
			continue
		}

		if ms, ok := cmd["stream_interruptible"].(float64); ok {
			// Run the stream in a goroutine so the main loop keeps reading
			// msgChan and can deliver interrupts while the stream is active.
			go func(cmd map[string]interface{}, ms float64) {
				id := cmd["id"]
				taskMu.Lock()
				ctx, cancel := context.WithCancel(context.Background())
				currentCancel = cancel
				taskMu.Unlock()
				deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
				ticker := time.NewTicker(50 * time.Millisecond)
				seq := 0
			loop:
				for {
					select {
					case <-ctx.Done():
						emit(map[string]interface{}{"type": "interrupted", "id": id, "message": "int"})
						ticker.Stop()
						break loop
					case <-ticker.C:
						if time.Now().After(deadline) {
							emit(map[string]interface{}{"type": "success", "id": id, "message": "done"})
							ticker.Stop()
							break loop
						}
						emit(map[string]interface{}{"type": "chunk", "id": id, "seq": seq})
						seq++
					}
				}
				taskMu.Lock()
				currentCancel = nil
				taskMu.Unlock()
			}(cmd, ms)
			continue
		}

		// Default: simple success echo.
		emit(map[string]interface{}{"type": "success", "id": id, "data": cmd["data"]})
	}
}
`

// compileStreamingWorker builds the shared streaming-test subprocess binary and
// returns (tmpDir, binaryPath, cleanup).
func compileStreamingWorker(t *testing.T) (string, string, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "streaming_test")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tmpDir, "worker.go")
	if err := os.WriteFile(src, []byte(streamingWorkerSource), 0644); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatal(err)
	}
	bin := filepath.Join(tmpDir, "worker")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("compile streaming worker: %v\n%s", err, string(out))
	}
	return tmpDir, bin, func() { os.RemoveAll(tmpDir) }
}

// silentLogger returns a logger that discards most output to keep streaming
// test output readable. Streaming tests emit many debug lines that drown the
// signal otherwise.
func silentLogger() zerolog.Logger {
	return zerolog.New(os.Stderr).Level(zerolog.WarnLevel).With().Timestamp().Logger()
}

// collectStream drains the channel into a slice, returning the frames and
// whether the channel closed before the deadline.
func collectStream(t *testing.T, ch <-chan map[string]interface{}, deadline time.Duration) ([]map[string]interface{}, bool) {
	t.Helper()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	var frames []map[string]interface{}
	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				return frames, true
			}
			frames = append(frames, frame)
		case <-timer.C:
			return frames, false
		}
	}
}

// Test 1: Happy path — 3 chunks + success => 4 frames, channel closes.
func TestStreamingHappyPath(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-happy", 1, &logger, tmpDir, bin, []string{},
		5*time.Second, 5*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	ch, err := pool.SendCommandStreaming(map[string]interface{}{
		"id":       "happy-1",
		"chunks":   3.0,
		"terminal": "success",
	})
	if err != nil {
		t.Fatal(err)
	}

	frames, closed := collectStream(t, ch, 5*time.Second)
	if !closed {
		t.Fatal("channel did not close within deadline")
	}
	if len(frames) != 4 {
		t.Fatalf("expected 4 frames, got %d: %+v", len(frames), frames)
	}
	for i := 0; i < 3; i++ {
		if frames[i]["type"] != "chunk" {
			t.Errorf("frame %d: expected chunk, got %v", i, frames[i]["type"])
		}
	}
	if frames[3]["type"] != "success" {
		t.Errorf("last frame: expected success, got %v", frames[3]["type"])
	}
}

// Test 2: Error mid-stream — 2 chunks then error => 3 frames, channel closes.
func TestStreamingErrorMidStream(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-err", 1, &logger, tmpDir, bin, []string{},
		5*time.Second, 5*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	ch, err := pool.SendCommandStreaming(map[string]interface{}{
		"id":         "err-1",
		"chunks":     10.0,
		"fail_after": 2.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	frames, closed := collectStream(t, ch, 5*time.Second)
	if !closed {
		t.Fatal("channel did not close")
	}
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames (2 chunks + 1 error), got %d: %+v", len(frames), frames)
	}
	if frames[0]["type"] != "chunk" || frames[1]["type"] != "chunk" {
		t.Errorf("expected chunk, chunk, got %v, %v", frames[0]["type"], frames[1]["type"])
	}
	if frames[2]["type"] != "error" {
		t.Errorf("expected error terminal, got %v", frames[2]["type"])
	}
}

// Test 3: Non-streaming SendCommand against a streaming-emitting subprocess.
// Chunks must be filtered; only the final success delivered.
func TestStreamingChunksFilteredByRegularSendCommand(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-filter", 1, &logger, tmpDir, bin, []string{},
		5*time.Second, 5*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	resp, err := pool.SendCommand(map[string]interface{}{
		"id":       "filter-1",
		"chunks":   5.0,
		"terminal": "success",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "success" {
		t.Errorf("expected success, got %v (full: %+v)", resp["type"], resp)
	}
}

// Test 4: Interrupt during stream.
func TestStreamingInterrupt(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-int", 1, &logger, tmpDir, bin, []string{},
		10*time.Second, 30*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	cmdID := "int-stream-1"
	ch, err := pool.SendCommandStreaming(map[string]interface{}{
		"id":                   cmdID,
		"stream_interruptible": 5000.0, // 5 seconds of chunking
	})
	if err != nil {
		t.Fatal(err)
	}

	// Let a couple chunks flow then interrupt.
	time.Sleep(200 * time.Millisecond)
	if err := pool.Interrupt(cmdID); err != nil {
		t.Fatalf("interrupt failed: %v", err)
	}

	frames, closed := collectStream(t, ch, 5*time.Second)
	if !closed {
		t.Fatal("channel did not close after interrupt")
	}
	if len(frames) == 0 {
		t.Fatal("expected at least one frame before interrupt")
	}
	last := frames[len(frames)-1]
	if last["type"] != "interrupted" {
		t.Errorf("expected last frame type=interrupted, got %v (full: %+v)", last["type"], last)
	}
}

// Test 5: Idle timeout during stream — subprocess emits a few chunks then
// hangs. The idle timer should fire and close the channel.
func TestStreamingIdleTimeout(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	// Short comTimeout so the test runs fast. This is the idle timeout for
	// streaming commands.
	pool := NewProcessPool("stream-idle", 1, &logger, tmpDir, bin, []string{},
		10*time.Second, 500*time.Millisecond, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	ch, err := pool.SendCommandStreaming(map[string]interface{}{
		"id":               "idle-1",
		"stream_then_hang": 2.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	frames, closed := collectStream(t, ch, 5*time.Second)
	elapsed := time.Since(start)

	if !closed {
		t.Fatal("channel did not close after idle timeout")
	}
	if len(frames) < 2 {
		t.Errorf("expected at least 2 chunk frames before timeout, got %d", len(frames))
	}
	// Channel should close shortly after idle timeout (500ms) plus the time
	// for the initial chunks. Upper bound: a few seconds.
	if elapsed > 4*time.Second {
		t.Errorf("idle timeout took too long: %v", elapsed)
	}
	// Pool should recover for future commands.
	if err := pool.WaitForReady(); err != nil {
		t.Fatalf("pool did not recover after idle timeout: %v", err)
	}
}

// Test 6: Concurrent streaming + non-streaming commands on same pool.
func TestStreamingConcurrentWithNonStreaming(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-concurrent", 2, &logger, tmpDir, bin, []string{},
		10*time.Second, 10*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReadyAllProcess(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	// Streaming command.
	var streamFrames int
	var streamOK bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch, err := pool.SendCommandStreaming(map[string]interface{}{
			"id":               "concurrent-stream",
			"chunks":           5.0,
			"terminal":         "success",
			"sleep_between_ms": 50.0,
		})
		if err != nil {
			t.Errorf("streaming send failed: %v", err)
			return
		}
		frames, closed := collectStream(t, ch, 5*time.Second)
		streamFrames = len(frames)
		streamOK = closed && len(frames) == 6
	}()

	// Non-streaming command at the same time.
	var regularOK bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := pool.SendCommand(map[string]interface{}{
			"id":   "concurrent-regular",
			"data": "hello",
		})
		if err != nil {
			t.Errorf("regular send failed: %v", err)
			return
		}
		regularOK = resp["type"] == "success" && resp["data"] == "hello"
	}()

	wg.Wait()

	if !streamOK {
		t.Errorf("streaming command failed or had wrong frame count: got %d frames", streamFrames)
	}
	if !regularOK {
		t.Error("regular command failed")
	}
}

// Test 7: stream_done terminal type — subprocess emits 5 chunks + stream_done.
func TestStreamingStreamDoneTerminal(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-done", 1, &logger, tmpDir, bin, []string{},
		5*time.Second, 5*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	ch, err := pool.SendCommandStreaming(map[string]interface{}{
		"id":           "done-1",
		"rapid_chunks": 5.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	frames, closed := collectStream(t, ch, 5*time.Second)
	if !closed {
		t.Fatal("channel did not close")
	}
	if len(frames) != 6 {
		t.Fatalf("expected 6 frames (5 chunk + 1 stream_done), got %d", len(frames))
	}
	if frames[5]["type"] != "stream_done" {
		t.Errorf("expected stream_done terminal, got %v", frames[5]["type"])
	}
}

// Test 8: Channel buffer pressure — 500 chunks rapid, slow consumer, no loss.
func TestStreamingBufferPressure(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-pressure", 1, &logger, tmpDir, bin, []string{},
		30*time.Second, 30*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	ch, err := pool.SendCommandStreaming(map[string]interface{}{
		"id":           "pressure-1",
		"rapid_chunks": 500.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Slow consumer: sleep briefly every 50 frames to force back-pressure
	// through the buffered channels.
	seen := make(map[int]bool, 500)
	var lastSeq int = -1
	var terminal map[string]interface{}
	count := 0
	for frame := range ch {
		if ftype, _ := frame["type"].(string); ftype == "chunk" {
			if seq, ok := frame["seq"].(float64); ok {
				intSeq := int(seq)
				if intSeq != lastSeq+1 {
					t.Errorf("out of order: got seq=%d after %d", intSeq, lastSeq)
				}
				lastSeq = intSeq
				if seen[intSeq] {
					t.Errorf("duplicate seq: %d", intSeq)
				}
				seen[intSeq] = true
			}
			count++
			if count%50 == 0 {
				time.Sleep(5 * time.Millisecond)
			}
		} else {
			terminal = frame
		}
	}

	if count != 500 {
		t.Errorf("expected 500 chunks, got %d (last seq: %d)", count, lastSeq)
	}
	if terminal == nil || terminal["type"] != "stream_done" {
		t.Errorf("expected stream_done terminal, got %+v", terminal)
	}
}

// Test 9: Process.SendCommandStreaming direct (no pool) — sanity check that
// the lower-level API works standalone.
func TestStreamingProcessDirect(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-direct", 1, &logger, tmpDir, bin, []string{},
		5*time.Second, 5*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	worker, err := pool.GetWorker()
	if err != nil {
		t.Fatal(err)
	}
	defer worker.SetBusy(0)

	ch, err := worker.SendCommandStreaming(map[string]interface{}{
		"id":       "direct-1",
		"chunks":   2.0,
		"terminal": "stream_done",
	})
	if err != nil {
		t.Fatal(err)
	}

	frames, closed := collectStream(t, ch, 5*time.Second)
	if !closed {
		t.Fatal("channel did not close")
	}
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(frames))
	}
	// Verify ordering by seq.
	for i := 0; i < 2; i++ {
		if frames[i]["type"] != "chunk" {
			t.Errorf("frame %d not chunk", i)
		}
		if seq, ok := frames[i]["seq"].(float64); !ok || int(seq) != i {
			t.Errorf("frame %d wrong seq: %v", i, frames[i]["seq"])
		}
	}
	if frames[2]["type"] != "stream_done" {
		t.Errorf("last frame not stream_done: %v", frames[2]["type"])
	}
}

// Sanity: ensure uuid-assigned IDs still work (no id passed).
func TestStreamingAutoAssignedID(t *testing.T) {
	tmpDir, bin, cleanup := compileStreamingWorker(t)
	defer cleanup()

	logger := silentLogger()
	pool := NewProcessPool("stream-autoid", 1, &logger, tmpDir, bin, []string{},
		5*time.Second, 5*time.Second, 5*time.Second)
	defer pool.StopAll()

	if err := pool.WaitForReady(); err != nil {
		t.Fatal(err)
	}

	ch, err := pool.SendCommandStreaming(map[string]interface{}{
		"chunks":   2.0,
		"terminal": "success",
	})
	if err != nil {
		t.Fatal(err)
	}
	frames, closed := collectStream(t, ch, 5*time.Second)
	if !closed || len(frames) != 3 {
		t.Fatalf("expected 3 frames with auto-id, got %d (closed=%v)", len(frames), closed)
	}
}

// Sanity compile-time check that the returned types match the spec.
var (
	_ func(map[string]interface{}) (<-chan map[string]interface{}, error) = (*Process)(nil).SendCommandStreaming
	_ func(map[string]interface{}) (<-chan map[string]interface{}, error) = (*ProcessPool)(nil).SendCommandStreaming
)

// Compile-time assertion that fmt is used — guards against refactors removing
// the only fmt usage and breaking the build.
var _ = fmt.Sprintf
