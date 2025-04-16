#!/bin/bash
set -e

echo "=== Process Pool Test Subset ==="
echo "Testing critical functionality of v0.0.4 version"

# Create a directory for test results
mkdir -p test_results

# Run a basic test to verify functionality
echo -e "\n=== Basic Functionality ==="
go test -v -run "TestBasicSendReceive" ./

# Run the priority queue behavior test
echo -e "\n=== Priority Queue Behavior ==="
go test -v -run "TestPriorityQueueBehavior" ./

# Run one edge case test
echo -e "\n=== Edge Case Test ==="
go test -v -run "TestExportAll" ./

echo -e "\n=== Subset of tests passed! ==="
echo "The key functionality is verified."