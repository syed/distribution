# Distribution/Distribution Code Explanation

This document provides a detailed explanation of the `distribution` project, the open-source engine that powers Docker Registry and other OCI-compliant container registries.

## 1. High-Level Overview

The `distribution` project is a Go application designed to store, manage, and distribute container images according to the OCI (Open Container Initiative) and Docker V2 specifications.

### Core Concepts

- **Pluggable Storage**: The registry is not tied to a specific backend. It uses a storage driver interface, allowing it to store image layers (blobs) and manifests on various backends like a local filesystem, S3, Google Cloud Storage, Azure Blob Storage, and more.
- **Content Addressable Storage**: Image layers (blobs) are stored and retrieved by their hash (digest). This deduplicates storage, as identical layers are only stored once, regardless of how many images reference them.
- **Manifests**: A manifest is a JSON file that describes an image. It contains references to the image's configuration object and its layers (blobs), identified by their digests.
- **Extensible Authentication**: Authentication is handled via a flexible middleware system, allowing for different auth schemes like token-based auth, htpasswd, or custom solutions.
- **Notifications**: The registry can be configured to send events (e.g., on image push) to external endpoints (sinks) for integration with CI/CD systems, logging, or other automation.

## 2. Request Flow

A typical `docker push` or `docker pull` operation involves a series of HTTP requests that are handled by the registry as follows:

1.  **Entrypoint**: An incoming HTTP request first hits the main `http.Server`.
2.  **Middleware**: The request passes through a chain of middleware:
    - **Logging**: Access logs are recorded.
    - **Health & Liveness**: Endpoints like `/_/ping` and `/v2/` are checked.
    - **Authentication**: The configured authentication middleware (`auth` package) validates credentials and authorizes the action on a specific repository.
    - **Tracing/Metrics**: OpenTelemetry hooks capture tracing data for the request.
3.  **API Routing**: The request is routed to a specific API handler in the `registry/handlers` package based on its URL path (e.g., `/v2/<name>/blobs/<digest>`, `/v2/<name>/manifests/<tag>`).
4.  **Handler Logic**: The handler parses the request and interacts with the core `registry.Registry` object.
5.  **Storage Interaction**: The registry object calls the configured **storage driver** (`registry/storage/driver`) to perform the required action, such as reading a manifest, writing a blob, or listing tags.
6.  **Response**: The handler constructs and sends the HTTP response back to the client.

## 3. Code Organization (Directory Breakdown)

Here is a breakdown of the key directories and their purpose.

### `cmd/registry/`
- **Purpose**: The main entry point of the registry application.
- **Key Files**:
    - `main.go`: Contains the `main` function. It's extremely simple: it just calls `registry.RootCmd.Execute()`, which is a pattern from the `cobra` CLI library.
    - The actual application startup logic is in the `registry` package, triggered by the `serve` command.

### `configuration/`
- **Purpose**: Handles parsing and validation of the registry's configuration file (typically a YAML file).
- **Key Files**:
    - `configuration.go`: Defines the `Configuration` struct, which holds all possible configuration parameters for the registry (storage, auth, http, notifications, etc.).
    - `parser.go`: Implements a sophisticated version-aware parser. It can read different versions of the configuration format and convert them to the current internal `Configuration` struct. It also handles overriding configuration values with environment variables (e.g., `REGISTRY_STORAGE_S3_BUCKET`).

### `registry/`
- **Purpose**: The core package of the application, tying everything together.
- **Key Files**:
    - `registry.go`: Defines the `Registry` struct, which represents a running registry instance. The `NewRegistry` function is the main constructor, responsible for:
        - Setting up logging.
        - Initializing the `handlers.App` which sets up API routes.
        - Wrapping the main handler with middleware (health checks, logging, auth).
        - Creating the `http.Server`.
    - `handlers/`: Contains the HTTP handlers for the Docker V2 API. Each endpoint (blobs, manifests, tags) has its own logic for handling requests. `app.go` in this directory sets up the routing.
    - `storage/`: Defines the storage driver interface and contains implementations for different backends.
        - `driver/`: Contains the `StorageDriver` interface and implementations for `filesystem`, `s3-aws`, `gcs`, `azure`, etc. This is the key to the pluggable storage architecture.
    - `auth/`: Implements different authentication schemes. The `token` auth handler is commonly used for production setups, integrating with an external auth service.

### `manifest/`
- **Purpose**: Contains definitions and handling logic for different image manifest formats.
- **Key Files**:
    - `schema1/`, `schema2/`, `ocischema/`: These packages define the Go structs that correspond to different versions of the Docker and OCI manifest specifications. They handle parsing, validation, and serialization of these manifest formats.

### `notifications/`
- **Purpose**: Implements the event notification system.
- **Key Files**:
    - `event.go`: Defines the `Event` struct, which contains information about an action that occurred in the registry (e.g., a manifest push).
    - `endpoint.go`: Defines the `Endpoint` interface for sending notifications.
    - `sinks.go`: Provides implementations for various notification sinks, such as HTTP endpoints. When an image is pushed, the registry can be configured to send an `Event` to one or more of these sinks.

### `health/`
- **Purpose**: Provides a framework for service health checking.
- **Key Files**:
    - `health.go`: Defines the core health checking handler.
    - `checks/`: Contains actual health check implementations, such as checking the status of the backend storage driver.

### `internal/`
- **Purpose**: Contains Go packages that are internal to the project and not intended for external use.
- **Key Files**:
    - `dcontext/`: A custom context package ("distribution context") used throughout the application to carry request-scoped information like loggers with request-specific fields, request IDs, and other important values.

### `version/`
- **Purpose**: Manages the application's version information.
- **Key Files**:
    - `version.go`: Contains the version string, which is typically injected at build time.

### Build & Test
- **`Dockerfile`**: Used to build the official `registry:2` container image.
- **`Makefile`**: Contains various build, test, and validation targets for development.
- **`tests/`**: Contains end-to-end tests and configuration for running them with `docker-compose`.
