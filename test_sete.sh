#!/bin/bash
set -e
set +e
echo "Testing with set +e"
set -e
exit 1
echo "This should not print"
