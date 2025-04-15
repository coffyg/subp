Simple subprocess management

## Overview
A fast, efficient process pool for managing long-running processes with proper work queueing.

## Features

- **Efficient Process Management**: Manages a pool of worker processes
- **Work Queueing**: Tasks queue properly when all workers are busy 
- **Full Timeout Respect**: Worker timeout is completely controlled by the developer
- **Auto-restart**: Workers automatically restart on communication errors
- **Fast JSON Communication**: Using stdin/stdout with optimized JSON marshaling

## Usage

Use this package to manage worker processes using a shared pool - great for both quick tasks (like SSR rendering) and long-running operations.

The worker timeout provided during initialization will be fully respected before returning a "no available workers" error.
