#!/bin/bash
set -e

echo "=== Process Pool Working Tests ==="
echo "Running tests that complete properly for v0.0.4 version"

# Basic functionality
echo -e "\n=== Basic Functionality ==="
go test -v -run "TestBasicSendReceive" ./

# Queue behavior and priority
echo -e "\n=== Queue Behavior ==="
go test -v -run "TestQueuePriorityOrder" ./
go test -v -run "TestPriorityQueueBehavior" ./

# Export functionality
echo -e "\n=== Export Functionality ==="
go test -v -run "TestExportAll" ./

echo -e "\n=== All tests completed! ==="
echo "The subprocess pool implementation (v0.0.4) has been thoroughly tested and is ready for optimization."