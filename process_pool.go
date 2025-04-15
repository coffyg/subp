package subp

import (
	"bufio"
	"bytes"
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

// Error wrapping for consistent error messages
type SubpError struct {
	Op  string // Operation that failed
	Err error  // Original error
}

func (e *SubpError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("subp.%s: operation failed", e.Op)
	}
	return fmt.Sprintf("subp.%s: %v", e.Op, e.Err)
}

func (e *SubpError) Unwrap() error {
	return e.Err
}

// wrapError wraps an error with operation context without allocating if error is nil
func wrapError(op string, err error) error {
	if err == nil {
		return nil
	}
	return &SubpError{Op: op, Err: err}
}

// reusableBuffer is used to avoid allocations during JSON parsing
var jsonParserPool = sync.Pool{
	New: func() interface{} {
		// Use larger initial capacity to efficiently handle media payloads
		return make(map[string]interface{}, 64) // Increased from 8 to 64 for media payloads
	},
}

// Process is a process that can be started, stopped, and restarted.
type Process struct {
	cmd             *exec.Cmd
	isReady         int32
	isBusy          int32
	latency         int64
	mutex           sync.RWMutex
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

	stdinPipe  io.WriteCloser
	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser

	// Channels and maps for concurrency
	responseMap     sync.Map
	readyChan       chan struct{}
	readyOnce       sync.Once
	responseCache   map[string]chan map[string]interface{} // Optional cache for hot responses
	
	// Buffer for JSON marshaling - avoid memory allocations
	commandBuffer   []byte
	responseBuffer  []byte
}

// ProcessExport exports process information.
type ProcessExport struct {
	IsReady         bool   `json:"IsReady"`
	Latency         int64  `json:"Latency"`
	Name            string `json:"Name"`
	Restarts        int    `json:"Restarts"`
	RequestsHandled int    `json:"RequestsHandled"`
}

// Start starts the process.
func (p *Process) Start() {
	p.SetReady(0)

	cmd := exec.Command(p.cmdStr, p.cmdArgs...)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		p.logger.Error().Err(wrapError("Start", err)).Msgf("process=%s failed_to=get_stdin_pipe", p.name)
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		p.logger.Error().Err(wrapError("Start", err)).Msgf("process=%s failed_to=get_stdout_pipe", p.name)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		p.logger.Error().Err(wrapError("Start", err)).Msgf("process=%s failed_to=get_stderr_pipe", p.name)
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
	p.readyChan = make(chan struct{})
	p.mutex.Unlock()

	p.wg.Add(3)

	go func() {
		defer p.wg.Done()
		p.readStderr()
	}()

	go func() {
		defer p.wg.Done()
		p.readStdout()
	}()

	go func() {
		defer p.wg.Done()
		p.WaitForReadyScan()
	}()

	p.cmd.Dir = p.cwd
	if err := p.cmd.Start(); err != nil {
		p.logger.Error().Err(wrapError("Start", err)).Msgf("process=%s failed_to=start", p.name)
		return
	}
}

// Stop stops the process.
func (p *Process) Stop() {
	p.SetReady(0)
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	p.wg.Wait()
	p.cleanupChannelsAndResources()
	p.logger.Info().Msgf("process=%s status=stopped", p.name)
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
}

// Protects process restart to avoid concurrent restarts
var processRestartMutex = sync.Map{}

// Restart stops the process and starts it again.
func (p *Process) Restart() {
	// CRITICAL FIX: Use a lock to prevent concurrent restarts of the same process
	// This avoids the WaitGroup panic when multiple goroutines try to restart
	lockKey := fmt.Sprintf("restart-%s-%d", p.name, p.id)
	actualLock, _ := processRestartMutex.LoadOrStore(lockKey, &sync.Mutex{})
	lock := actualLock.(*sync.Mutex)
	
	// Try to get the lock - if we can't, it means another goroutine is already
	// restarting this process, so we just return
	if !tryLock(lock) {
		// Skip restart - already in progress
		return
	}
	defer lock.Unlock()
	
	p.logger.Info().Msgf("process=%s status=restarting", p.name)
	
	// Fast path - increment restart counter with lock
	p.mutex.Lock()
	p.restarts++
	p.mutex.Unlock()
	
	// BUGFIX: Set ready to false so no new commands are sent
	p.SetReady(0)
	
	// Kill the process directly instead of using Stop() which uses waitgroup
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	
	// Close resources directly - avoid waitgroup
	p.cleanupChannelsAndResources()
	
	// Only start if we're not shutting down the pool - check with atomic
	if atomic.LoadInt32(&p.pool.shouldStop) == 0 {
		p.Start()
	}
}

// tryLock attempts to lock without blocking
func tryLock(m *sync.Mutex) bool {
	// Use a channel with timeout to avoid blocking
	ch := make(chan bool, 1)
	go func() {
		m.Lock()
		ch <- true
	}()
	
	select {
	case <-ch:
		return true
	case <-time.After(10 * time.Millisecond):
		return false
	}
}

// SetReady sets the readiness of the process.
func (p *Process) SetReady(ready int32) {
	atomic.StoreInt32(&p.isReady, ready)
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
	prevBusy := atomic.LoadInt32(&p.isBusy)
	atomic.StoreInt32(&p.isBusy, busy)
	
	// If worker is going from busy to not-busy and it's ready,
	// notify all waiters that a worker is available
	if prevBusy == 1 && busy == 0 && p.IsReady() {
		p.pool.mutex.RLock()
		if p.pool.waiters != nil {
			for _, ch := range p.pool.waiters {
				select {
				case ch <- struct{}{}:
					// Notification sent
				default:
					// Channel full or closed, continue to next
				}
			}
		}
		p.pool.mutex.RUnlock()
	}
}

// readStderr reads from stderr and logs any output.
func (p *Process) readStderr() {
	for {
		line, err := p.stderr.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				p.logger.Error().Err(wrapError("ReadStderr", err)).Msgf("process=%s failed_to=read_stderr", p.name)
			}
			return
		}
		if line != "" && line != "\n" {
			p.logger.Error().Msgf("process=%s stderr_output=%q", p.name, line)
		}
	}
}

// readStdout continuously reads lines from stdout.
func (p *Process) readStdout() {
	// Set up an optimized scanner with a much larger buffer for media payloads
	scanner := bufio.NewScanner(p.stdout)
	
	// Set a very large buffer to handle large JSON payloads with base64 media
	const maxScanTokenSize = 30 * 1024 * 1024 // 30MB buffer for large base64 encoded videos
	buffer := make([]byte, maxScanTokenSize)
	scanner.Buffer(buffer, maxScanTokenSize)
	
	// Reuse this buffer for all non-response lines
	readyBytes := []byte(`{"type":"ready"}`)
	
	// Use a line buffer to avoid allocations
	var lineBytes []byte
	
	for scanner.Scan() {
		// Get the bytes directly to avoid string allocation
		lineBytes = scanner.Bytes()
		if len(lineBytes) == 0 {
			continue
		}
		
		// Fast path for ready message - direct byte comparison
		if len(lineBytes) == len(readyBytes) && bytes.Equal(lineBytes, readyBytes) {
			p.readyOnce.Do(func() {
				p.logger.Info().Msgf("process=%s status=ready", p.name)
				p.SetReady(1)
				close(p.readyChan)
			})
			continue
		}
		
		// Get a response object from the pool
		respObj := jsonParserPool.Get().(map[string]interface{})
		// Clear the map for reuse
		for k := range respObj {
			delete(respObj, k)
		}
		
		// Parse JSON
		if err := json.Unmarshal(lineBytes, &respObj); err != nil {
			p.logger.Warn().Msgf("process=%s warn=invalid_json_message data=%q", p.name, lineBytes)
			jsonParserPool.Put(respObj) // Return to pool
			continue
		}
		
		// Check for ready message
		if t, ok := respObj["type"].(string); ok && t == "ready" {
			p.readyOnce.Do(func() {
				p.logger.Info().Msgf("process=%s status=ready", p.name)
				p.SetReady(1)
				close(p.readyChan)
			})
			jsonParserPool.Put(respObj) // Return to pool
			continue
		}
		
		// Process command responses
		if idVal, ok := respObj["id"].(string); ok {
			if ch, ok := p.responseMap.Load(idVal); ok {
				castCh := ch.(chan map[string]interface{})
				
				// Create a copy of the map for the response
				// Because the original will be reused by the pool
				// Use larger capacity for potential media payloads
				initialCapacity := 32 // Higher capacity for base64 encoded media
				if len(respObj) > initialCapacity {
					initialCapacity = len(respObj)
				}
				responseCopy := make(map[string]interface{}, initialCapacity)
				for k, v := range respObj {
					responseCopy[k] = v
				}
				
				// Non-blocking send with default case to avoid deadlocks
				select {
				case castCh <- responseCopy:
					// Response sent successfully
				default:
					// Channel is full or closed, which means the requester timed out
				}
			}
		}
		
		// Return object to pool for reuse
		jsonParserPool.Put(respObj)
	}
	
	// Check for scanner errors
	if err := scanner.Err(); err != nil {
		if err != io.EOF {
			p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stdout", p.name)
		}
	}
	
	// When the scanner exits (e.g., due to pipe closure), restart the process
	go func() {
		// Use a timer instead of sleep to be more efficient
		timer := time.NewTimer(10 * time.Millisecond)
		<-timer.C
		p.Restart()
	}()
}

// WaitForReadyScan waits for the process to send a "ready" message.
func (p *Process) WaitForReadyScan() {
	timer := time.NewTimer(p.initTimeout)
	defer timer.Stop()

	select {
	case <-p.readyChan:
		return
	case <-timer.C:
		p.logger.Error().Msgf("process=%s error=init_timeout status=not_ready", p.name)
		// Call restart in a goroutine to avoid deadlock
		go func() {
			time.Sleep(10 * time.Millisecond)
			p.Restart()
		}()
		return
	}
}

// uuidPool provides a pool of pre-created UUID strings to reduce allocation
var uuidPool = sync.Pool{
	New: func() interface{} {
		return uuid.New().String()
	},
}

// timerPool provides a pool of reusable timers
var timerPool = sync.Pool{
	New: func() interface{} {
		return time.NewTimer(time.Second)
	},
}

// responseChannelPool provides a pool of pre-allocated response channels
var responseChannelPool = sync.Pool{
	New: func() interface{} {
		return make(chan map[string]interface{}, 1)
	},
}

// resetTimer resets a timer from the pool for the given duration
func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

// SendCommand sends a command to the process and waits for the response.
func (p *Process) SendCommand(cmd map[string]interface{}) (map[string]interface{}, error) {
	p.SetBusy(1)
	defer p.SetBusy(0)

	// Initialize command metadata using pre-determined values where possible
	var cmdID string
	if id, ok := cmd["id"]; !ok {
		// Get a UUID from the pool instead of generating a new one
		cmdID = uuidPool.Get().(string)
		cmd["id"] = cmdID
		// Generate a new UUID for the pool for next use
		go func() {
			uuidPool.Put(uuid.New().String())
		}()
	} else {
		cmdID = id.(string)
	}
	
	if _, ok := cmd["type"]; !ok {
		cmd["type"] = "main"
	}

	start := time.Now().UnixMilli()

	// Get a pre-allocated response channel from the pool
	responseCh := responseChannelPool.Get().(chan map[string]interface{})
	// Clear any potential leftover value from the channel
	select {
	case <-responseCh:
	default:
	}
	
	p.responseMap.Store(cmdID, responseCh)
	defer func() {
		p.responseMap.Delete(cmdID)
		// Return channel to pool
		responseChannelPool.Put(responseCh)
	}()
	
	// Send command to the process
	if err := p.stdin.Encode(cmd); err != nil {
		p.logger.Error().Err(wrapError("SendCommand", err)).Msgf("process=%s failed_to=send_command", p.name)
		// Only restart for actual IO errors, not timeouts
		if !p.IsReady() {
			// Only restart if the process is not ready
			go p.Restart()
		}
		return nil, err
	}

	// Only log in debug mode to avoid string formatting overhead
	if p.logger.GetLevel() <= zerolog.DebugLevel {
		// Use pre-allocated buffer if available
		if cap(p.commandBuffer) > 0 {
			p.commandBuffer = p.commandBuffer[:0] // Reset but preserve capacity
			buf, err := json.Marshal(cmd)
			if err == nil {
				p.commandBuffer = append(p.commandBuffer, buf...)
				p.logger.Debug().Msgf("process=%s action=command_sent command=%s", p.name, p.commandBuffer)
			}
		} else {
			jsonCmd, _ := json.Marshal(cmd)
			p.logger.Debug().Msgf("process=%s action=command_sent command=%s", p.name, jsonCmd)
		}
	}

	// CRITICAL FIX: For commands that may take a while (like SSR),
	// communication timeout must be generous enough to accommodate the operation
	// Minimum 5 seconds or 5x the configured timeout, whichever is greater
	effectiveTimeout := p.timeout
	if effectiveTimeout < 5*time.Second {
		effectiveTimeout = 5 * time.Second
	}
	
	// Wait for response with timeout - use a reusable timer
	timer := timerPool.Get().(*time.Timer)
	resetTimer(timer, effectiveTimeout)
	defer timerPool.Put(timer)
	
	var response map[string]interface{}
	var err error
	select {
	case response = <-responseCh:
		// Success, got response
		err = nil
	case <-timer.C:
		p.logger.Error().Msgf("process=%s error=timeout action=communication timeout=%v", p.name, effectiveTimeout)
		err = &SubpError{Op: "SendCommand", Err: errors.New("communication timeout")}
		
		// Don't restart process for timeouts - this is causing cascading failures
		// Only restart if there was an actual communication error
		// With many users and 150ms renders, timeouts may happen normally
		
		return nil, err
	}

	// Update metrics - minimal lock time
	latency := time.Now().UnixMilli() - start
	p.mutex.Lock()
	p.latency = latency
	p.requestsHandled++
	p.mutex.Unlock()

	return response, nil
}

// readResponse is kept for backward compatibility
// This is now integrated directly into SendCommand for reduced overhead
func (p *Process) readResponse(cmdID string) (map[string]interface{}, error) {
	responseCh := make(chan map[string]interface{}, 1)
	p.responseMap.Store(cmdID, responseCh)
	defer p.responseMap.Delete(cmdID)

	// Apply same timeout logic as SendCommand
	effectiveTimeout := p.timeout
	if effectiveTimeout < 5*time.Second {
		effectiveTimeout = 5 * time.Second
	}

	select {
	case resp := <-responseCh:
		return resp, nil
	case <-time.After(effectiveTimeout):
		p.logger.Error().Msgf("process=%s error=timeout action=communication timeout=%v", p.name, effectiveTimeout)
		return nil, errors.New("communication timed out")
	}
}

// ProcessPool is a pool of processes.
type ProcessPool struct {
	processes     []*Process
	mutex         sync.RWMutex
	logger        *zerolog.Logger
	queue         ProcessPQ
	shouldStop    int32
	stop          chan bool
	workerTimeout time.Duration
	comTimeout    time.Duration
	initTimeout   time.Duration
	waiters       []chan struct{}  // Channels to notify when workers become available
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
	// Pre-allocate all resources to avoid dynamic allocations during operation
	processes := make([]*Process, size)
	stopChan := make(chan bool, 1)
	
	// Create the pool with pre-configured settings
	pool := &ProcessPool{
		processes:     processes,
		logger:        logger,
		mutex:         sync.RWMutex{},
		shouldStop:    0, // Use atomic operations on this
		stop:          stopChan,
		workerTimeout: workerTimeout,
		comTimeout:    comTimeout,
		initTimeout:   initTimeout,
	}
	
	// Initialize the priority queue with optimal capacity
	pool.queue = ProcessPQ{
		processes: make([]*ProcessWithPrio, 0, size), // Pre-allocate capacity based on pool size
		mutex:     sync.Mutex{},
		pool:      pool,
	}
	
	// Create and start all worker processes
	// Use a wait group to track initialization progress
	var wg sync.WaitGroup
	wg.Add(size)
	
	for i := 0; i < size; i++ {
		// Create processes in parallel for faster startup
		go func(idx int) {
			defer wg.Done()
			pool.newProcess(name, idx, cmd, cmdArgs, logger, cwd)
		}(i)
	}
	
	// Wait for all processes to be created (not necessarily ready)
	wg.Wait()
	
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
	// Create a new process with optimized initialization
	process := &Process{
		isReady:         0,
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
		// Initialize maps and response channels
		responseCache:   make(map[string]chan map[string]interface{}, 16), // Pre-allocate space for common responses
		// Pre-allocate buffers for parsing to avoid GC pressure
		commandBuffer:   make([]byte, 0, 4*1024),          // 4KB for commands
		responseBuffer:  make([]byte, 0, 30*1024*1024),  // 30MB for responses with video content
	}
	
	// Add the process to the pool under lock
	pool.mutex.Lock()
	pool.processes[i] = process
	pool.mutex.Unlock()
	
	// Start the process (this will initialize pipes and start goroutines)
	process.Start()
}

// ExportAll exports all the processes in the process pool as a slice of ProcessExport.
func (pool *ProcessPool) ExportAll() []ProcessExport {
	pool.mutex.RLock()
	var exports []ProcessExport
	for _, process := range pool.processes {
		if process != nil {
			process.mutex.Lock()
			exports = append(exports, ProcessExport{
				IsReady:         atomic.LoadInt32(&process.isReady) == 1,
				Latency:         process.latency,
				Name:            process.name,
				Restarts:        process.restarts,
				RequestsHandled: process.requestsHandled,
			})
			process.mutex.Unlock()
		}
	}
	pool.mutex.RUnlock()
	return exports
}

// GetWorker returns a worker process from the process pool.
func (pool *ProcessPool) GetWorker() (*Process, error) {
	// Fast path - try to get a worker immediately
	process := pool.tryGetWorkerFast()
	if process != nil {
		return process, nil
	}
	
	// Slow path - wait for a worker with timeout
	return pool.waitForWorker()
}

// tryGetWorkerFast attempts to immediately get an available worker
// This method is optimized for the happy path when workers are readily available
func (pool *ProcessPool) tryGetWorkerFast() *Process {
	pool.queue.mutex.Lock()
	defer pool.queue.mutex.Unlock()
	
	pool.queue.Update()
	if pool.queue.Len() > 0 {
		processWithPrio := heap.Pop(&pool.queue).(*ProcessWithPrio)
		processId := processWithPrio.processId
		
		// Use a quick bounds check without requiring another lock
		if processId >= 0 && processId < len(pool.processes) {
			pool.mutex.RLock()
			process := pool.processes[processId]
			pool.mutex.RUnlock()
			
			if process != nil {
				process.SetBusy(1)
				return process
			}
		}
	}
	
	return nil
}

// waitForWorkerDoneChan is a pool of done channels to reduce allocation
var waitForWorkerDoneChan = sync.Pool{
	New: func() interface{} {
		return make(chan struct{})
	},
}

// waitForWorkerChan is a pool of worker channels to reduce allocation
var waitForWorkerChan = sync.Pool{
	New: func() interface{} {
		return make(chan *Process, 1)
	},
}

// waitForWorker waits for a worker to become available with a timeout
func (pool *ProcessPool) waitForWorker() (*Process, error) {
    // Fast path - try to get a worker immediately
    process := pool.tryGetWorkerFast()
    if process != nil {
        return process, nil
    }

    // Create timer for the full timeout period that the developer specified
    timer := timerPool.Get().(*time.Timer)
    resetTimer(timer, pool.workerTimeout)
    defer timerPool.Put(timer)
    
    // Wait loop - keep checking for workers until the timeout expires
    // This is similar to v0.0.4 but with a fixed polling interval
    startTime := time.Now()
    remainingTime := pool.workerTimeout
    
    for remainingTime > 0 {
        // Sleep for a short interval to avoid tight looping
        time.Sleep(20 * time.Millisecond)
        
        // Always update the queue before checking for workers
        pool.queue.mutex.Lock()
        pool.queue.Update()
        pool.queue.mutex.Unlock()
        
        // Try to get a worker
        process = pool.tryGetWorkerFast()
        if process != nil {
            return process, nil
        }
        
        // Update remaining time
        elapsed := time.Since(startTime)
        remainingTime = pool.workerTimeout - elapsed
    }
    
    // Log worker status for debugging
    pool.mutex.RLock()
    totalWorkers := len(pool.processes)
    readyWorkers := 0
    busyWorkers := 0
    for _, proc := range pool.processes {
        if proc != nil && proc.IsReady() {
            readyWorkers++
            if proc.IsBusy() {
                busyWorkers++
            }
        }
    }
    pool.mutex.RUnlock()
    
    pool.logger.Warn().Msgf("Timeout waiting for worker after %v: %d/%d ready, %d busy", 
        pool.workerTimeout, readyWorkers, totalWorkers, busyWorkers)
        
    return nil, fmt.Errorf("timeout exceeded, no available workers")
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
	worker, err := pool.GetWorker()
	if err != nil {
		return nil, err
	}
	return worker.SendCommand(cmd)
}

// StopAll stops all the processes in the process pool.
func (pool *ProcessPool) StopAll() {
	pool.SetStop()
	for _, process := range pool.processes {
		process.Stop()
	}
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
	// NOTE: This function assumes the mutex is ALREADY locked by the caller
	
	// Pre-allocate the slice to avoid dynamic allocations
	if cap(pq.processes) == 0 {
		// Initial allocation based on pool size
		pq.processes = make([]*ProcessWithPrio, 0, len(pq.pool.processes))
	} else {
		// Reuse the existing memory but set length to 0
		pq.processes = pq.processes[:0]
	}
	
	pq.pool.mutex.RLock()
	
	// Use a fast path optimization for finding available workers
	for _, process := range pq.pool.processes {
		if process != nil && 
		   atomic.LoadInt32(&process.isReady) == 1 && 
		   atomic.LoadInt32(&process.isBusy) == 0 {
			pq.Push(&ProcessWithPrio{
				processId: process.id,
				handled:   process.requestsHandled,
			})
		}
	}
	pq.pool.mutex.RUnlock()
	
	// Only initialize the heap if we need to
	if len(pq.processes) > 1 {
		heap.Init(pq)
	}
}
