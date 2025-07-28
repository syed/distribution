# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is **Distribution** - the reference implementation of the OCI Distribution Specification, a container registry for storing and distributing container images and other content. It's the core registry implementation used by Docker Hub, GitHub Container Registry, GitLab Container Registry, and Harbor.

## Build and Development Commands

### Quick Start
```bash
# Build all binaries
make

# Run registry locally (requires /var/lib/registry directory or REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY env var)
./bin/registry serve cmd/registry/config-dev.yml

# The registry will be available at:
# - Main API: http://localhost:5000
# - Debug/health: http://localhost:5001
```

### Build Commands
```bash
make build              # Build Go packages
make binaries          # Build all binaries (registry, digest, registry-api-descriptor-template)
make bin/registry      # Build specific binary
make clean             # Clean up binaries
make image             # Build Docker image
```

### Testing Commands
```bash
make test              # Unit tests (with test.short)
make test-race         # Unit tests with race detection
make test-full         # All unit tests
make integration       # Integration tests
make test-coverage     # Unit tests with coverage

# Storage-specific tests
make test-s3-storage       # S3 tests with local MinIO
make start-s3-storage      # Start local S3 environment
make stop-s3-storage       # Stop local S3 environment
make test-azure-storage    # Azure storage tests
make start-azure-storage   # Start local Azure storage (Azurite)
make stop-azure-storage    # Stop local Azure storage

# E2E tests  
make start-e2e-s3-env     # Start full E2E S3 environment (S3, Redis, registry)
make stop-e2e-s3-env      # Stop E2E S3 environment
```

### Validation and Linting
```bash
make validate          # All validators (requires Docker buildx)
make lint              # Run linters
make validate-git      # Git validation
make validate-vendor   # Vendor validation
make validate-authors  # Authors validation
```

### Running Single Tests
```bash
# Run tests for specific package
go test ./registry/storage/driver/filesystem/
go test ./registry/handlers/

# Run specific test
go test -run TestSpecificFunction ./path/to/package/

# Run with verbose output
go test -v ./registry/...
```

## Architecture Overview

### Core Components
- **Registry Server** (`cmd/registry/`) - Main HTTP server entry point
- **Storage Layer** (`registry/storage/`) - Blob and manifest storage abstraction
- **API Handlers** (`registry/handlers/`) - OCI Distribution API v2 implementation  
- **Authentication** (`registry/auth/`) - Multiple auth backends (htpasswd, token, silly)
- **Storage Drivers** - Pluggable backends (filesystem, S3, Azure, GCS, in-memory)
- **Notifications** (`notifications/`) - Event-driven webhook system
- **Client Libraries** (`internal/client/`) - HTTP client for registry communication

### Storage Driver Architecture
Storage drivers implement the `storagedriver.StorageDriver` interface:
- **Filesystem** (`registry/storage/driver/filesystem/`) - Local filesystem storage
- **S3** (`registry/storage/driver/s3-aws/`) - Amazon S3 and S3-compatible storage  
- **Azure** (`registry/storage/driver/azure/`) - Azure Blob Storage
- **GCS** (`registry/storage/driver/gcs/`) - Google Cloud Storage
- **In-Memory** (`registry/storage/driver/inmemory/`) - For testing only

### Request Flow Architecture
```
HTTP Request → Router (gorilla/mux) → Auth Middleware → Handler → Storage Layer → Storage Driver
```

### Manifest Support
- **Docker Schema v2** (`manifest/schema2/`) 
- **OCI Image Spec** (`manifest/ocischema/`)
- **Manifest Lists** (`manifest/manifestlist/`) - Multi-platform support

## Key Configuration Files

### Development Configuration
- `cmd/registry/config-dev.yml` - Local development registry config
- `tests/conf-local-s3.yml` - Local S3 testing with MinIO
- `tests/conf-e2e-cloud-storage.yml` - E2E cloud storage testing

### Build Configuration  
- `Makefile` - Traditional build system
- `docker-bake.hcl` - Modern Docker buildx configuration with multi-platform support
- `go.mod` - Go module with Go 1.23.7+ requirement

## Testing Strategy

### Test Types
- **Unit Tests** - Package-level tests with mocks
- **Integration Tests** - Storage driver compliance tests with real backends
- **E2E Tests** - Full registry workflow tests using `tests/push.sh`
- **Fuzz Tests** - Security-focused fuzzing for manifest parsing and critical paths

### Storage Driver Testing
Each storage driver has compliance tests that verify:
- Basic read/write operations
- Path handling and traversal
- Concurrent access behavior
- Error conditions and recovery

### Local Development Testing
Use Docker Compose environments for testing against real storage:
```bash
# Test S3 integration locally
make start-s3-storage
AWS_ACCESS_KEY=distribution AWS_SECRET_KEY=password ./bin/registry serve tests/conf-local-s3.yml
```

## Common Development Patterns

### Adding New Storage Driver
1. Implement `storagedriver.StorageDriver` interface in `registry/storage/driver/yourdriver/`
2. Add factory function and registration
3. Add comprehensive compliance tests
4. Update documentation and build configuration

### Extending API Handlers
1. Add routes in `registry/handlers/` following existing patterns
2. Implement middleware chain for auth/logging
3. Add comprehensive unit and integration tests
4. Update API documentation

### Authentication Extensions
1. Implement auth interfaces in `registry/auth/`
2. Add configuration parsing
3. Integrate with middleware pipeline
4. Test with various auth scenarios

## Environment Variables

Key environment variables for development:
- `REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY` - Override default storage location
- `BUILDTAGS` - Optional build tags (e.g., `noresumabledigest`)
- `DISABLE_OPTIMIZATION` - Disable compiler optimizations for debugging

## Git and Development Workflow

### Vendor Management
```bash
make vendor            # Update vendor directory (uses Docker buildx)
make validate-vendor   # Validate vendor consistency
```

### Build Tags
Optional build tags can be set via `BUILDTAGS` environment variable:
- `noresumabledigest` - Compiles without resumable digest support

This registry implementation follows Go best practices with comprehensive testing, clean architecture separation, and production-ready scalability patterns.