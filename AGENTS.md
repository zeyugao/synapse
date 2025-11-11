# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Development Commands

### Building
- `make all` - Build both client and server binaries
- `make client` - Build only the client binary (`./bin/client`)
- `make server` - Build only the server binary (`./bin/server`)
- `make clean` - Remove all built binaries from `./bin/`
- `make docker` - Build Docker image with tag `ghcr.io/zeyugao/synapse:latest`

### Running
- Server: `./bin/server --host localhost --port 8080 [options]`
- Client: `./bin/client --server-url ws://localhost:8080/ws --base-url http://localhost:8081/v1 [options]`

### Version Management
- Version is automatically determined from git: `git rev-parse --short=6 HEAD` + `-dev` suffix if working directory is dirty
- Both binaries embed version info via build-time `-ldflags`
- When modifying the client/server communication protocol, bump the semantic version major; use the minor component for backwards-compatible changes.

## Architecture

Synapse is a reverse proxy system for LLM APIs with two main components:

### Core Components
1. **Server** (`internal/server/server.go`): Central WebSocket hub that receives API requests and forwards to clients
2. **Client** (`internal/client/client.go`): Connects to both upstream LLM APIs and the Synapse server via WebSocket

### Key Features
- **WebSocket Communication**: Bidirectional communication between server and clients
- **Dynamic Model Discovery**: Clients register available models with the server
- **Load Balancing**: Multiple clients can serve the same model; server selects based on minimal load
- **Automatic Reconnection**: Clients automatically reconnect on connection failures
- **Version Compatibility**: Server enforces version matching between clients and server
- **Heartbeat Mechanism**: Maintains connection health with periodic heartbeats
- **Streaming Support**: Handles both regular and streaming LLM responses

### Message Types
- `TypeNormal` (0): Standard request/response
- `TypeStream` (1): Streaming response chunks
- `TypeHeartbeat` (2): Client heartbeat
- `TypePong` (3): Server heartbeat response
- `TypeClientClose` (4): Request cancellation
- `TypeModelUpdate` (5): Model list updates
- `TypeUnregister` (6): Client disconnect
- `TypeForceShutdown` (7): Force close active requests

### Data Flow
1. External API request → Server HTTP handler
2. Server selects client based on model and load
3. Server forwards request via WebSocket to client
4. Client makes HTTP request to upstream LLM API
5. Client streams/sends response back to server
6. Server returns response to original API caller

## Project Structure

- `cmd/server/main.go` - Server entry point with CLI argument parsing
- `cmd/client/main.go` - Client entry point with CLI argument parsing  
- `internal/server/server.go` - Core server logic (WebSocket handling, request routing, load balancing)
- `internal/client/client.go` - Core client logic (upstream API communication, reconnection)
- `internal/types/types.go` - Shared data structures and message types
- `Makefile` - Build configuration
- `Dockerfile` - Multi-stage build using Go 1.23 and distroless runtime

## Authentication
- API requests: Bearer token via `--api-auth-key` server flag
- WebSocket connections: Query parameter via `--ws-auth-key` (both server and client)

## Dependencies
- `github.com/gorilla/websocket` - WebSocket implementation
- Go 1.23+ required
- No other external dependencies

## Protobuf Schema
- Protocol message definitions live in `internal/types/proto/messages.proto`, and the generated Go bindings are kept in `internal/types/proto/messages.pb.go`.
- After making changes to `messages.proto`, run `protoc --go_out=. --go_opt=paths=source_relative internal/types/proto/messages.proto` from the repo root to regenerate the Go file.
- If `protoc` is not installed, install it (and the headers) with `sudo apt install libprotobuf-dev protobuf-compiler` before rerunning the generation command.
