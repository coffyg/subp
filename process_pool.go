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
	p.stdin = json.NewEncoder(stdinPipe)
	p.stdout = bufio.NewReader(stdoutPipe)
	p.stderr = bufio.NewReader(stderrPipe)
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
	p.stdin = nil
	p.stdout = nil
	p.stderr = nil
	p.cmd = nil
	p.mutex.Unlock()
}

// Restart stops the process and starts it again.
func (p *Process) Restart() {
	p.logger.Info().Msgf("[nyxsub|%s] Restarting process", p.name)
	
	// Increment restart counter
	p.mutex.Lock()
	p.restarts++
	p.mutex.Unlock()
	
	// Stop the current process
	p.Stop()
	
	// Only start if the pool is not being shut down
	if atomic.LoadInt32(&p.pool.shouldStop) == 0 {
		// Reset state before starting
		p.mutex.Lock()
		p.isBusy = 0                // Reset busy state
		// Do not reset requestsHandled to maintain fair load balancing
		p.mutex.Unlock()
		
		// Start the process
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
		
		// Check if stderr is nil (could happen during restart)
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
			p.logger.Error().Msgf("[nyxsub|%s|stderr] %s", p.name, line)
		}
	}
}

// WaitForReadyScan waits for the process to send a "ready" message.
func (p *Process) WaitForReadyScan() {
	// Pre-allocate response object to reduce GC pressure
	responseBuf := make(map[string]interface{}, 2)
	
	// Create a timeout to avoid potential deadlocks
	timeoutTimer := time.NewTimer(p.initTimeout)
	defer timeoutTimer.Stop()
	
	// Create a ticker for throttling error messages
	errorTicker := time.NewTicker(500 * time.Millisecond)
	defer errorTicker.Stop()
	lastErrorTime := time.Now()
	
	for {
		select {
		case <-timeoutTimer.C:
			p.logger.Error().Msgf("[nyxsub|%s] Timed out waiting for ready message", p.name)
			return
		default:
			// Use a critical section for the actual read
			p.mutex.Lock()
			stdout := p.stdout
			if stdout == nil {
				p.mutex.Unlock()
				return
			}
			
			line, err := stdout.ReadString('\n')
			p.mutex.Unlock()
			
			if err != nil {
				// Throttle error messages to avoid spamming logs
				if time.Since(lastErrorTime) > 500*time.Millisecond {
					p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stdout", p.name)
					lastErrorTime = time.Now()
				}
				
				// If this is a real error rather than just timeout, restart
				if err != io.EOF {
					p.Restart()
					return
				}
				
				// Brief sleep to avoid CPU spinning on EOF
				time.Sleep(10 * time.Millisecond)
				continue
			}
			
			if line == "" || line == "\n" {
				continue
			}

			// Clear the response map for reuse
			for k := range responseBuf {
				delete(responseBuf, k)
			}
			
			if err := json.Unmarshal([]byte(line), &responseBuf); err != nil {
				p.logger.Warn().Msgf("[nyxsub|%s] Non JSON message received: '%s'", p.name, line)
				continue
			}

			// Check for ready message
			if typeVal, ok := responseBuf["type"]; ok && typeVal == "ready" {
				p.logger.Info().Msgf("[nyxsub|%s] Process is ready", p.name)
				p.SetReady(1)
				return
			}
		}
	}
}

// SendCommand sends a command to the process and waits for the response.
func (p *Process) SendCommand(cmd map[string]interface{}) (map[string]interface{}, error) {
	p.SetBusy(1)
	defer p.SetBusy(0)

	// Pre-allocate common fields only if needed for better performance
	if _, ok := cmd["id"]; !ok {
		cmd["id"] = uuid.New().String()
	}
	if _, ok := cmd["type"]; !ok {
		cmd["type"] = "main"
	}

	start := time.Now().UnixMilli()

	// Use a shorter critical section with RLock for checking nil 
	// The actual Encode operation needs exclusive access
	p.mutex.RLock()
	if p.stdin == nil {
		p.mutex.RUnlock()
		p.logger.Error().Msgf("[nyxsub|%s] stdin is nil", p.name)
		p.Restart()
		return nil, errors.New("stdin is nil")
	}
	p.mutex.RUnlock()
	
	// Lock only for the actual encode operation
	p.mutex.Lock()
	err := p.stdin.Encode(cmd)
	p.mutex.Unlock()
	
	if err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to send command", p.name)
		p.Restart()
		return nil, err
	}

	// Log the command sent only at debug level
	if p.logger.Debug().Enabled() {
		jsonCmd, _ := json.Marshal(cmd)
		p.logger.Debug().Msgf("[nyxsub|%s] Command sent: %v", p.name, string(jsonCmd))
	}

	// Wait for response
	response, err := p.readResponse(cmd["id"].(string))
	if err != nil {
		p.Restart()
		return nil, err
	}

	// Use atomic operations for metrics updates to reduce lock contention
	now := time.Now().UnixMilli()
	latency := now - start
	
	p.mutex.Lock()
	p.latency = latency
	p.requestsHandled++
	p.mutex.Unlock()

	return response, nil
}

// readResponse reads the response for a specific command ID.
func (p *Process) readResponse(cmdID string) (map[string]interface{}, error) {
	// Pre-allocate timeout timer once rather than recreating it on each iteration
	timeoutTimer := time.NewTimer(p.timeout)
	defer timeoutTimer.Stop()

	// Pre-check stdout with read lock before entering the loop
	p.mutex.RLock()
	if p.stdout == nil {
		p.mutex.RUnlock()
		return nil, errors.New("stdout is nil")
	}
	p.mutex.RUnlock()

	// Create a small buffer for reads to reduce allocations
	responseBuf := make(map[string]interface{}, 4)
	
	for {
		select {
		case <-timeoutTimer.C:
			p.logger.Error().Msgf("[nyxsub|%s] Communication timed out", p.name)
			return nil, errors.New("communication timed out")
		default:
			// Use exclusive lock for the actual read since bufio.Reader is not threadsafe
			p.mutex.Lock()
			// Double-check stdout is not nil before reading
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
				continue
			}

			// Reset map for reuse to reduce allocations
			for k := range responseBuf {
				delete(responseBuf, k)
			}
			
			if err := json.Unmarshal([]byte(line), &responseBuf); err != nil {
				p.logger.Warn().Msgf("[nyxsub|%s] Non JSON message received: '%s'", p.name, line)
				continue
			}

			// Check for matching response ID
			if id, ok := responseBuf["id"]; ok && id == cmdID {
				// Create a copy of the response to return
				response := make(map[string]interface{}, len(responseBuf))
				for k, v := range responseBuf {
					response[k] = v
				}
				return response, nil
			}
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
	// Initialize the queue with estimated capacity for all processes
	pool.queue = ProcessPQ{
		processes: make([]*ProcessWithPrio, 0, size),
		mutex:     sync.RWMutex{},
		pool:      pool,
	}
	
	// Create all the processes
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
	var exports []ProcessExport
	for _, process := range pool.processes {
		if process != nil {
			// Use read lock since we're only reading the process state
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

// GetWorker returns a worker process from the process pool.
func (pool *ProcessPool) GetWorker() (*Process, error) {
	// Early exit if pool is shutting down
	if atomic.LoadInt32(&pool.shouldStop) == 1 {
		return nil, fmt.Errorf("process pool is stopping")
	}

	// Use timer for better performance than time.After
	timeoutTimer := time.NewTimer(pool.workerTimeout)
	defer timeoutTimer.Stop()
	
	// Use adaptive ticker frequency - start fast, then slow down
	// This improves responsiveness while reducing CPU usage for long waits
	tickerInterval := time.Millisecond * 10 // Start with faster polling
	maxInterval := time.Millisecond * 100   // Maximum interval
	tickCount := 0
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutTimer.C:
			return nil, fmt.Errorf("timeout exceeded, no available workers")
		case <-ticker.C:
			// Adaptive polling - increase interval gradually
			tickCount++
			if tickCount > 10 && tickerInterval < maxInterval {
				tickerInterval = tickerInterval * 2
				if tickerInterval > maxInterval {
					tickerInterval = maxInterval
				}
				ticker.Reset(tickerInterval)
			}
			
			// Check for any ready workers before locking queue
			anyReady := false
			pool.mutex.RLock()
			for _, process := range pool.processes {
				if process != nil && atomic.LoadInt32(&process.isReady) == 1 && 
				   atomic.LoadInt32(&process.isBusy) == 0 {
					anyReady = true
					break
				}
			}
			pool.mutex.RUnlock()
			
			if !anyReady {
				continue // No workers available, avoid locking the queue
			}
			
			// Update the queue with available workers
			pool.queue.mutex.Lock()
			pool.queue.Update()
			
			// Check if we have any available workers
			if pool.queue.Len() > 0 {
				// Use the standard heap package interface
				processWithPrio := heap.Pop(&pool.queue).(*ProcessWithPrio)
				pid := processWithPrio.processId
				pool.queue.mutex.Unlock()
				
				// Try to get and mark the process as busy atomically
				var process *Process
				pool.mutex.RLock()
				if pid < len(pool.processes) {
					process = pool.processes[pid]
				}
				pool.mutex.RUnlock()
				
				// Verify process exists and is ready
				if process != nil {
					// Try to mark process as busy atomically
					if atomic.LoadInt32(&process.isReady) == 1 && 
					   atomic.CompareAndSwapInt32(&process.isBusy, 0, 1) {
						// Reset ticker interval on success
						tickerInterval = time.Millisecond * 10
						ticker.Reset(tickerInterval)
						return process, nil
					}
				}
				
				// Process wasn't available, retry immediately
				tickerInterval = time.Millisecond * 10
				ticker.Reset(tickerInterval)
			} else {
				pool.queue.mutex.Unlock()
			}
		}
	}
}

// WaitForReady waits until at least one worker is ready or times out.
func (pool *ProcessPool) WaitForReady() error {
	// Use timer instead of time.After for better performance
	timeoutTimer := time.NewTimer(pool.initTimeout)
	defer timeoutTimer.Stop()
	
	// Use adaptive polling strategy - start with short intervals, then increase
	// This gives good responsiveness while keeping CPU usage reasonable
	pollInterval := 20 * time.Millisecond  // Start with short intervals
	maxInterval := 200 * time.Millisecond  // Maximum polling interval
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	
	pollCount := 0
	
	// Look for ready workers
	for {
		select {
		case <-timeoutTimer.C:
			return fmt.Errorf("timeout waiting for workers to be ready")
		case <-ticker.C:
			// Adaptive polling - increase interval gradually
			pollCount++
			if pollCount > 5 && pollInterval < maxInterval {
				pollInterval = pollInterval * 2
				if pollInterval > maxInterval {
					pollInterval = maxInterval
				}
				ticker.Reset(pollInterval)
			}
			
			// Check if any worker is ready
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
		}
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
	mutex     sync.RWMutex // Changed to RWMutex for more efficient concurrent reads
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
	// Note: This function should be called with pq.mutex already locked
	
	// Reuse existing slice if possible to reduce allocations
	if len(pq.processes) > 0 {
		pq.processes = pq.processes[:0] // Clear slice but maintain capacity
	} else if cap(pq.processes) < len(pq.pool.processes) {
		// If slice capacity is too small, allocate a new one
		pq.processes = make([]*ProcessWithPrio, 0, len(pq.pool.processes))
	}
	
	// Create a small cache of ProcessWithPrio objects to reuse
	var prioCache []*ProcessWithPrio
	
	// Use atomic operations for faster ready/busy checks
	pq.pool.mutex.RLock()
	for _, process := range pq.pool.processes {
		if process != nil && 
		   atomic.LoadInt32(&process.isReady) == 1 && 
		   atomic.LoadInt32(&process.isBusy) == 0 {
			
			process.mutex.RLock() // Use RLock since we're only reading
			handled := process.requestsHandled
			process.mutex.RUnlock()
			
			// Reuse objects from cache when possible to reduce allocations
			var pwp *ProcessWithPrio
			if len(prioCache) > 0 {
				pwp = prioCache[len(prioCache)-1]
				prioCache = prioCache[:len(prioCache)-1]
				pwp.processId = process.id
				pwp.handled = handled
			} else {
				pwp = &ProcessWithPrio{
					processId: process.id,
					handled:   handled,
				}
			}
			
			pq.processes = append(pq.processes, pwp)
		}
	}
	pq.pool.mutex.RUnlock()
	
	// Only initialize the heap if we have processes
	if len(pq.processes) > 0 {
		heap.Init(pq)
	}
}
