// process_pool.go
package subp

import (
	"bufio"
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// responsePool is a pool of pre-allocated response maps to reduce GC pressure
var responsePool = sync.Pool{
	New: func() interface{} {
		return make(map[string]interface{}, 16) // Pre-allocate with reasonable capacity
	},
}

// commandPool is a pool of pre-allocated command maps to reduce GC pressure
var commandPool = sync.Pool{
	New: func() interface{} {
		return make(map[string]interface{}, 8) // Pre-allocate with reasonable capacity
	},
}

// encoderPool is a pool of pre-allocated JSON encoders to reduce GC pressure
var encoderPool = sync.Pool{
	New: func() interface{} {
		// Return a nil encoder - it will be properly initialized when acquired
		return (*json.Encoder)(nil)
	},
}

// uuidPool is a pool of pre-allocated UUIDs to reduce GC pressure during command creation
var uuidPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 36) // UUID string length
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

	// Added fields
	stdinPipe  io.WriteCloser
	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser
}

// ProcessExport exports process information.
type ProcessExport struct {
	IsReady         bool   `json:"IsReady"`
	Latency         int64  `json:"Latency"`
	Name            string `json:"Name"`
	Restarts        int    `json:"Restarts"`
	RequestsHandled int    `json:"RequestsHandled"`
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
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to get stderr pipe for process", p.name)
		return
	}

	p.mutex.Lock()
	p.cmd = cmd
	p.stdinPipe = stdinPipe
	p.stdoutPipe = stdoutPipe
	p.stderrPipe = stderrPipe
	
	// Use encoder from pool if available, or create a new one
	encoder := encoderPool.Get().(*json.Encoder)
	if encoder == nil {
		encoder = json.NewEncoder(stdinPipe)
	} else {
		// Reset the encoder to use our pipe
		*encoder = *json.NewEncoder(stdinPipe)
	}
	p.stdin = encoder
	
	// Use larger buffer sizes for stdout/stderr to handle large responses like base64-encoded images
	p.stdout = bufio.NewReaderSize(stdoutPipe, 4*1024*1024) // 4MB buffer for large responses
	p.stderr = bufio.NewReaderSize(stderrPipe, 64*1024)     // 64KB buffer for errors
	p.mutex.Unlock()

	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		p.readStderr()
	}()
	go func() {
		defer p.wg.Done()
		p.WaitForReadyScan()
	}()

	p.cmd.Dir = p.cwd
	if err := p.cmd.Start(); err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to start process", p.name)
		return
	}
}

// Stop stops the process by sending a kill signal to the process and cleaning up the resources.
func (p *Process) Stop() {
	p.SetReady(0)
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
	
	// Return the encoder to the pool if it exists
	if p.stdin != nil {
		encoderPool.Put(p.stdin)
		p.stdin = nil
	}
	
	p.stdout = nil
	p.stderr = nil
	p.cmd = nil
	p.mutex.Unlock()
}

// Restart stops the process and starts it again.
func (p *Process) Restart() {
	p.logger.Info().Msgf("[nyxsub|%s] Restarting process", p.name)
	atomic.StoreInt32(&p.isReady, 0)
	p.mutex.Lock()
	p.restarts++
	p.mutex.Unlock()
	if atomic.LoadInt32(&p.pool.shouldStop) == 1 {
		p.Stop()
		return
	}
	atomic.StoreInt32(&p.isBusy, 0)
	p.Stop()
	if atomic.LoadInt32(&p.pool.shouldStop) == 0 {
		p.Start()
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
	atomic.StoreInt32(&p.isBusy, busy)
}

// readStderr reads from stderr and logs any output.
func (p *Process) readStderr() {
	for {
		p.mutex.RLock()
		stderr := p.stderr
		p.mutex.RUnlock()
		if stderr == nil {
			return
		}
		line, err := stderr.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stderr", p.name)
			}
			return
		}
		if line != "" && line != "\n" {
			// Trim trailing newlines and whitespace for cleaner output
			trimmedLine := strings.TrimSpace(line)
			
			// Log with more informative prefix indicating subprocess stderr - using Info level
			// since subprocess stderr output is often informational and not an actual error
			p.logger.Info().Msgf("[nyxsub|%s|stderr] Subprocess: %s", p.name, trimmedLine)
		}
	}
}

// WaitForReadyScan waits for the process to send a "ready" message.
func (p *Process) WaitForReadyScan() {
	// Pre-allocate a ready message flag
	const readyMsg = "ready"
	
	// Use a progressively increasing backoff for EOF errors
	backoff := 1 * time.Millisecond
	maxBackoff := 50 * time.Millisecond
	
	// Get a buffer for line reading from the pool
	bufPtr := lineBufferPool.Get().(*[]byte)
	// Clear but keep capacity
	*bufPtr = (*bufPtr)[:0] 
	defer lineBufferPool.Put(bufPtr)
	
	// Setup timeout for initialization
	initTimeout := time.After(p.initTimeout)
	
	// Get the stdout reader once outside the loop
	p.mutex.RLock()
	stdout := p.stdout
	p.mutex.RUnlock()
	
	if stdout == nil {
		return
	}
	
	for {
		select {
		case <-initTimeout:
			// Timeout waiting for ready message
			p.logger.Error().Msgf("[nyxsub|%s] Timeout waiting for ready message", p.name)
			p.Restart()
			return
		default:
			// Continue with normal processing
		}
		
		// Very short, exclusive lock only for the actual read operation
		p.mutex.Lock()
		if p.stdout == nil { // Double-check after exclusive lock
			p.mutex.Unlock()
			return
		}
		line, err := p.stdout.ReadString('\n')
		p.mutex.Unlock()
		
		if err != nil {
			if err != io.EOF {
				p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stdout", p.name)
				p.Restart()
				return
			}
			// Use exponential backoff with jitter for EOF errors
			jitter := time.Duration(rand.Int63n(int64(backoff) / 5))
			sleepTime := backoff + jitter
			time.Sleep(sleepTime)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		
		// Reset backoff on successful reads
		backoff = 1 * time.Millisecond
		
		if line == "" || line == "\n" {
			continue
		}
		
		// Get a pre-allocated response map from the pool
		responseMap := responsePool.Get().(map[string]interface{})
		// Clear the map for reuse using our optimized function
		fastClearMap(responseMap)
		
		if err := json.Unmarshal([]byte(line), &responseMap); err != nil {
			// Return the map to the pool if unmarshaling fails
			responsePool.Put(responseMap)
			
			// Format non-JSON messages more clearly
			trimmedLine := strings.TrimSpace(line)
			p.logger.Info().Msgf("[nyxsub|%s|stdout] Non-JSON output: %s", p.name, trimmedLine)
			continue
		}
		
		if typeVal, ok := responseMap["type"]; ok && typeVal == readyMsg {
			p.logger.Info().Msgf("[nyxsub|%s] Process is ready", p.name)
			p.SetReady(1)
			// Return the map to the pool before returning
			responsePool.Put(responseMap)
			return
		}
		
		// Return the map to the pool if it's not a ready message
		responsePool.Put(responseMap)
	}
}

// SendCommand sends a command to the process and waits for the response.
func (p *Process) SendCommand(cmd map[string]interface{}) (map[string]interface{}, error) {
	atomic.StoreInt32(&p.isBusy, 1)
	defer atomic.StoreInt32(&p.isBusy, 0)

	// Calculate start time once at the beginning to measure accurate latency
	start := time.Now().UnixMilli()
	
	var cmdID string
	if id, ok := cmd["id"]; !ok {
		// Generate UUID with less allocations by using the UUID pool
		uuidBuf := uuidPool.Get().([]byte)
		// Write the UUID string directly into the pre-allocated buffer
		uuidStr := uuid.New().String()
		copy(uuidBuf, uuidStr)
		cmdID = string(uuidBuf[:36])
		cmd["id"] = cmdID
		// Return the buffer to the pool
		uuidPool.Put(uuidBuf)
	} else {
		cmdID = id.(string)
	}
	if _, ok := cmd["type"]; !ok {
		cmd["type"] = "main"
	}

	p.mutex.Lock()
	if p.stdin == nil {
		p.mutex.Unlock()
		p.logger.Error().Msgf("[nyxsub|%s] stdin is nil", p.name)
		p.Restart()
		return nil, errors.New("stdin is nil")
	}
	err := p.stdin.Encode(cmd)
	p.mutex.Unlock()
	if err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to send command", p.name)
		p.Restart()
		return nil, err
	}

	if p.logger.Debug().Enabled() {
		cmdb, _ := json.Marshal(cmd)
		p.logger.Debug().Msgf("[nyxsub|%s] Command sent: %s", p.name, string(cmdb))
	}

	response, err := p.readResponse(cmdID)
	if err != nil {
		p.Restart()
		return nil, err
	}

	latency := time.Now().UnixMilli() - start
	p.mutex.Lock()
	p.latency = latency
	p.requestsHandled++
	p.mutex.Unlock()

	return response, nil
}

// readResponse reads the response for a specific command ID.
// fastClearMap quickly clears a map without deallocating its memory
func fastClearMap(m map[string]interface{}) {
	for k := range m {
		delete(m, k)
	}
}

// lineBufferPool is a pool of pre-allocated byte slices for reading lines
var lineBufferPool = sync.Pool{
	New: func() interface{} {
		// Pre-allocate a 4KB buffer, which is enough for most JSON responses
		buffer := make([]byte, 0, 4096)
		return &buffer
	},
}

func (p *Process) readResponse(cmdID string) (map[string]interface{}, error) {
	timeout := time.After(p.timeout)
	
	// Get a buffer for line reading from the pool
	bufPtr := lineBufferPool.Get().(*[]byte)
	// Clear but keep capacity
	*bufPtr = (*bufPtr)[:0] 
	defer lineBufferPool.Put(bufPtr)
	
	// Pre-allocate the result map once outside the loop to avoid repeated allocations
	result := make(map[string]interface{}, 16)
	
	// Get the stdout reader once outside the loop
	p.mutex.RLock()
	stdout := p.stdout
	p.mutex.RUnlock()

	if stdout == nil {
		return nil, errors.New("stdout is nil")
	}

	for {
		select {
		case <-timeout:
			p.logger.Error().Msgf("[nyxsub|%s] Communication timed out", p.name)
			return nil, errors.New("communication timed out")
		default:
			// Very short, exclusive lock only for the actual read operation
			p.mutex.Lock()
			if p.stdout == nil {
				p.mutex.Unlock()
				return nil, errors.New("stdout is nil")
			}
			line, err := p.stdout.ReadString('\n')
			p.mutex.Unlock()
			
			if err != nil {
				p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stdout", p.name)
				return nil, err
			}
			
			if line == "" || line == "\n" {
				time.Sleep(1 * time.Millisecond)
				continue
			}
			
			// Get a pre-allocated response map from the pool
			responseMap := responsePool.Get().(map[string]interface{})
			// Clear the map for reuse using our optimized function
			fastClearMap(responseMap)
			
			if err := json.Unmarshal([]byte(line), &responseMap); err != nil {
				// Return the map to the pool if unmarshaling fails
				responsePool.Put(responseMap)
				
				// Format non-JSON messages more clearly - using Info level since
				// non-JSON output from subprocess is often informational, not a warning
				trimmedLine := strings.TrimSpace(line)
				p.logger.Info().Msgf("[nyxsub|%s|stdout] Non-JSON output: %s", p.name, trimmedLine)
				continue
			}

			if id, ok := responseMap["id"]; ok && id == cmdID {
				// Copy to our pre-allocated result map - this avoids allocating a new map for each match
				fastClearMap(result) // Ensure it's empty before copying
				for k, v := range responseMap {
					result[k] = v
				}
				
				// Return the original map to the pool
				responsePool.Put(responseMap)
				return result, nil
			}
			
			// Return the map to the pool if it doesn't match our cmdID
			responsePool.Put(responseMap)
		}
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
	pool := &ProcessPool{
		processes:     make([]*Process, size),
		logger:        logger,
		mutex:         sync.RWMutex{},
		shouldStop:    shouldStop,
		stop:          make(chan bool, 1),
		workerTimeout: workerTimeout,
		comTimeout:    comTimeout,
		initTimeout:   initTimeout,
	}
	pool.queue = ProcessPQ{
		processes: make([]*ProcessWithPrio, 0, size),
		mutex:     sync.RWMutex{},
		pool:      pool,
	}
	for i := 0; i < size; i++ {
		pool.newProcess(name, i, cmd, cmdArgs, logger, cwd)
	}
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
	}
	pool.mutex.Unlock()
	pool.processes[i].Start()
}

// ExportAll exports all the processes in the process pool as a slice of ProcessExport.
func (pool *ProcessPool) ExportAll() []ProcessExport {
	pool.mutex.RLock()
	// Pre-allocate slice with exact capacity needed
	processCount := 0
	for _, process := range pool.processes {
		if process != nil {
			processCount++
		}
	}
	exports := make([]ProcessExport, 0, processCount)
	
	for _, process := range pool.processes {
		if process != nil {
			process.mutex.RLock()
			exports = append(exports, ProcessExport{
				IsReady:         atomic.LoadInt32(&process.isReady) == 1,
				Latency:         process.latency,
				Name:            process.name,
				Restarts:        process.restarts,
				RequestsHandled: process.requestsHandled,
			})
			process.mutex.RUnlock()
		}
	}
	pool.mutex.RUnlock()
	return exports
}

// processWithPrioPool is a pool of pre-allocated ProcessWithPrio objects
var processWithPrioPool = sync.Pool{
	New: func() interface{} {
		return &ProcessWithPrio{}
	},
}

// GetWorker returns a worker process from the process pool.
func (pool *ProcessPool) GetWorker() (*Process, error) {
	if atomic.LoadInt32(&pool.shouldStop) == 1 {
		return nil, fmt.Errorf("process pool is stopping")
	}

	// Use adaptive polling strategy - start with shorter intervals, then increase
	initialInterval := 1 * time.Millisecond
	maxInterval := 20 * time.Millisecond
	currentInterval := initialInterval
	
	// Create a ticker with the initial interval
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()
	
	// Create a timeout channel
	timeout := time.After(pool.workerTimeout)
	
	// Create a variable to track consecutive attempts with no ready workers
	noReadyCount := 0
	
	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("timeout exceeded, no available workers")
		case <-ticker.C:
			// Quickly check if any workers are ready before taking locks
			anyReady := false
			pool.mutex.RLock()
			for _, process := range pool.processes {
				if process != nil && atomic.LoadInt32(&process.isReady) == 1 && atomic.LoadInt32(&process.isBusy) == 0 {
					anyReady = true
					break
				}
			}
			pool.mutex.RUnlock()
			
			if !anyReady {
				// Adjust polling interval based on consecutive failures
				noReadyCount++
				if noReadyCount > 5 && currentInterval < maxInterval {
					// Increase interval exponentially
					currentInterval *= 2
					if currentInterval > maxInterval {
						currentInterval = maxInterval
					}
					// Reset the ticker with the new interval
					ticker.Reset(currentInterval)
				}
				continue
			}
			
			// We found a ready worker, reset the count and interval
			noReadyCount = 0
			if currentInterval != initialInterval {
				currentInterval = initialInterval
				ticker.Reset(currentInterval)
			}
			
			// Now update the queue and get the next available worker
			pool.queue.mutex.Lock()
			pool.queue.Update()
			if pool.queue.Len() > 0 {
				processWithPrio := heap.Pop(&pool.queue).(*ProcessWithPrio)
				pid := processWithPrio.processId
				
				// Return the ProcessWithPrio object to the pool
				processWithPrioPool.Put(processWithPrio)
				
				pool.queue.mutex.Unlock()
				
				// Try to get and mark the worker as busy
				pool.mutex.RLock()
				process := pool.processes[pid]
				pool.mutex.RUnlock()
				
				if process != nil && atomic.LoadInt32(&process.isReady) == 1 && atomic.CompareAndSwapInt32(&process.isBusy, 0, 1) {
					return process, nil
				}
			} else {
				pool.queue.mutex.Unlock()
			}
		}
	}
}

// WaitForReady waits until at least one worker is ready or times out.
func (pool *ProcessPool) WaitForReady() error {
	timeout := time.After(pool.initTimeout)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for workers to be ready")
		case <-ticker.C:
			ready := false
			pool.mutex.RLock()
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
		}
	}
}

// SendCommand sends a command to a worker in the process pool.
func (pool *ProcessPool) SendCommand(cmd map[string]interface{}) (map[string]interface{}, error) {
	// Get a worker from the pool
	worker, err := pool.GetWorker()
	if err != nil {
		return nil, err
	}
	
	// Create a copy of the command if the original should be preserved
	// This is needed because the worker will modify the map (adding ID if not present)
	cmdCopy := commandPool.Get().(map[string]interface{})
	// Clear the map for reuse
	fastClearMap(cmdCopy)
	
	// Copy the command data
	for k, v := range cmd {
		cmdCopy[k] = v
	}
	
	// Send the command copy to the worker
	resp, err := worker.SendCommand(cmdCopy)
	
	// Return the command map to the pool
	commandPool.Put(cmdCopy)
	
	return resp, err
}

// StopAll stops all the processes in the process pool.
func (pool *ProcessPool) StopAll() {
	pool.SetStop()
	for _, process := range pool.processes {
		if process != nil {
			process.Stop()
		}
	}
}

type ProcessWithPrio struct {
	processId int
	handled   int
}

type ProcessPQ struct {
	processes []*ProcessWithPrio
	mutex     sync.RWMutex
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
	// Note: we don't return the item to the pool here because GetWorker
	// needs to use its values before returning it to the pool
	return item
}

func (pq *ProcessPQ) Update() {
	// Reuse the existing slice but clear it
	if cap(pq.processes) > 0 {
		pq.processes = pq.processes[:0]
	} else {
		// Allocate with capacity if it's the first run
		pq.processes = make([]*ProcessWithPrio, 0, len(pq.pool.processes))
	}
	
	pq.pool.mutex.RLock()
	var readyCount int
	// First pass: count ready processes to optimize allocation
	for _, process := range pq.pool.processes {
		if process != nil && atomic.LoadInt32(&process.isReady) == 1 && atomic.LoadInt32(&process.isBusy) == 0 {
			readyCount++
		}
	}
	
	// Ensure capacity
	if cap(pq.processes) < readyCount {
		// Create a new slice with larger capacity
		newProcesses := make([]*ProcessWithPrio, 0, readyCount)
		pq.processes = newProcesses
	}
	
	// Second pass: collect the ready processes
	for _, process := range pq.pool.processes {
		if process != nil && atomic.LoadInt32(&process.isReady) == 1 && atomic.LoadInt32(&process.isBusy) == 0 {
			// Get a ProcessWithPrio from the pool
			pwp := processWithPrioPool.Get().(*ProcessWithPrio)
			
			process.mutex.RLock()
			pwp.processId = process.id
			pwp.handled = process.requestsHandled
			process.mutex.RUnlock()
			
			pq.processes = append(pq.processes, pwp)
		}
	}
	pq.pool.mutex.RUnlock()
	
	// Only initialize the heap if we found ready processes
	if len(pq.processes) > 0 {
		heap.Init(pq)
	}
}