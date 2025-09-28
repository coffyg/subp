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
	activeCommandID string // Currently executing command ID
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

	// Only start goroutines after process successfully starts
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		p.readStderr()
	}()
	go func() {
		defer p.wg.Done()
		p.WaitForReadyScan()
	}()
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
func (p *Process) WaitForReadyScan() {
	for {
		line, err := p.stdout.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stdout", p.name)
			}
			// Signal that the process needs restart by marking it as not ready
			p.SetReady(0)
			// Simply return and let the process be restarted when needed
			// The process will be restarted by SendCommand when it fails
			return
		}
		if line == "" || line == "\n" {
			continue
		}

		var response map[string]interface{}
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			p.logger.Warn().Msgf("[nyxsub|%s] Non JSON message received: '%s'", p.name, line)
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

	// Lock for command operations to prevent concurrent access to stdin/stdout
	p.commandMutex.Lock()

	// Track active command
	cmdID := cmd["id"].(string)
	p.activeCommandID = cmdID

	// Send command
	if err := p.stdin.Encode(cmd); err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to send command", p.name)
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

	// Wait for response
	response, err := p.readResponse(cmd["id"].(string))
	if err != nil {
		// Clean up local command tracking
		p.activeCommandID = ""
		// Mark as not ready before unlocking to prevent new commands
		p.SetReady(0)
		p.commandMutex.Unlock()
		p.Restart()
		return nil, err
	}
	
	// Clean up local command tracking on successful completion
	p.activeCommandID = ""
	p.commandMutex.Unlock()

	p.mutex.Lock()
	p.latency = time.Now().UnixMilli() - start
	p.requestsHandled++
	p.mutex.Unlock()

	return response, nil
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

// readResponse reads the response for a specific command ID.
func (p *Process) readResponse(cmdID string) (map[string]interface{}, error) {
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			p.logger.Error().Msgf("[nyxsub|%s] Communication timed out", p.name)
			return nil, errors.New("communication timed out")
		default:
			line, err := p.stdout.ReadString('\n')
			if err != nil {
				p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to read stdout", p.name)
				return nil, err
			}
			if line == "" || line == "\n" {
				continue
			}

			var response map[string]interface{}
			if err := json.Unmarshal([]byte(line), &response); err != nil {
				p.logger.Warn().Msgf("[nyxsub|%s] Non JSON message received: '%s'", p.name, line)
				continue
			}

			// Check for matching response ID
			if response["id"] == cmdID {
				// Only return for final response types, not intermediate ones like "started"
				responseType, hasType := response["type"]
				if !hasType {
					// No type field - assume it's a final response
					return response, nil
				}
				
				// Skip intermediate responses like "started", only return for final ones
				switch responseType {
				case "started", "progress", "info":
					// Intermediate response - continue waiting for final response
					continue
				default:
					// Final response (success, error, interrupted, etc.) - return it
					return response, nil
				}
			}
		}
	}
}

// queuedCommand represents a command waiting in the queue
type queuedCommand struct {
	cmd      map[string]interface{}
	response chan commandResponse
}

// commandResponse holds the response or error from a command
type commandResponse struct {
	result map[string]interface{}
	err    error
}

// ProcessPool is a pool of processes.
type ProcessPool struct {
	processes        []*Process
	mutex            sync.RWMutex
	logger           *zerolog.Logger
	shouldStop       int32
	stop             chan bool
	workerTimeout    time.Duration
	comTimeout       time.Duration
	initTimeout      time.Duration
	availableWorkers chan *Process // Channel for available workers
	commandToProcess map[string]*Process // Track which process is handling which command ID
	cancelledCommands map[string]bool // Track cancelled command IDs
	commandQueue     chan *queuedCommand // Real queue for commands
	queueDepth       int // Maximum queue depth
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
		processes:        make([]*Process, size),
		logger:           logger,
		mutex:            sync.RWMutex{},
		shouldStop:       shouldStop,
		stop:             make(chan bool, 1),
		workerTimeout:    workerTimeout,
		comTimeout:       comTimeout,
		initTimeout:      initTimeout,
		availableWorkers: make(chan *Process, size), // Buffer size = number of workers
		commandToProcess: make(map[string]*Process),
		cancelledCommands: make(map[string]bool),
		commandQueue:     make(chan *queuedCommand, queueDepth),
		queueDepth:       queueDepth,
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
		case qCmd := <-pool.commandQueue:
			// Get command ID for tracking
			cmdID, ok := qCmd.cmd["id"].(string)
			if !ok {
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
				
				qCmd.response <- commandResponse{
					err: fmt.Errorf("command %s was interrupted while queued", cmdID),
				}
				close(qCmd.response)
				continue
			}
			
			// Wait for an available worker with timeout
			var worker *Process
			workerTimer := time.NewTimer(pool.workerTimeout)
			defer workerTimer.Stop()
			
			select {
			case <-pool.stop:
				// Pool is stopping
				qCmd.response <- commandResponse{err: fmt.Errorf("pool is stopping")}
				close(qCmd.response)
				
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
						cmd.response <- commandResponse{err: fmt.Errorf("pool stopping during restart")}
						close(cmd.response)
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
				
				qCmd.response <- commandResponse{
					err: fmt.Errorf("command %s was interrupted while queued", cmdID),
				}
				close(qCmd.response)
				continue
			}
			
			// Update tracking to show which worker is handling this command
			pool.mutex.Lock()
			pool.commandToProcess[cmdID] = worker
			pool.mutex.Unlock()
			
			// Execute command on worker (in goroutine to not block dispatcher)
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
