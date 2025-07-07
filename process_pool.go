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
	commandMutex    sync.Mutex    // Mutex for command send/receive operations
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
			// Schedule restart asynchronously to avoid goroutine leak
			if atomic.LoadInt32(&p.pool.shouldStop) == 0 {
				go func() {
					p.Restart()
				}()
			}
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

	// Send command
	if err := p.stdin.Encode(cmd); err != nil {
		p.logger.Error().Err(err).Msgf("[nyxsub|%s] Failed to send command", p.name)
		p.commandMutex.Unlock()
		p.Restart()
		return nil, err
	}

	// Log the command sent
	jsonCmd, _ := json.Marshal(cmd)
	p.logger.Debug().Msgf("[nyxsub|%s] Command sent: %v", p.name, string(jsonCmd))

	// Wait for response
	response, err := p.readResponse(cmd["id"].(string))
	p.commandMutex.Unlock()
	if err != nil {
		p.Restart()
		return nil, err
	}

	p.mutex.Lock()
	p.latency = time.Now().UnixMilli() - start
	p.requestsHandled++
	p.mutex.Unlock()

	return response, nil
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
	shouldStop    int32
	stop          chan bool
	workerTimeout time.Duration
	comTimeout    time.Duration
	initTimeout   time.Duration
	availableWorkers chan *Process // Channel for available workers
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
		availableWorkers: make(chan *Process, size), // Buffer size = number of workers
	}
	// Priority queue no longer needed with channel-based approach
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
