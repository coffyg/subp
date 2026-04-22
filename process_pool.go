// process_pool.go
package subp

import (
	"bufio"
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Process is a process that can be started, stopped, and restarted.
type Process struct {
	cmd             *exec.Cmd
	isReady         int32
	isBusy          int32
	isRestarting    int32 // Flag to prevent concurrent restarts
	latency         int64
	mutex           sync.RWMutex
	commandMutex    sync.Mutex // Mutex for command send/receive operations
	logger          *zerolog.Logger
	stdin           *json.Encoder
	stdout          *bufio.Reader
	stderr          *bufio.Reader
	name            string
	cmdStr          string
	cmdArgs         []string
	timeout         time.Duration
	initTimeout     time.Duration
	requestsHandled int
	restarts        int
	id              int
	cwd             string
	pool            *ProcessPool
	wg              sync.WaitGroup

	// Added fields
	stdinPipe       io.WriteCloser
	stdoutPipe      io.ReadCloser
	stderrPipe      io.ReadCloser
	activeCommandID string         // Currently executing command ID
	restartHistory  []RestartEvent // Rolling history of last 10 restart events
	
	// Response routing for persistent reader.
	//
	// Two maps are maintained so the reader can cheaply distinguish regular
	// (single-response) commands from streaming commands without extra allocations
	// per-frame.
	//
	// Frame type taxonomy (must stay in sync with persistentReader):
	//   Terminal   : "success", "error", "interrupted", "stream_done"
	//   Intermediate: "chunk", "started", "progress", "info"
	//
	// For regular channels the intermediate frames are filtered (logged but not
	// delivered). For streaming channels EVERY frame is delivered; on terminal
	// frame the channel is closed after delivery.
	responseChannels  map[string]chan map[string]interface{} // cmdID -> regular (single-response) channel
	streamingChannels map[string]chan map[string]interface{} // cmdID -> streaming (multi-frame) channel
	responseMutex     sync.RWMutex                           // Mutex for both maps (reads take RLock, writes Lock)
	readerDone        chan struct{}                          // Signal reader to stop

	// streamingActivity is keyed by cmdID and stores a pointer to a monotonically
	// increasing counter that persistentReader bumps on every received frame.
	// SendCommandStreaming uses this counter to implement the no-activity
	// ("idle") timeout: if the counter does not advance within p.timeout the
	// stream is considered dead. This avoids a per-frame channel send from the
	// reader to a timer goroutine (which would cost allocations on the hot path).
	streamingActivity map[string]*int64
	
	// Performance tracking
	successCount    int       // Total successful commands
	failureCount    int       // Total failed commands
	lastActiveTime  time.Time // Last time a command completed successfully
	lastErrorTime   time.Time // Last time a command failed
}

// RestartEvent tracks details of a restart event
type RestartEvent struct {
	Time      time.Time `json:"time"`
	EventType string    `json:"event_type"` // timeout, send_error, read_error, queue_timeout, manual
	TriggerID string    `json:"trigger_id"` // Command ID that triggered restart
}

// ProcessExport exports process information.
type ProcessExport struct {
	IsReady         bool           `json:"IsReady"`
	Latency         int64          `json:"Latency"`
	Name            string         `json:"Name"`
	Restarts        int            `json:"Restarts"`
	RequestsHandled int            `json:"RequestsHandled"`
	RestartHistory  []RestartEvent `json:"RestartHistory,omitempty"`
	SuccessRate     float64        `json:"SuccessRate"`     // Percentage of successful commands (0.0-1.0)
	LastActiveTime  *time.Time     `json:"LastActiveTime"`  // Last successful command completion
	LastErrorTime   *time.Time     `json:"LastErrorTime"`   // Last command failure
}

// Start starts the process by creating a new exec.Cmd, setting up the stdin and stdout pipes, and starting the process.
func (p *Process) Start() {
	p.SetReady(0)
	cmd := exec.Command(p.cmdStr, p.cmdArgs...)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to get stdin pipe for process", p.name)
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to get stdout pipe for process", p.name)
		stdinPipe.Close()
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to get stderr pipe for process", p.name)
		stdinPipe.Close()
		stdoutPipe.Close()
		return
	}

	p.mutex.Lock()
	p.cmd = cmd
	p.stdinPipe = stdinPipe
	p.stdoutPipe = stdoutPipe
	p.stderrPipe = stderrPipe
	p.stdin = json.NewEncoder(stdinPipe)
	p.stdout = bufio.NewReader(stdoutPipe)
	p.stderr = bufio.NewReader(stderrPipe)
	p.mutex.Unlock()

	p.cmd.Dir = p.cwd
	if err := p.cmd.Start(); err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to start process", p.name)
		// Clean up pipes on start failure
		stdinPipe.Close()
		stdoutPipe.Close()
		stderrPipe.Close()
		// Reset the stored references
		p.mutex.Lock()
		p.cmd = nil
		p.stdinPipe = nil
		p.stdoutPipe = nil
		p.stderrPipe = nil
		p.stdin = nil
		p.stdout = nil
		p.stderr = nil
		p.mutex.Unlock()
		return
	}

	// Initialize response routing
	p.responseMutex.Lock()
	p.responseChannels = make(map[string]chan map[string]interface{})
	p.streamingChannels = make(map[string]chan map[string]interface{})
	p.streamingActivity = make(map[string]*int64)
	p.readerDone = make(chan struct{})
	p.responseMutex.Unlock()
	
	// Only start goroutines after process successfully starts
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		p.readStderr()
	}()
	go func() {
		defer p.wg.Done()
		// First wait for ready, then start persistent reader
		p.WaitForReadyScan()
		// Now start the persistent reader after ready is received
		p.persistentReader()
	}()
}

// Stop stops the process by sending a kill signal to the process and cleaning up the resources.
func (p *Process) Stop() {
	p.SetReady(0)
	
	// Signal reader to stop
	p.responseMutex.Lock()
	if p.readerDone != nil {
		close(p.readerDone)
	}
	p.responseMutex.Unlock()
	
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	p.wg.Wait()
	p.cleanupChannelsAndResources()
	p.logger.Info().Msgf("[nyxsub|%s] Process stopped", p.name)
}

// cleanupChannelsAndResources closes the pipes and resets the pointers.
func (p *Process) cleanupChannelsAndResources() {
	p.mutex.Lock()
	if p.stdinPipe != nil {
		p.stdinPipe.Close()
		p.stdinPipe = nil
	}
	if p.stdoutPipe != nil {
		p.stdoutPipe.Close()
		p.stdoutPipe = nil
	}
	if p.stderrPipe != nil {
		p.stderrPipe.Close()
		p.stderrPipe = nil
	}
	p.stdin = nil
	p.stdout = nil
	p.stderr = nil
	p.cmd = nil
	p.mutex.Unlock()
	
	// Clean up response channels
	p.responseMutex.Lock()
	for _, ch := range p.responseChannels {
		close(ch)
	}
	p.responseChannels = nil
	// Streaming channels are also closed here so any caller ranging over them
	// unblocks when the process is torn down.
	for _, ch := range p.streamingChannels {
		// Guard against double-close (channel already closed by the reader
		// after a terminal frame). There is no cheap way to test if a channel
		// is closed in Go, so we recover from the panic that close() raises.
		func(c chan map[string]interface{}) {
			defer func() { _ = recover() }()
			close(c)
		}(ch)
	}
	p.streamingChannels = nil
	p.streamingActivity = nil
	p.responseMutex.Unlock()
}

// addRestartEvent adds a restart event to the rolling history (max 10)
func (p *Process) addRestartEvent(eventType string, triggerID string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	event := RestartEvent{
		Time:      time.Now(),
		EventType: eventType,
		TriggerID: triggerID,
	}

	// Add to history
	p.restartHistory = append(p.restartHistory, event)

	// Keep only last 10 events
	if len(p.restartHistory) > 10 {
		p.restartHistory = p.restartHistory[len(p.restartHistory)-10:]
	}
}

// Restart stops the process and starts it again.
func (p *Process) Restart() {
	// Prevent concurrent restarts using atomic compare-and-swap
	if !atomic.CompareAndSwapInt32(&p.isRestarting, 0, 1) {
		// Another restart is already in progress
		p.logger.Debug().Msgf("[nyxsub|%s] Restart already in progress, skipping", p.name)
		return
	}
	defer atomic.StoreInt32(&p.isRestarting, 0)

	p.logger.Info().Msgf("[nyxsub|%s] Restarting process", p.name)
	p.mutex.Lock()
	p.restarts++
	p.mutex.Unlock()
	p.Stop()
	if atomic.LoadInt32(&p.pool.shouldStop) == 0 {
		p.Start()
	}
}

// SetReady sets the readiness of the process.
func (p *Process) SetReady(ready int32) {
	wasReady := atomic.SwapInt32(&p.isReady, ready)
	// If transitioning to ready and not busy, add to available workers
	if wasReady == 0 && ready == 1 && !p.IsBusy() && p.pool != nil && atomic.LoadInt32(&p.pool.shouldStop) == 0 {
		select {
		case p.pool.availableWorkers <- p:
			// Successfully added to channel
		default:
			// Channel is full, should not happen with proper buffer size
			p.logger.Warn().Msgf("[nyxsub|%s] Available workers channel is full", p.name)
		}
	}
}

// IsReady checks if the process is ready.
func (p *Process) IsReady() bool {
	return atomic.LoadInt32(&p.isReady) == 1
}

// IsBusy checks if the process is busy.
func (p *Process) IsBusy() bool {
	return atomic.LoadInt32(&p.isBusy) == 1
}

// SetBusy sets the busy status of the process.
func (p *Process) SetBusy(busy int32) {
	wasbusy := atomic.SwapInt32(&p.isBusy, busy)
	// If transitioning from busy to not busy, and process is ready, add to available workers
	if wasbusy == 1 && busy == 0 && p.IsReady() && p.pool != nil && atomic.LoadInt32(&p.pool.shouldStop) == 0 {
		select {
		case p.pool.availableWorkers <- p:
			// Successfully added to channel
		default:
			// Channel is full, should not happen with proper buffer size
			p.logger.Warn().Msgf("[nyxsub|%s] Available workers channel is full", p.name)
		}
	}
}

// readStderr reads from stderr and logs any output.
func (p *Process) readStderr() {
	for {
		line, err := p.stderr.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stderr", p.name)
			}
			return
		}
		if line != "" && line != "\n" {
			p.logger.Error().Msgf("[nyxsub|%s|stderr] %s", p.name, line)
		}
	}
}

// WaitForReadyScan waits for the process to send a "ready" message.
// This runs during startup, then exits so persistentReader can take over
func (p *Process) WaitForReadyScan() {
	for {
		line, err := p.stdout.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stdout during ready scan", p.name)
			}
			// Signal that the process needs restart by marking it as not ready
			p.SetReady(0)
			return
		}
		if line == "" || line == "\n" {
			continue
		}

		var response map[string]interface{}
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			p.logger.Warn().Msgf("[nyxsub|%s] Non JSON message received during ready scan: '%s'", p.name, line)
			continue
		}

		if response["type"] == "ready" {
			p.logger.Info().Msgf("[nyxsub|%s] Process is ready", p.name)
			p.SetReady(1)
			return
		}
	}
}

// SendCommand sends a command to the process and waits for the response.
func (p *Process) SendCommand(cmd map[string]interface{}) (map[string]interface{}, error) {
	p.SetBusy(1)
	defer p.SetBusy(0)

	if _, ok := cmd["id"]; !ok {
		cmd["id"] = uuid.New().String()
	}
	if _, ok := cmd["type"]; !ok {
		cmd["type"] = "main"
	}

	start := time.Now().UnixMilli()
	cmdID := cmd["id"].(string)

	// Create response channel for this command
	respChan := make(chan map[string]interface{}, 1)
	p.responseMutex.Lock()
	p.responseChannels[cmdID] = respChan
	p.responseMutex.Unlock()
	
	// Clean up response channel on exit
	defer func() {
		p.responseMutex.Lock()
		delete(p.responseChannels, cmdID)
		p.responseMutex.Unlock()
	}()

	// Lock for command operations to prevent concurrent access to stdin
	p.commandMutex.Lock()

	// Track active command
	p.activeCommandID = cmdID

	// Send command
	if err := p.stdin.Encode(cmd); err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to send command", p.name)
		// Track failure
		p.mutex.Lock()
		p.failureCount++
		p.lastErrorTime = time.Now()
		p.mutex.Unlock()
		// Track restart event
		p.addRestartEvent("send_error", cmdID)
		// Clean up local command tracking
		p.activeCommandID = ""
		// Mark as not ready before unlocking to prevent new commands
		p.SetReady(0)
		p.commandMutex.Unlock()
		p.Restart()
		return nil, err
	}

	// Log the command sent
	jsonCmd, _ := json.Marshal(cmd)
	p.logger.Debug().Msgf("[nyxsub|%s] Command sent: %v", p.name, string(jsonCmd))
	
	// Unlock immediately after sending - we don't need to hold the lock while waiting
	p.commandMutex.Unlock()

	// Wait for response with timeout
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	
	select {
	case response := <-respChan:
		// Got response
		p.activeCommandID = ""
		
		p.mutex.Lock()
		p.latency = time.Now().UnixMilli() - start
		p.requestsHandled++
		p.successCount++
		p.lastActiveTime = time.Now()
		p.mutex.Unlock()
		
		return response, nil
	case <-timer.C:
		// Timeout
		p.logger.Error().Msgf("[nyxsub|%s] Communication timed out for command %s", p.name, cmdID)
		// Track failure
		p.mutex.Lock()
		p.failureCount++
		p.lastErrorTime = time.Now()
		p.mutex.Unlock()
		// Track restart event
		p.addRestartEvent("timeout", cmdID)
		// Clean up local command tracking
		p.activeCommandID = ""
		// Mark as not ready to prevent new commands
		p.SetReady(0)
		p.Restart()
		return nil, errors.New("communication timed out")
	case <-p.readerDone:
		// Reader died, process needs restart
		p.logger.Error().Msgf("[nyxsub|%s] Reader died while waiting for command %s", p.name, cmdID)
		// Track failure
		p.mutex.Lock()
		p.failureCount++
		p.lastErrorTime = time.Now()
		p.mutex.Unlock()
		p.addRestartEvent("reader_died", cmdID)
		p.activeCommandID = ""
		p.SetReady(0)
		p.Restart()
		return nil, errors.New("reader died")
	}
}

// streamChanCapacity is the buffer size for streaming response channels.
// Chosen large enough to absorb bursts of audio/video chunks without forcing
// the reader to block on a slow consumer, yet bounded so that a completely
// stalled consumer eventually exerts back-pressure instead of growing memory.
const streamChanCapacity = 256

// SendCommandStreaming sends a command and returns a receive-only channel
// that will receive EVERY frame (intermediate AND terminal) from the
// subprocess for this command.
//
// Semantics:
//   - The returned channel is buffered (capacity 256).
//   - The library closes the channel after the terminal frame is delivered,
//     OR on error/idle-timeout/interrupt/process-death.
//   - The caller MUST range over the channel until closed.
//   - No-activity ("idle") timeout: p.timeout is the MAX gap between frames,
//     not the total command duration. The timer resets on every frame.
//   - On timeout or send error the process is marked not ready and restarted
//     (same behavior as SendCommand).
//
// The public single-response SendCommand is unchanged and remains the
// recommended API for non-streaming workloads.
func (p *Process) SendCommandStreaming(cmd map[string]interface{}) (<-chan map[string]interface{}, error) {
	p.SetBusy(1)

	if _, ok := cmd["id"]; !ok {
		cmd["id"] = uuid.New().String()
	}
	if _, ok := cmd["type"]; !ok {
		cmd["type"] = "main"
	}

	cmdID := cmd["id"].(string)
	start := time.Now().UnixMilli()

	// Create streaming channel and activity counter.
	streamCh := make(chan map[string]interface{}, streamChanCapacity)
	var activity int64

	p.responseMutex.Lock()
	if p.streamingChannels == nil {
		// Process already torn down.
		p.responseMutex.Unlock()
		p.SetBusy(0)
		close(streamCh)
		return nil, errors.New("process not running")
	}
	p.streamingChannels[cmdID] = streamCh
	p.streamingActivity[cmdID] = &activity
	p.responseMutex.Unlock()

	// We return a user-facing channel separate from the one the reader writes
	// to: that way we can multiplex timeout/readerDone signals into the close
	// of the user channel without racing with the reader's close. `out` is
	// the user-facing channel; `streamCh` is fed by the reader.
	out := make(chan map[string]interface{}, streamChanCapacity)

	p.commandMutex.Lock()
	p.activeCommandID = cmdID

	if err := p.stdin.Encode(cmd); err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to send streaming command", p.name)
		p.mutex.Lock()
		p.failureCount++
		p.lastErrorTime = time.Now()
		p.mutex.Unlock()
		p.addRestartEvent("send_error", cmdID)
		p.activeCommandID = ""
		p.SetReady(0)
		p.commandMutex.Unlock()

		// Clean up registration.
		p.responseMutex.Lock()
		if p.streamingChannels != nil {
			delete(p.streamingChannels, cmdID)
			delete(p.streamingActivity, cmdID)
		}
		p.responseMutex.Unlock()

		close(out)
		p.SetBusy(0)
		go p.Restart()
		return nil, err
	}

	jsonCmd, _ := json.Marshal(cmd)
	p.logger.Debug().Msgf("[nyxsub|%s] Streaming command sent: %v", p.name, string(jsonCmd))
	p.commandMutex.Unlock()

	// Forwarder goroutine: copies frames reader->out while enforcing the
	// idle-timeout. Exits when:
	//   - streamCh is closed by the reader (normal terminal delivery)
	//   - idle timeout fires
	//   - readerDone fires (process death / Stop())
	go func() {
		defer func() {
			// Release busy and clear active cmd id ONCE the stream is done.
			p.activeCommandID = ""
			p.SetBusy(0)
			close(out)
		}()

		// Snapshot readerDone once to avoid repeated mutex reads.
		p.responseMutex.RLock()
		readerDone := p.readerDone
		p.responseMutex.RUnlock()

		// Record the activity counter we started with. Each tick we compare
		// against the current value; if it's advanced we know a frame
		// arrived and we reset our reference.
		lastSeen := atomic.LoadInt64(&activity)

		ticker := time.NewTicker(p.timeout)
		defer ticker.Stop()

		terminalSeen := false
		for {
			select {
			case frame, ok := <-streamCh:
				if !ok {
					// Reader closed the stream (terminal frame already delivered
					// on the previous iteration, or process death cleanup).
					if terminalSeen {
						// Normal terminal completion - success metrics.
						p.mutex.Lock()
						p.latency = time.Now().UnixMilli() - start
						p.requestsHandled++
						p.successCount++
						p.lastActiveTime = time.Now()
						p.mutex.Unlock()
					}
					return
				}
				// Deliver the frame. Blocking send: streaming consumers must
				// drain promptly. We still race against readerDone so shutdown
				// can interrupt a stuck consumer.
				select {
				case out <- frame:
				case <-readerDone:
					return
				}
				// Track whether we saw a terminal frame; the reader will
				// close streamCh on the next iteration.
				if t, ok := frame["type"].(string); ok && isTerminalFrameType(t) {
					terminalSeen = true
				}
				// Reset idle timer.
				ticker.Reset(p.timeout)
				lastSeen = atomic.LoadInt64(&activity)

			case <-ticker.C:
				// Check atomically whether activity advanced since last tick.
				cur := atomic.LoadInt64(&activity)
				if cur != lastSeen {
					// A frame did arrive but our goroutine was not scheduled
					// on the streamCh case in time. Treat as activity.
					lastSeen = cur
					continue
				}
				// Real idle timeout.
				p.logger.Error().Msgf("[nyxsub|%s] Streaming idle timeout for command %s", p.name, cmdID)
				p.mutex.Lock()
				p.failureCount++
				p.lastErrorTime = time.Now()
				p.mutex.Unlock()
				p.addRestartEvent("timeout", cmdID)

				// Unregister so the reader stops delivering to streamCh and
				// releases its reference.
				p.responseMutex.Lock()
				if p.streamingChannels != nil {
					if ch, ok := p.streamingChannels[cmdID]; ok && ch == streamCh {
						delete(p.streamingChannels, cmdID)
						delete(p.streamingActivity, cmdID)
					}
				}
				p.responseMutex.Unlock()

				p.SetReady(0)
				go p.Restart()
				return

			case <-readerDone:
				// Process being torn down.
				return
			}
		}
	}()

	return out, nil
}

// WaitForReady waits until this specific process is ready or times out.
func (p *Process) WaitForReady() error {
	start := time.Now()
	for {
		if atomic.LoadInt32(&p.isReady) == 1 {
			return nil
		}
		if time.Since(start) > p.initTimeout {
			return fmt.Errorf("timeout waiting for process %s to be ready", p.name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// isTerminalFrameType reports whether a response "type" marks the end of a
// command exchange. Kept as a small inline-friendly helper so the hot path
// in persistentReader stays branch-predictable.
//
// Terminal types:
//   "success", "error", "interrupted", "stream_done"
//
// Everything else (including "chunk", "started", "progress", "info" and any
// unknown string) is treated as intermediate.
func isTerminalFrameType(t string) bool {
	switch t {
	case "success", "error", "interrupted", "stream_done":
		return true
	}
	return false
}

// persistentReader continuously reads from stdout and routes responses to
// waiting commands.
//
// Routing rules:
//   1. If cmdID has a streaming channel registered, EVERY frame is delivered
//      to it (blocking send — streams must not silently drop frames). On a
//      terminal frame the channel is closed after delivery and both streaming
//      maps are cleared under the write lock.
//   2. Else if cmdID has a regular response channel, only terminal frames are
//      delivered; intermediate frames ("chunk", "started", "progress", "info")
//      are logged and dropped. Delivery is non-blocking (single-slot channel)
//      to preserve existing behavior.
//   3. Else the frame is logged at debug level (late response from an
//      interrupted or already-completed command).
//
// Note on mutex strategy: we take the RWMutex once per line and do all
// routing inside. A previous version took the lock twice (once to check
// streaming, once for regular); consolidating avoids the extra atomic op per
// frame. The write-lock path is only taken on terminal streaming frames
// where we must atomically delete+close.
func (p *Process) persistentReader() {
	for {
		// Check if we should stop
		select {
		case <-p.readerDone:
			return
		default:
		}

		// Read next line (this blocks)
		line, err := p.stdout.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stdout in persistent reader", p.name)
			} else {
				p.logger.Error().Msgf("[nyxsub|%s] Process died (EOF on stdout)", p.name)
			}
			// Reader failed (including EOF which means process died), mark as not ready.
			// Close any live streaming channels so ranging callers unblock.
			p.responseMutex.Lock()
			for id, ch := range p.streamingChannels {
				func(c chan map[string]interface{}) {
					defer func() { _ = recover() }()
					close(c)
				}(ch)
				delete(p.streamingChannels, id)
				delete(p.streamingActivity, id)
			}
			p.responseMutex.Unlock()
			p.SetReady(0)
			return
		}

		if line == "" || line == "\n" {
			continue
		}

		// Parse JSON response
		var response map[string]interface{}
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			p.logger.Warn().Msgf("[nyxsub|%s] Non JSON message received: '%s'", p.name, line)
			continue
		}

		cmdID, hasID := response["id"].(string)
		if !hasID {
			continue
		}

		// Look up both maps under a single read-lock.
		p.responseMutex.RLock()
		streamCh, isStream := p.streamingChannels[cmdID]
		respChan, isRegular := p.responseChannels[cmdID]
		activity := p.streamingActivity[cmdID]
		p.responseMutex.RUnlock()

		// Bump idle-timeout counter for streaming commands BEFORE delivery,
		// so the waiter sees activity even if its goroutine is scheduled
		// between our send and the counter bump.
		if activity != nil {
			atomic.AddInt64(activity, 1)
		}

		if isStream {
			// Streaming path — deliver every frame.
			responseType, _ := response["type"].(string)
			terminal := isTerminalFrameType(responseType)

			// Blocking send: streaming callers MUST drain their channel in a
			// timely manner. Capacity is 256, so only sustained back-pressure
			// can reach this branch. We still honor readerDone so shutdown
			// can interrupt a stuck send.
			select {
			case streamCh <- response:
			case <-p.readerDone:
				return
			}

			if terminal {
				// Close the channel and remove from both maps atomically.
				p.responseMutex.Lock()
				// Double-check: Stop() may have cleared the map concurrently.
				if ch, ok := p.streamingChannels[cmdID]; ok && ch == streamCh {
					delete(p.streamingChannels, cmdID)
					delete(p.streamingActivity, cmdID)
					func(c chan map[string]interface{}) {
						defer func() { _ = recover() }()
						close(c)
					}(streamCh)
				}
				p.responseMutex.Unlock()
			}
			continue
		}

		if isRegular {
			responseType, hasType := response["type"]
			if !hasType {
				// No type field - assume it's a final response
				select {
				case respChan <- response:
				default:
					p.logger.Warn().Msgf("[nyxsub|%s] Response channel full for command %s", p.name, cmdID)
				}
				continue
			}
			// Filter intermediate frames (defensive: "chunk" added so a
			// streaming-emitting subprocess accidentally called via SendCommand
			// does not deliver a chunk as the final response).
			switch responseType {
			case "started", "progress", "info", "chunk":
				p.logger.Debug().Msgf("[nyxsub|%s] Intermediate response for %s: %v", p.name, cmdID, responseType)
			default:
				// Final response (success, error, interrupted, stream_done, unknown)
				select {
				case respChan <- response:
				default:
					p.logger.Warn().Msgf("[nyxsub|%s] Response channel full for command %s", p.name, cmdID)
				}
			}
			continue
		}

		// No one waiting for this response (likely a late frame after interrupt).
		p.logger.Debug().Msgf("[nyxsub|%s] Received response for unknown command %s", p.name, cmdID)
	}
}

// queuedCommand represents a command waiting in the queue.
//
// For non-streaming commands, `response` receives a single commandResponse
// containing either the final map or an error, then is closed.
//
// For streaming commands, `streamResponse` receives a single streamSetup
// message containing either (a) the worker's stream channel and nil err, or
// (b) a nil channel and an error. Once delivered, the caller ranges over the
// worker's stream channel directly until the library closes it.
type queuedCommand struct {
	cmd            map[string]interface{}
	response       chan commandResponse // nil for streaming commands
	streamResponse chan streamSetup     // nil for non-streaming commands
}

// commandResponse holds the response or error from a command
type commandResponse struct {
	result map[string]interface{}
	err    error
}

// streamSetup is the one-shot handshake result for a streaming command:
// either the caller receives the stream channel (ch != nil, err == nil) or
// an error (ch == nil, err != nil). After delivery the caller ranges ch
// directly.
type streamSetup struct {
	ch  <-chan map[string]interface{}
	err error
}

// ProcessPool is a pool of processes.
type ProcessPool struct {
	processes         []*Process
	mutex             sync.RWMutex
	logger            *zerolog.Logger
	shouldStop        int32
	stop              chan bool
	workerTimeout     time.Duration
	comTimeout        time.Duration
	initTimeout       time.Duration
	availableWorkers  chan *Process       // Channel for available workers
	commandToProcess  map[string]*Process // Track which process is handling which command ID
	cancelledCommands map[string]bool     // Track cancelled command IDs
	commandQueue      chan *queuedCommand // Real queue for commands
	queueDepth        int                 // Maximum queue depth
}

// NewProcessPool creates a new process pool.
func NewProcessPool(
	name string,
	size int,
	logger *zerolog.Logger,
	cwd string,
	cmd string,
	cmdArgs []string,
	workerTimeout time.Duration,
	comTimeout time.Duration,
	initTimeout time.Duration,
) *ProcessPool {
	shouldStop := int32(0)
	queueDepth := 1000 // Reasonable default queue depth
	pool := &ProcessPool{
		processes:         make([]*Process, size),
		logger:            logger,
		mutex:             sync.RWMutex{},
		shouldStop:        shouldStop,
		stop:              make(chan bool, 1),
		workerTimeout:     workerTimeout,
		comTimeout:        comTimeout,
		initTimeout:       initTimeout,
		availableWorkers:  make(chan *Process, size), // Buffer size = number of workers
		commandToProcess:  make(map[string]*Process),
		cancelledCommands: make(map[string]bool),
		commandQueue:      make(chan *queuedCommand, queueDepth),
		queueDepth:        queueDepth,
	}
	// Create all processes first (without starting them)
	for i := 0; i < size; i++ {
		pool.newProcess(name, i, cmd, cmdArgs, logger, cwd)
	}

	// Start processes sequentially and wait for each to be ready
	for i := 0; i < size; i++ {
		pool.processes[i].Start()
		if err := pool.processes[i].WaitForReady(); err != nil {
			logger.Error().Err(err).Msgf("Failed to start process %s", pool.processes[i].name)
			// Continue with other processes even if one fails
		}
	}

	// Start the dispatcher goroutine
	go pool.dispatcher()

	return pool
}

// SetShouldStop sets the shouldStop flag.
func (pool *ProcessPool) SetShouldStop(ready int32) {
	atomic.StoreInt32(&pool.shouldStop, ready)
}

// SetStop sets the pool to stop.
func (pool *ProcessPool) SetStop() {
	pool.SetShouldStop(1)
	pool.stop <- true
}

// newProcess creates a new process in the process pool.
func (pool *ProcessPool) newProcess(name string, i int, cmd string, cmdArgs []string, logger *zerolog.Logger, cwd string) {
	pool.mutex.Lock()
	pool.processes[i] = &Process{
		isReady:         0,
		isBusy:          0,
		isRestarting:    0,
		latency:         0,
		logger:          logger,
		name:            fmt.Sprintf("%s#%d", name, i),
		cmdStr:          cmd,
		cmdArgs:         cmdArgs,
		timeout:         pool.comTimeout,
		initTimeout:     pool.initTimeout,
		requestsHandled: 0,
		restarts:        0,
		id:              i,
		cwd:             cwd,
		pool:            pool,
		restartHistory:  make([]RestartEvent, 0, 10), // Initialize with capacity 10
	}
	pool.mutex.Unlock()
	// Start() will be called manually in NewProcessPool for sequential startup
}

// ExportAll exports all the processes in the process pool as a slice of ProcessExport.
func (pool *ProcessPool) ExportAll() []ProcessExport {
	pool.mutex.RLock()
	var exports []ProcessExport
	for _, process := range pool.processes {
		if process != nil {
			process.mutex.Lock()
			// Copy restart history to avoid race conditions
			historyCopy := make([]RestartEvent, len(process.restartHistory))
			copy(historyCopy, process.restartHistory)
			
			// Calculate success rate
			var successRate float64
			totalCommands := process.successCount + process.failureCount
			if totalCommands > 0 {
				successRate = float64(process.successCount) / float64(totalCommands)
			}
			
			// Prepare time pointers
			var lastActivePtr *time.Time
			var lastErrorPtr *time.Time
			if !process.lastActiveTime.IsZero() {
				lastActivePtr = &process.lastActiveTime
			}
			if !process.lastErrorTime.IsZero() {
				lastErrorPtr = &process.lastErrorTime
			}
			
			exports = append(exports, ProcessExport{
				IsReady:         atomic.LoadInt32(&process.isReady) == 1,
				Latency:         process.latency,
				Name:            process.name,
				Restarts:        process.restarts,
				RequestsHandled: process.requestsHandled,
				RestartHistory:  historyCopy,
				SuccessRate:     successRate,
				LastActiveTime:  lastActivePtr,
				LastErrorTime:   lastErrorPtr,
			})
			process.mutex.Unlock()
		}
	}
	pool.mutex.RUnlock()
	return exports
}

// GetWorker returns a worker process from the process pool.
// GetWorker returns a worker process from the process pool.
func (pool *ProcessPool) GetWorker() (*Process, error) {
	timeoutTimer := time.After(pool.workerTimeout)
	ticker := time.NewTicker(time.Millisecond * 10) // Much faster ticker for fallback
	defer ticker.Stop()

	for {
		select {
		case worker, ok := <-pool.availableWorkers:
			if !ok {
				// Channel is closed, pool is stopping
				return nil, fmt.Errorf("pool is stopping")
			}
			// Worker was available in the channel
			worker.SetBusy(1)
			return worker, nil
		case <-ticker.C:
			// Fallback: check if any workers are available that weren't in the channel
			// This handles initialization and edge cases
			pool.mutex.RLock()
			for _, process := range pool.processes {
				if process != nil && process.IsReady() && !process.IsBusy() {
					// Try to mark as busy atomically
					if atomic.CompareAndSwapInt32(&process.isBusy, 0, 1) {
						pool.mutex.RUnlock()
						return process, nil
					}
				}
			}
			pool.mutex.RUnlock()
		case <-timeoutTimer:
			return nil, fmt.Errorf("timeout exceeded, no available workers")
		}
	}
}

func (pool *ProcessPool) WaitForReadyAllProcess() error {
	start := time.Now()
	for {
		pool.mutex.RLock()
		ready := true
		for _, process := range pool.processes {
			if process != nil && atomic.LoadInt32(&process.isReady) == 0 {
				ready = false
				break
			}
		}
		pool.mutex.RUnlock()
		if ready {
			return nil
		}
		if time.Since(start) > pool.initTimeout {
			return fmt.Errorf("timeout waiting for all workers to be ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// WaitForReady waits until at least one worker is ready or times out.
func (pool *ProcessPool) WaitForReady() error {
	start := time.Now()
	for {
		pool.mutex.RLock()
		ready := false
		for _, process := range pool.processes {
			if process != nil && atomic.LoadInt32(&process.isReady) == 1 {
				ready = true
				break
			}
		}
		pool.mutex.RUnlock()
		if ready {
			return nil
		}
		if time.Since(start) > pool.initTimeout {
			return fmt.Errorf("timeout waiting for workers to be ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// SendCommand sends a command to a worker in the process pool.
func (pool *ProcessPool) SendCommand(cmd map[string]interface{}) (map[string]interface{}, error) {
	// Check if pool is stopping
	if atomic.LoadInt32(&pool.shouldStop) == 1 {
		return nil, fmt.Errorf("pool is stopping")
	}

	// Extract or assign command ID first
	var cmdID string
	if id, ok := cmd["id"]; ok {
		cmdID = id.(string)
	} else {
		cmdID = uuid.New().String()
		cmd["id"] = cmdID
	}

	// Track command as queued
	pool.mutex.Lock()
	pool.commandToProcess[cmdID] = nil // nil means queued, waiting for worker
	pool.mutex.Unlock()

	// Create response channel for this command
	respChan := make(chan commandResponse, 1)

	// Create queued command
	qCmd := &queuedCommand{
		cmd:      cmd,
		response: respChan,
	}

	// Try to add to queue
	select {
	case pool.commandQueue <- qCmd:
		// Successfully queued
	default:
		// Queue is full
		pool.mutex.Lock()
		delete(pool.commandToProcess, cmdID)
		pool.mutex.Unlock()
		return nil, fmt.Errorf("command queue is full (depth: %d)", pool.queueDepth)
	}

	// Wait for response from dispatcher
	resp := <-respChan

	return resp.result, resp.err
}

// SendCommandStreaming sends a command to a worker and returns a channel
// delivering every frame for that command.
//
// See Process.SendCommandStreaming for semantics. At the pool level this
// function additionally:
//   - Respects the queue depth limit (returns error if queue is full).
//   - Honors Interrupt(cmdID) both while queued and while streaming.
//   - Maintains commandToProcess tracking for the full lifetime of the stream.
//
// The returned channel is closed by the library after the terminal frame,
// or on error/timeout/interrupt/process-death. Range over it until closed.
func (pool *ProcessPool) SendCommandStreaming(cmd map[string]interface{}) (<-chan map[string]interface{}, error) {
	if atomic.LoadInt32(&pool.shouldStop) == 1 {
		return nil, fmt.Errorf("pool is stopping")
	}

	var cmdID string
	if id, ok := cmd["id"]; ok {
		cmdID = id.(string)
	} else {
		cmdID = uuid.New().String()
		cmd["id"] = cmdID
	}

	pool.mutex.Lock()
	pool.commandToProcess[cmdID] = nil // queued
	pool.mutex.Unlock()

	setupChan := make(chan streamSetup, 1)
	qCmd := &queuedCommand{
		cmd:            cmd,
		streamResponse: setupChan,
	}

	select {
	case pool.commandQueue <- qCmd:
	default:
		pool.mutex.Lock()
		delete(pool.commandToProcess, cmdID)
		pool.mutex.Unlock()
		return nil, fmt.Errorf("command queue is full (depth: %d)", pool.queueDepth)
	}

	setup := <-setupChan
	if setup.err != nil {
		return nil, setup.err
	}
	return setup.ch, nil
}

// Interrupt sends an interrupt signal to the process handling the specified command ID.
func (pool *ProcessPool) Interrupt(id string) error {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()

	process, exists := pool.commandToProcess[id]

	if !exists {
		pool.logger.Error().Msgf("No command found with ID: %s", id)
		return fmt.Errorf("no command found with ID: %s", id)
	}

	// If process is nil, command is queued but not yet executing - mark as cancelled
	if process == nil {
		pool.cancelledCommands[id] = true
		return nil // Successfully cancelled queued command
	}

	// Command is executing, send interrupt message directly to the process
	interruptCmd := map[string]interface{}{
		"interrupt": "²INTERUPT²",
	}

	// Temporarily unlock to avoid deadlock with process mutex
	pool.mutex.Unlock()

	// We need to send this directly without going through SendCommand
	// to avoid blocking or interfering with the active command
	err := process.stdin.Encode(interruptCmd)

	if err != nil {
		pool.logger.Error().Err(err).Msgf("Failed to send interrupt to process %s", process.name)
	}

	// Relock for defer unlock
	pool.mutex.Lock()

	// Interrupt request submitted (will be sent asynchronously)
	return nil
}

// dispatcher pulls commands from the queue and assigns them to available workers
func (pool *ProcessPool) dispatcher() {
	for {
		select {
		case <-pool.stop:
			// Pool is stopping, exit dispatcher
			return
		case qCmd, ok := <-pool.commandQueue:
			if !ok {
				// Channel is closed, exit dispatcher
				return
			}
			// Get command ID for tracking
			cmdID, cmdOk := qCmd.cmd["id"].(string)
			if !cmdOk {
				cmdID = uuid.New().String()
				qCmd.cmd["id"] = cmdID
			}

			// Check if command was cancelled while in queue
			pool.mutex.RLock()
			cancelled := pool.cancelledCommands[cmdID]
			pool.mutex.RUnlock()

			if cancelled {
				// Command was cancelled, send error response
				pool.mutex.Lock()
				delete(pool.cancelledCommands, cmdID)
				delete(pool.commandToProcess, cmdID)
				pool.mutex.Unlock()

				errMsg := fmt.Errorf("command %s was interrupted while queued", cmdID)
				if qCmd.streamResponse != nil {
					qCmd.streamResponse <- streamSetup{err: errMsg}
					close(qCmd.streamResponse)
				} else {
					qCmd.response <- commandResponse{err: errMsg}
					close(qCmd.response)
				}
				continue
			}

			// Wait for an available worker with timeout
			var worker *Process
			workerTimer := time.NewTimer(pool.workerTimeout)
			defer workerTimer.Stop()

			select {
			case <-pool.stop:
				// Pool is stopping
				errMsg := fmt.Errorf("pool is stopping")
				if qCmd.streamResponse != nil {
					qCmd.streamResponse <- streamSetup{err: errMsg}
					close(qCmd.streamResponse)
				} else {
					qCmd.response <- commandResponse{err: errMsg}
					close(qCmd.response)
				}

				// Clean up tracking
				pool.mutex.Lock()
				delete(pool.commandToProcess, cmdID)
				pool.mutex.Unlock()
				continue
			case worker = <-pool.availableWorkers:
				// Got a worker
				worker.SetBusy(1)
			case <-workerTimer.C:
				// Timeout waiting for worker - workers are likely stuck
				pool.logger.Error().Msgf("Queue timeout after %v waiting for worker - restarting all workers", pool.workerTimeout)

				// Restart all workers
				pool.restartAllWorkers()

				// Put command back in queue (at front)
				go func(cmd *queuedCommand) {
					select {
					case pool.commandQueue <- cmd:
						// Re-queued successfully
					case <-pool.stop:
						// Pool stopping, abandon
						errMsg := fmt.Errorf("pool stopping during restart")
						if cmd.streamResponse != nil {
							cmd.streamResponse <- streamSetup{err: errMsg}
							close(cmd.streamResponse)
						} else {
							cmd.response <- commandResponse{err: errMsg}
							close(cmd.response)
						}
					}
				}(qCmd)

				// Clean up tracking for now (will be re-added when re-queued)
				pool.mutex.Lock()
				delete(pool.commandToProcess, cmdID)
				pool.mutex.Unlock()
				continue
			}

			// Check again if cancelled after getting worker
			pool.mutex.RLock()
			cancelled = pool.cancelledCommands[cmdID]
			pool.mutex.RUnlock()

			if cancelled {
				// Release worker and send error
				worker.SetBusy(0)

				pool.mutex.Lock()
				delete(pool.cancelledCommands, cmdID)
				delete(pool.commandToProcess, cmdID)
				pool.mutex.Unlock()

				errMsg := fmt.Errorf("command %s was interrupted while queued", cmdID)
				if qCmd.streamResponse != nil {
					qCmd.streamResponse <- streamSetup{err: errMsg}
					close(qCmd.streamResponse)
				} else {
					qCmd.response <- commandResponse{err: errMsg}
					close(qCmd.response)
				}
				continue
			}

			// Update tracking to show which worker is handling this command
			pool.mutex.Lock()
			pool.commandToProcess[cmdID] = worker
			pool.mutex.Unlock()

			// Execute command on worker (in goroutine to not block dispatcher).
			// Streaming and non-streaming paths are structurally similar but
			// differ in (a) which worker method is called and (b) when the
			// pool's commandToProcess tracking is released: for streams the
			// tracking must live until the stream closes, otherwise
			// Interrupt() can't find the worker while frames are still flowing.
			if qCmd.streamResponse != nil {
				go func(w *Process, cmd map[string]interface{}, setupChan chan streamSetup) {
					workerCh, err := w.SendCommandStreaming(cmd)
					if err != nil {
						pool.mutex.Lock()
						delete(pool.commandToProcess, cmdID)
						delete(pool.cancelledCommands, cmdID)
						pool.mutex.Unlock()

						setupChan <- streamSetup{err: err}
						close(setupChan)
						return
					}
					// Tee the worker channel into a user-facing channel so
					// we can detect completion (worker channel close) and
					// clean up pool tracking without stealing frames from
					// the caller.
					userCh := make(chan map[string]interface{}, streamChanCapacity)
					setupChan <- streamSetup{ch: userCh}
					close(setupChan)

					for frame := range workerCh {
						userCh <- frame
					}
					close(userCh)

					pool.mutex.Lock()
					delete(pool.commandToProcess, cmdID)
					delete(pool.cancelledCommands, cmdID)
					pool.mutex.Unlock()
				}(worker, qCmd.cmd, qCmd.streamResponse)
			} else {
				go func(w *Process, cmd map[string]interface{}, respChan chan commandResponse) {
					result, err := w.SendCommand(cmd)

					// Clean up tracking
					pool.mutex.Lock()
					delete(pool.commandToProcess, cmdID)
					delete(pool.cancelledCommands, cmdID)
					pool.mutex.Unlock()

					// Send response
					respChan <- commandResponse{result: result, err: err}
					close(respChan)
				}(worker, qCmd.cmd, qCmd.response)
			}
		}
	}
}

// restartAllWorkers restarts all workers in the pool
func (pool *ProcessPool) restartAllWorkers() {
	pool.logger.Warn().Msg("Restarting all workers due to queue timeout")

	// Restart each worker
	pool.mutex.RLock()
	workers := make([]*Process, len(pool.processes))
	copy(workers, pool.processes)
	pool.mutex.RUnlock()

	for _, process := range workers {
		if process != nil {
			// Track the restart event with the active command if any
			// Need to lock to read activeCommandID safely
			process.commandMutex.Lock()
			activeCmd := process.activeCommandID
			process.commandMutex.Unlock()

			if activeCmd == "" {
				activeCmd = "unknown"
			}
			process.addRestartEvent("queue_timeout", activeCmd)
			process.Restart()
		}
	}

	// Wait for at least one worker to be ready
	if err := pool.WaitForReady(); err != nil {
		pool.logger.Error().Err(err).Msg("Failed to wait for workers after restart")
	}
}

// StopAll stops all the processes in the process pool.
func (pool *ProcessPool) StopAll() {
	pool.SetStop()

	// Close command queue first to prevent new commands
	close(pool.commandQueue)

	// Drain any remaining queued commands with error responses
	for qCmd := range pool.commandQueue {
		qCmd.response <- commandResponse{
			err: fmt.Errorf("pool is shutting down"),
		}
		close(qCmd.response)
	}

	for _, process := range pool.processes {
		process.Stop()
	}
	// Close the channel after stopping all processes
	close(pool.availableWorkers)
	// Drain any remaining entries
	for range pool.availableWorkers {
		// Drain channel
	}
	// Close the stop channel
	close(pool.stop)
}

type ProcessWithPrio struct {
	processId int
	handled   int
}

type ProcessPQ struct {
	processes []*ProcessWithPrio
	mutex     sync.Mutex
	pool      *ProcessPool
}

func (pq *ProcessPQ) Len() int {
	return len(pq.processes)
}

func (pq *ProcessPQ) Less(i, j int) bool {
	return pq.processes[i].handled < pq.processes[j].handled
}

func (pq *ProcessPQ) Swap(i, j int) {
	pq.processes[i], pq.processes[j] = pq.processes[j], pq.processes[i]
}

func (pq *ProcessPQ) Push(x interface{}) {
	item := x.(*ProcessWithPrio)
	pq.processes = append(pq.processes, item)
}

func (pq *ProcessPQ) Pop() interface{} {
	old := pq.processes
	n := len(old)
	item := old[n-1]
	pq.processes = old[0 : n-1]
	return item
}

func (pq *ProcessPQ) Update() {
	pq.mutex.Lock()
	defer pq.mutex.Unlock()

	pq.processes = nil

	pq.pool.mutex.RLock()
	defer pq.pool.mutex.RUnlock()

	for _, process := range pq.pool.processes {
		if process != nil && process.IsReady() && !process.IsBusy() {
			pq.Push(&ProcessWithPrio{
				processId: process.id,
				handled:   process.requestsHandled,
			})
		}
	}

	heap.Init(pq)
}
