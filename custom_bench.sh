#!/bin/bash

# Custom benchmark script for process pool performance testing
set -e

echo "==================================================================="
echo "Process Pool Performance Benchmarks"
echo "==================================================================="
echo ""

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Benchmark settings
BENCH_TIME=${BENCH_TIME:-"10s"}
BENCH_COUNT=${BENCH_COUNT:-""}

# Build bench time argument
TIME_ARG=""
if [ -n "$BENCH_TIME" ]; then
    TIME_ARG="-benchtime=$BENCH_TIME"
fi

# Function to run a specific benchmark
run_benchmark() {
    local name=$1
    local pattern=$2
    echo -e "${GREEN}Running: $name${NC}"
    echo "-------------------------------------------------------------------"
    if [ -n "$BENCH_COUNT" ]; then
        go test -bench="$pattern" -benchmem -run=^$ -count=$BENCH_COUNT $TIME_ARG . | grep -E "^(Benchmark|ns/op|allocs/op|reqs/s|avg_μs)"
    else
        go test -bench="$pattern" -benchmem -run=^$ $TIME_ARG . | grep -E "^(Benchmark|ns/op|allocs/op|reqs/s|avg_μs)"
    fi
    echo ""
}

# Run all benchmarks
echo -e "${YELLOW}1. WORKER ACQUISITION PERFORMANCE${NC}"
run_benchmark "Worker Acquisition Latency" "BenchmarkWorkerAcquisitionLatency"
run_benchmark "Worker Acquisition Under Load" "BenchmarkWorkerAcquisitionUnderLoad"

echo -e "${YELLOW}2. QUEUE OPERATIONS PERFORMANCE${NC}"
run_benchmark "Queue Operations" "BenchmarkQueueOperations"

echo -e "${YELLOW}3. CONCURRENCY & LOCK PERFORMANCE${NC}"
run_benchmark "Lock Contention" "BenchmarkLockContention"

echo -e "${YELLOW}4. JSON COMMUNICATION OVERHEAD${NC}"
run_benchmark "JSON Overhead" "BenchmarkJSONOverhead"

echo -e "${YELLOW}5. THROUGHPUT MEASUREMENTS${NC}"
run_benchmark "High Throughput" "BenchmarkHighThroughput"

echo -e "${YELLOW}6. LATENCY MEASUREMENTS${NC}"
run_benchmark "Latency Percentiles" "BenchmarkLatencyPercentiles"

echo -e "${YELLOW}7. RESOURCE USAGE${NC}"
run_benchmark "Memory Allocation" "BenchmarkMemoryAllocation"
run_benchmark "Worker Lifecycle" "BenchmarkWorkerLifecycle"

echo -e "${YELLOW}8. SCALABILITY${NC}"
run_benchmark "Scalability" "BenchmarkScalability"

echo -e "${YELLOW}9. ORIGINAL BENCHMARKS (for comparison)${NC}"
run_benchmark "Send Command" "BenchmarkSendCommand"
run_benchmark "Concurrent Commands" "BenchmarkConcurrentCommands"

echo "==================================================================="
echo "Benchmark Summary Complete"
echo "==================================================================="

# Optional: Save results to file
if [ -n "$SAVE_RESULTS" ]; then
    TIMESTAMP=$(date +%Y%m%d_%H%M%S)
    RESULTS_FILE="bench_results_${TIMESTAMP}.txt"
    echo "Saving results to $RESULTS_FILE..."
    ./custom_bench.sh > "$RESULTS_FILE" 2>&1
fi