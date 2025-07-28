# Quay Storage Driver

The Quay Storage Driver integrates the OCI Distribution registry with Quay's existing database-backed storage system.

## Features

- **Database Integration**: Connects to Quay's database to lookup blob and manifest metadata
- **S3 Signed URLs**: Generates pre-signed S3 URLs for blob access, eliminating the need to proxy blob data
- **Direct Manifest Serving**: Fetches manifest data directly from the database and serves it efficiently
- **Read-Only Operations**: Designed for read-only operations, leveraging Quay's existing storage logic for writes

## Configuration

The driver requires the following parameters:

### Required Parameters

- `databaseurl`: Connection string for Quay's database (e.g., `postgres://user:pass@localhost/quay`)
- `s3bucket`: S3 bucket name where blobs are stored

### Optional Parameters

- `databasedriver`: Database driver type (default: `postgres`)
- `s3region`: AWS region for S3 (default: `us-east-1`)
- `s3accesskey`: AWS access key for S3 operations
- `s3secretkey`: AWS secret key for S3 operations  
- `s3endpoint`: Custom S3 endpoint URL
- `signedurlDuration`: Duration for signed URLs (default: `15m`)

## Example Configuration

```yaml
storage:
  quaystorage:
    databaseurl: postgres://quay:password@localhost:5432/quay
    s3bucket: my-quay-storage-bucket
    s3region: us-west-2
    signedurlDuration: 30m
```

## Architecture

### Path Handling

The driver recognizes two types of paths:

1. **Manifest Paths**: Contain `/manifests/` and are served directly from the database
   - Example: `/docker/registry/v2/repositories/myrepo/manifests/latest`

2. **Blob Paths**: Contain `/blobs/` and are redirected to S3 signed URLs
   - Example: `/docker/registry/v2/blobs/sha256/ab/abcd1234.../data`

### Database Schema Assumptions

The driver expects the following tables/columns in Quay's database:

- `manifest` table with columns:
  - `repository_name`: Repository name
  - `tag`: Manifest tag/reference
  - `manifest_bytes`: Raw manifest data

- `blob_storage` table with columns:
  - `blob_id`: Blob identifier (hash)
  - `storage_path`: S3 object key/path
  - `size`: Blob size in bytes

## Supported Operations

### Supported Methods

- `GetContent()`: Fetches manifest data from DB or blob data via signed URL
- `Reader()`: Provides streaming access to content
- `Stat()`: Returns file metadata
- `RedirectURL()`: Returns S3 signed URLs for blobs
- `Name()`: Returns driver name

### Unsupported Methods

The following methods return `ErrUnsupportedMethod` as they should be handled by Quay's existing logic:

- `PutContent()`
- `Writer()`
- `List()`
- `Move()`
- `Delete()`
- `Walk()`

## Usage

To use the driver, import it in your application:

```go
import _ "github.com/distribution/distribution/v3/registry/storage/driver/quaystorage"
```

The driver will automatically register itself with the name `"quaystorage"`.

## Testing

Run the driver tests:

```bash
go test ./registry/storage/driver/quaystorage/
```

The tests cover parameter validation, path parsing, and interface compliance.

## Notes

- This driver is designed specifically for integration with Quay's database schema
- It assumes blobs are stored in S3-compatible storage
- Write operations are intentionally unsupported to maintain data consistency with Quay's existing storage logic
- The driver requires appropriate database and S3 permissions for the configured credentials