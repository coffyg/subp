#!/bin/bash
set -e

echo "=== Process Pool Edge Case Tests ==="
echo "Testing edge cases for v0.0.4 version"

# Test ExportAll method
echo -e "\n=== TestExportAll ==="
go test -v -run "TestExportAll" ./

# Test priority queue behavior
echo -e "\n=== TestPriorityQueueBehavior ==="
go test -v -run "TestPriorityQueueBehavior" ./

echo -e "\n=== All edge case tests completed! ==="