package main

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Function to create the fix file
func main() {
	fixSrc := `package subp

import (
	"fmt"
	"sync"
	"time"
)

// FIX: Modify the waitForWorker function to properly wait for available workers
// This function was changed in the optimization to use polling with backoff,
// which breaks the queueing system and causes "no available workers" errors
// even with low traffic.

// waitForWorker waits for a worker to become available with a timeout
func (pool *ProcessPool) waitForWorker() (*Process, error) {
	// Fast path - try to get a worker immediately
	process := pool.tryGetWorkerFast()
	if process != nil {
		return process, nil
	}

	// Create a notification channel for when workers become available
	waitCh := make(chan struct{}, 1)
	
	// Register with the pool to be notified when workers become available
	pool.mutex.Lock()
	// Lazy initialize the waiters slice if needed
	if pool.waiters == nil {
		pool.waiters = make([]chan struct{}, 0, 8) // Pre-allocate for common case
	}
	pool.waiters = append(pool.waiters, waitCh)
	pool.mutex.Unlock()
	
	// Cleanup on exit
	defer func() {
		pool.mutex.Lock()
		// Remove our channel from the waiters list
		for i, ch := range pool.waiters {
			if ch == waitCh {
				// Fast removal without preserving order
				lastIdx := len(pool.waiters) - 1
				pool.waiters[i] = pool.waiters[lastIdx] 
				pool.waiters = pool.waiters[:lastIdx]
				break
			}
		}
		pool.mutex.Unlock()
		close(waitCh)
	}()

	// Use timer from pool for timeout
	timer := timerPool.Get().(*time.Timer)
	resetTimer(timer, pool.workerTimeout)
	defer timerPool.Put(timer)
	
	// Keep trying until timeout
	for {
		// Check if any worker is available right now
		process := pool.tryGetWorkerFast()
		if process != nil {
			return process, nil
		}

		// Wait for either a worker notification or timeout
		select {
		case <-timer.C:
			// We've timed out waiting for a worker
			return nil, fmt.Errorf("timeout exceeded, no available workers")
		case <-waitCh:
			// A worker might be available, try to get it
			process := pool.tryGetWorkerFast()
			if process != nil {
				return process, nil
			}
			// If we couldn't get the worker, continue waiting
		}
	}
}

// FIX: Add waiters field to ProcessPool for notification system
// Add this field to the ProcessPool struct

// ProcessPool struct should now contain:
// waiters []chan struct{}  // Channels to notify when workers become available

// FIX: Modify SetBusy to notify waiters when a worker becomes available
// This function should notify any waiters when a worker goes from busy to ready

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
`

	err := os.WriteFile("fix.patch", []byte(fixSrc), 0644)
	if err != nil {
		fmt.Printf("Error creating patch file: %v\n", err)
		return
	}

	fmt.Println("=== CRITICAL FIX FOR PROCESS POOL ===")
	fmt.Println()
	fmt.Println("The issue is in the waitForWorker function. The optimization replaced")
	fmt.Println("the previous notification system with polling + backoff, which breaks")
	fmt.Println("the queueing behavior and causes 'no available workers' errors.")
	fmt.Println()
	fmt.Println("To fix this without rolling back, you need to:")
	fmt.Println()
	fmt.Println("1. Add a waiters field to ProcessPool struct:")
	fmt.Println("   waiters []chan struct{}")
	fmt.Println()
	fmt.Println("2. Replace waitForWorker with notification-based waiting")
	fmt.Println()
	fmt.Println("3. Update SetBusy to notify waiters when workers become available")
	fmt.Println()
	fmt.Println("A complete patch file has been created at: fix.patch")
	fmt.Println("This maintains all optimizations while fixing the queueing behavior.")
}