// Package quaystorage provides a storagedriver.StorageDriver implementation to
// store blobs in Quay's database-backed storage system.
//
// This package integrates with Quay's existing database to lookup blob endpoints
// and generates S3 signed URLs for blob access. For manifests, it fetches
// manifest data directly from the database.
package quaystorage

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"

	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/base"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	"github.com/sirupsen/logrus"
)

const driverName = "quaystorage"

func init() {
	fmt.Println("registering quaystorage")
	factory.Register(driverName, &quayStorageDriverFactory{})
}

// quayStorageDriverFactory implements the factory.StorageDriverFactory interface.
type quayStorageDriverFactory struct{}

func (factory *quayStorageDriverFactory) Create(ctx context.Context, parameters map[string]interface{}) (storagedriver.StorageDriver, error) {
	logrus.Info("quayStorageDriverFactory.Create called")
	return New(parameters)
}

// DriverParameters contains all driver configuration parameters
type DriverParameters struct {
	// Database connection parameters
	DatabaseURL    string
	DatabaseDriver string

	// S3 configuration for blob storage
	S3Region          string
	S3Bucket          string
	S3AccessKey       string
	S3SecretKey       string
	S3Endpoint        string
	S3RootDirectory   string
	SignedURLDuration time.Duration
}

type driver struct {
	db         *sql.DB
	s3Session  *session.Session
	s3Client   *s3.S3
	parameters DriverParameters
}

// baseEmbed allows us to hide the Base embed.
type baseEmbed struct {
	base.Base
}

// Driver is a storagedriver.StorageDriver implementation backed by Quay's database.
type Driver struct {
	baseEmbed // embedded, hidden base driver.
}

var _ storagedriver.StorageDriver = &Driver{}

// New constructs a new QuayStorage Driver with given parameters.
func New(parameters map[string]interface{}) (*Driver, error) {
	logrus.Info("New called")
	params, err := parseParameters(parameters)
	if err != nil {
		return nil, err
	}

	// Initialize database connection
	db, err := sql.Open(params.DatabaseDriver, params.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// Test database connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Initialize S3 session for signed URL generation
	sess, err := session.NewSession(&aws.Config{
		Region:   aws.String(params.S3Region),
		Endpoint: aws.String(params.S3Endpoint),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 session: %w", err)
	}

	s3Client := s3.New(sess)

	d := &driver{
		db:         db,
		s3Session:  sess,
		s3Client:   s3Client,
		parameters: params,
	}

	return &Driver{
		baseEmbed: baseEmbed{
			Base: base.Base{
				StorageDriver: d,
			},
		},
	}, nil
}

// parseParameters processes the driver parameters
func parseParameters(parameters map[string]interface{}) (DriverParameters, error) {
	logrus.Info("parseParameters called")
	params := DriverParameters{
		DatabaseDriver:    "postgres", // default
		S3Region:          "us-east-1", // default
		S3RootDirectory:   "/quay/registry", // default
		SignedURLDuration: 15 * time.Minute, // default
	}

	if databaseURL, ok := parameters["databaseurl"]; ok {
		params.DatabaseURL = fmt.Sprint(databaseURL)
	} else {
		return params, fmt.Errorf("databaseurl parameter is required")
	}

	if databaseDriver, ok := parameters["databasedriver"]; ok {
		params.DatabaseDriver = fmt.Sprint(databaseDriver)
	}

	if s3Region, ok := parameters["s3region"]; ok {
		params.S3Region = fmt.Sprint(s3Region)
	}

	if s3Bucket, ok := parameters["s3bucket"]; ok {
		params.S3Bucket = fmt.Sprint(s3Bucket)
	} else {
		return params, fmt.Errorf("s3bucket parameter is required")
	}

	if s3AccessKey, ok := parameters["s3accesskey"]; ok {
		params.S3AccessKey = fmt.Sprint(s3AccessKey)
	}

	if s3SecretKey, ok := parameters["s3secretkey"]; ok {
		params.S3SecretKey = fmt.Sprint(s3SecretKey)
	}

	if s3Endpoint, ok := parameters["s3endpoint"]; ok {
		params.S3Endpoint = fmt.Sprint(s3Endpoint)
	}

	if s3RootDirectory, ok := parameters["s3rootdirectory"]; ok {
		params.S3RootDirectory = fmt.Sprint(s3RootDirectory)
	}

	if duration, ok := parameters["signedurlDuration"]; ok {
		if d, err := time.ParseDuration(fmt.Sprint(duration)); err == nil {
			params.SignedURLDuration = d
		}
	}

	return params, nil
}

// Name returns the human-readable name of the driver
func (d *driver) Name() string {
	logrus.Info("Name called")
	return driverName
}

// GetContent retrieves the content stored at "path" as a []byte.
func (d *driver) GetContent(ctx context.Context, path string) ([]byte, error) {
	logrus.Infof("GetContent called for path=%s", path)
	
	// Check if this is a manifest path
	if d.isManifestPath(path) {
		// Check if this is a tag link request (ends with current/link)
		if strings.HasSuffix(path, "/link") {
			return d.getManifestDigest(ctx, path)
		}
		return d.getManifestContent(ctx, path)
	}
	
	// Check if this is a layer link path
	if d.isLayerPath(path) && strings.HasSuffix(path, "/link") {
		digest, err := d.extractDigestFromLayerPath(path)
		if err != nil {
			logrus.Errorf("GetContent: failed to extract digest from layer path %s: %v", path, err)
			return nil, err
		}
		logrus.Infof("GetContent: returning digest %s for layer link path", digest)
		return []byte(digest), nil
	}
	
	// For blob paths, first check if this blob is actually a manifest in the database
	if d.isBlobPath(path) {
		digest, err := d.extractDigestFromBlobPath(path)
		if err != nil {
			logrus.Warnf("GetContent: failed to extract digest from blob path %s: %v", path, err)
		} else {
			// Check if this digest exists in the manifest table
			manifestBytes, err := d.getBlobAsManifestFromDB(ctx, digest)
			if err == nil && manifestBytes != nil {
				logrus.Infof("GetContent: found blob %s as manifest in database, returning manifest bytes", digest)
				return manifestBytes, nil
			}
			if err != nil && !isPathNotFoundError(err) {
				logrus.Warnf("GetContent: error checking manifest table for digest %s: %v", digest, err)
			}
		}
	}
	
	// For blobs not found in manifest table, get content via signed URL
	rc, err := d.Reader(ctx, path, 0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	return io.ReadAll(rc)
}

// PutContent stores the []byte content at a location designated by "path".
func (d *driver) PutContent(ctx context.Context, path string, content []byte) error {
	logrus.Infof("PutContent called for path=%s contentSize=%d", path, len(content))
	// For Quay integration, we typically don't allow direct puts
	// This would be handled by Quay's existing blob storage logic
	return storagedriver.ErrUnsupportedMethod{DriverName: driverName}
}

// Reader retrieves an io.ReadCloser for the content stored at "path" with a given byte offset.
func (d *driver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	logrus.Infof("Reader called for path=%s offset=%d", path, offset)
	logrus.Infof("%s", getCallTrace())
	if offset < 0 {
		return nil, storagedriver.InvalidOffsetError{Path: path, Offset: offset, DriverName: driverName}
	}

	// Check if this is a manifest path
	if d.isManifestPath(path) {
		content, err := d.getManifestContent(ctx, path)
		if err != nil {
			return nil, err
		}
		
		if offset >= int64(len(content)) {
			return io.NopCloser(strings.NewReader("")), nil
		}
		
		return io.NopCloser(bytes.NewReader(content[offset:])), nil
	}

	// For blob paths, first check if this blob is actually a manifest in the database
	if d.isBlobPath(path) {
		digest, err := d.extractDigestFromBlobPath(path)
		if err != nil {
			logrus.Warnf("Reader: failed to extract digest from blob path %s: %v", path, err)
		} else {
			// Check if this digest exists in the manifest table
			manifestBytes, err := d.getBlobAsManifestFromDB(ctx, digest)
			if err == nil && manifestBytes != nil {
				logrus.Infof("Reader: found blob %s as manifest in database, returning manifest content", digest)
				
				if offset >= int64(len(manifestBytes)) {
					return io.NopCloser(strings.NewReader("")), nil
				}
				
				return io.NopCloser(bytes.NewReader(manifestBytes[offset:])), nil
			}
			if err != nil && !isPathNotFoundError(err) {
				logrus.Warnf("Reader: error checking manifest table for digest %s: %v", digest, err)
			}
		}
	}

	// For blobs not found in manifest table, get signed URL and create HTTP reader
	signedURL, err := d.getBlobSignedURL(ctx, path)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", signedURL, nil)
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, storagedriver.PathNotFoundError{Path: path, DriverName: driverName}
	}

	return resp.Body, nil
}

// Writer returns a FileWriter which will store the content written to it at the location designated by "path"
func (d *driver) Writer(ctx context.Context, path string, append bool) (storagedriver.FileWriter, error) {
	logrus.Infof("Writer called for path=%s append=%v", path, append)
	// For Quay integration, we typically don't allow direct writes
	// This would be handled by Quay's existing blob storage logic
	return nil, storagedriver.ErrUnsupportedMethod{DriverName: driverName}
}

// Stat retrieves the FileInfo for the given path
func (d *driver) Stat(ctx context.Context, path string) (storagedriver.FileInfo, error) {
	logrus.Infof("Stat called for path=%s", path)
	logrus.Infof("%s", getCallTrace())

	// Health check calls with root path "/" - this is expected and should succeed
	if path == "/" {
		logrus.Info("quaystorage: Health check root path detected, returning success")
		return storagedriver.FileInfoInternal{
			FileInfoFields: storagedriver.FileInfoFields{
				Path:    path,
				Size:    0,
				ModTime: time.Now(),
				IsDir:   true,
			},
		}, nil
	}

	if d.isManifestPath(path) {
		logrus.Infof("quaystorage: Detected manifest path: %s", path)
		return d.statManifest(ctx, path)
	}
	
	// For blob paths, first check if this blob is actually a manifest in the database
	if d.isBlobPath(path) {
		digest, err := d.extractDigestFromBlobPath(path)
		if err != nil {
			logrus.Warnf("Stat: failed to extract digest from blob path %s: %v", path, err)
		} else {
			// Check if this digest exists in the manifest table
			manifestInfo, err := d.statBlobAsManifest(ctx, path, digest)
			if err == nil && manifestInfo != nil {
				logrus.Infof("Stat: found blob %s as manifest in database, returning manifest stats", digest)
				return manifestInfo, nil
			}
			if err != nil && !isPathNotFoundError(err) {
				logrus.Warnf("Stat: error checking manifest table for digest %s: %v", digest, err)
			}
		}
	}
	
	logrus.Infof("quaystorage: Detected blob path: %s", path)
	return d.statBlob(ctx, path)
}

// List returns a list of the objects that are direct descendants of the given path
func (d *driver) List(ctx context.Context, path string) ([]string, error) {
	logrus.Infof("List called for path=%s", path)
	// Implementation would depend on Quay's specific database schema
	// This is a simplified version
	return nil, storagedriver.ErrUnsupportedMethod{DriverName: driverName}
}

// Move moves an object stored at sourcePath to destPath
func (d *driver) Move(ctx context.Context, sourcePath string, destPath string) error {
	logrus.Infof("Move called from sourcePath=%s to destPath=%s", sourcePath, destPath)
	// For Quay integration, moves are typically not supported directly
	return storagedriver.ErrUnsupportedMethod{DriverName: driverName}
}

// Delete recursively deletes all objects stored at "path" and its subpaths
func (d *driver) Delete(ctx context.Context, path string) error {
	logrus.Infof("Delete called for path=%s", path)
	// For Quay integration, deletes would be handled by Quay's logic
	return storagedriver.ErrUnsupportedMethod{DriverName: driverName}
}

// RedirectURL returns a URL which the client may use to retrieve the content stored at path
func (d *driver) RedirectURL(r *http.Request, path string) (string, error) {
	logrus.Infof("RedirectURL called for path=%s", path)
	// For blobs, return the signed S3 URL
	if !d.isManifestPath(path) {
		return d.getBlobSignedURL(r.Context(), path)
	}
	
	// For manifests, we don't redirect as we serve them directly
	return "", nil
}

// Walk traverses a filesystem defined within driver, starting from the given path
func (d *driver) Walk(ctx context.Context, path string, f storagedriver.WalkFn, options ...func(*storagedriver.WalkOptions)) error {
	logrus.Infof("Walk called for path=%s", path)
	// Implementation would depend on Quay's specific requirements
	return storagedriver.ErrUnsupportedMethod{DriverName: driverName}
}

// Helper methods

// getCallTrace returns a formatted call stack trace
func getCallTrace() string {
	var trace []string
	trace = append(trace, "\n  Call Stack Trace:")
	
	for i := 1; i < 10; i++ { // Limit to 10 stack frames
		pc, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		function := runtime.FuncForPC(pc).Name()
		
		// Stop at main or test functions to avoid too much noise
		if strings.Contains(function, "main.") || strings.Contains(function, "testing.") {
			break
		}
		
		// Shorten file paths to just the filename
		parts := strings.Split(file, "/")
		filename := parts[len(parts)-1]
		
		// Shorten function names by removing package path
		funcParts := strings.Split(function, "/")
		shortFunction := funcParts[len(funcParts)-1]
		// Remove parentheses and parameters from function signature
		if idx := strings.Index(shortFunction, "("); idx != -1 {
			shortFunction = shortFunction[:idx]
		}
		
		trace = append(trace, fmt.Sprintf("    [%d] %s:%d -> %s()", i, filename, line, shortFunction))
	}
	
	if len(trace) == 1 { // Only header was added
		return "\n  Call Stack Trace: <empty>"
	}
	
	return strings.Join(trace, "\n")
}

// isManifestPath determines if a path refers to a manifest
func (d *driver) isManifestPath(path string) bool {
	logrus.Infof("isManifestPath called for path=%s", path)
	// Manifest paths typically contain "manifests" in the path
	// TODO: potential issue with nested repos or repos with name _manifest
	return strings.Contains(path, "/_manifests/")
}

// isBlobPath determines if a path refers to a blob
func (d *driver) isBlobPath(path string) bool {
	return strings.Contains(path, "/blobs/")
}

// isLayerPath determines if a path refers to a layer
func (d *driver) isLayerPath(path string) bool {
	return strings.Contains(path, "/_layers/")
}

// extractDigestFromLayerPath extracts the full digest from a layer path
// Example: "/docker/registry/v2/repositories/admin/redis/_layers/sha256/92ef0a47908bfd208020e28a3f34eb6cf2f8698982a6a8b0a4a725842b148668/link"
// Returns: "sha256:92ef0a47908bfd208020e28a3f34eb6cf2f8698982a6a8b0a4a725842b148668"
func (d *driver) extractDigestFromLayerPath(path string) (string, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	
	// Find the layer hash - typically after "_layers/sha256/"
	for i, part := range parts {
		if part == "_layers" && i+2 < len(parts) {
			// parts[i+1] is algorithm (sha256), parts[i+2] is full hash
			algorithm := parts[i+1]
			hash := parts[i+2]
			digest := algorithm + ":" + hash
			logrus.Infof("extractDigestFromLayerPath: extracted digest=%s from path=%s", digest, path)
			return digest, nil
		}
	}
	
	return "", fmt.Errorf("invalid layer path format: %s", path)
}

// extractDigestFromBlobPath extracts the full digest from a blob path
// Example: "/docker/registry/v2/blobs/sha256/e7/e7cc1f0e4e4f930828d743838a7fb4c5f3fc21f769f41a9fcd06ee28242a24d4/data"
// Returns: "sha256:e7cc1f0e4e4f930828d743838a7fb4c5f3fc21f769f41a9fcd06ee28242a24d4"
func (d *driver) extractDigestFromBlobPath(path string) (string, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	
	// Find the blob hash - typically after "blobs/sha256/xx/"
	for i, part := range parts {
		if part == "blobs" && i+3 < len(parts) {
			// parts[i+1] is algorithm (sha256), parts[i+2] is prefix, parts[i+3] is full hash
			algorithm := parts[i+1]
			hash := parts[i+3]
			digest := algorithm + ":" + hash
			logrus.Infof("extractDigestFromBlobPath: extracted digest=%s from path=%s", digest, path)
			return digest, nil
		}
	}
	
	return "", fmt.Errorf("invalid blob path format: %s", path)
}

// getBlobAsManifestFromDB checks if a blob digest exists in the manifest table and returns manifest bytes
func (d *driver) getBlobAsManifestFromDB(ctx context.Context, digest string) ([]byte, error) {
	logrus.Infof("getBlobAsManifestFromDB: checking if digest %s exists in manifest table", digest)
	
	query := `
		SELECT manifest_bytes 
		FROM manifest 
		WHERE digest = $1
		LIMIT 1
	`
	
	var manifestBytes []byte
	err := d.db.QueryRowContext(ctx, query, digest).Scan(&manifestBytes)
	if err != nil {
		if err == sql.ErrNoRows {
			logrus.Infof("getBlobAsManifestFromDB: digest %s not found in manifest table", digest)
			return nil, storagedriver.PathNotFoundError{Path: digest, DriverName: driverName}
		}
		logrus.Errorf("getBlobAsManifestFromDB: database query failed for digest %s: %v", digest, err)
		return nil, err
	}

	logrus.Infof("getBlobAsManifestFromDB: found manifest for digest %s (size: %d bytes)", digest, len(manifestBytes))
	return manifestBytes, nil
}

// isPathNotFoundError checks if an error is a PathNotFoundError
func isPathNotFoundError(err error) bool {
	_, ok := err.(storagedriver.PathNotFoundError)
	return ok
}

// statBlobAsManifest checks if a blob digest exists in the manifest table and returns manifest file info
func (d *driver) statBlobAsManifest(ctx context.Context, path, digest string) (storagedriver.FileInfo, error) {
	logrus.Infof("statBlobAsManifest: checking if digest %s exists in manifest table", digest)
	
	query := `
		SELECT LENGTH(manifest_bytes) as size
		FROM manifest 
		WHERE digest = $1
		LIMIT 1
	`
	
	var size int64
	err := d.db.QueryRowContext(ctx, query, digest).Scan(&size)
	if err != nil {
		if err == sql.ErrNoRows {
			logrus.Infof("statBlobAsManifest: digest %s not found in manifest table", digest)
			return nil, storagedriver.PathNotFoundError{Path: path, DriverName: driverName}
		}
		logrus.Errorf("statBlobAsManifest: database query failed for digest %s: %v", digest, err)
		return nil, err
	}

	logrus.Infof("statBlobAsManifest: found manifest for digest %s (size: %d bytes)", digest, size)
	
	return storagedriver.FileInfoInternal{
		FileInfoFields: storagedriver.FileInfoFields{
			Path:    path,
			Size:    size,
			ModTime: time.Now(), // Could be fetched from DB if timestamp is stored
			IsDir:   false,
		},
	}, nil
}

// queryManifestDigestFromDB queries the database to get the manifest digest
func (d *driver) queryManifestDigestFromDB(ctx context.Context, path string) (string, error) {
	// Parse the path to extract org, repository, and tag/digest using Quay-specific parsing
	org, repo, ref, err := d.parseQuayManifestPath(path)
	if err != nil {
		return "", err
	}

	// In Quay database, repository name is stored as "org/repo" format
	fullRepoName := org + "/" + repo
	
	// Check if this is a revision path (digest) or tag path
	isRevisionPath := strings.Contains(path, "/_manifests/revisions/")
	
	var query string
	var queryParam string
	
	if isRevisionPath {
		// For revision paths, we have a digest - query directly by digest
		// The ref will be something like "sha256/e7cc1f0e4e4f930828d743838a7fb4c5f3fc21f769f41a9fcd06ee28242a24d4"
		// We need to reconstruct the full digest format "sha256:..."
		digest := strings.Replace(ref, "/", ":", 1)
		logrus.Infof("queryManifestDigestFromDB: revision path detected, using digest=%s for repo=%s", digest, fullRepoName)
		
		query = `
			SELECT m.digest 
			FROM manifest m
			INNER JOIN repository r ON m.repository_id = r.id
			WHERE r.name = $1 AND m.digest = $2
			LIMIT 1
		`
		queryParam = digest
	} else {
		// For tag paths, query by tag name
		logrus.Infof("queryManifestDigestFromDB: tag path detected, using tag=%s for repo=%s", ref, fullRepoName)
		
		query = `
			SELECT m.digest 
			FROM manifest m
			INNER JOIN repository r ON m.repository_id = r.id
			LEFT JOIN tag t ON t.manifest_id = m.id AND t.repository_id = r.id
			WHERE r.name = $1 AND t.name = $2
			  AND t.hidden = false AND t.lifetime_end_ms IS NULL
			LIMIT 1
		`
		queryParam = ref
	}
	
	logrus.Infof("queryManifestDigestFromDB: executing query for fullRepoName=%s param=%s", fullRepoName, queryParam)
	
	// Log the actual query with parameters for debugging
	logrus.Infof("queryManifestDigestFromDB: SQL query: %s", strings.ReplaceAll(strings.ReplaceAll(query, "\n", " "), "\t", ""))
	logrus.Infof("queryManifestDigestFromDB: Query parameters: $1='%s' $2='%s'", fullRepoName, queryParam)
	
	var digest string
	err = d.db.QueryRowContext(ctx, query, repo, queryParam).Scan(&digest)
	if err != nil {
		if err == sql.ErrNoRows {
			logrus.Warnf("queryManifestDigestFromDB: manifest not found for fullRepoName=%s param=%s", fullRepoName, queryParam)
			return "", storagedriver.PathNotFoundError{Path: path, DriverName: driverName}
		}
		logrus.Errorf("queryManifestDigestFromDB: database query failed for fullRepoName=%s param=%s: %v", fullRepoName, queryParam, err)
		return "", err
	}

	logrus.Infof("queryManifestDigestFromDB: successfully retrieved digest for fullRepoName=%s param=%s: %s", fullRepoName, queryParam, digest)
	return digest, nil
}

// queryManifestContentFromDB queries the database to get the manifest content bytes
func (d *driver) queryManifestContentFromDB(ctx context.Context, path string) ([]byte, error) {
	// Parse the path to extract org, repository, and tag/digest using Quay-specific parsing
	org, repo, ref, err := d.parseQuayManifestPath(path)
	if err != nil {
		return nil, err
	}

	// In Quay database, repository name is stored as "org/repo" format
	fullRepoName := org + "/" + repo
	
	// Check if this is a revision path (digest) or tag path
	isRevisionPath := strings.Contains(path, "/_manifests/revisions/")
	
	var query string
	var queryParam string
	
	if isRevisionPath {
		// For revision paths, we have a digest - query directly by digest
		// The ref will be something like "sha256/e7cc1f0e4e4f930828d743838a7fb4c5f3fc21f769f41a9fcd06ee28242a24d4"
		// We need to reconstruct the full digest format "sha256:..."
		digest := strings.Replace(ref, "/", ":", 1)
		logrus.Infof("queryManifestContentFromDB: revision path detected, using digest=%s for repo=%s", digest, fullRepoName)
		
		query = `
			SELECT m.manifest_bytes 
			FROM manifest m
			INNER JOIN repository r ON m.repository_id = r.id
			WHERE r.name = $1 AND m.digest = $2
			LIMIT 1
		`
		queryParam = digest
	} else {
		// For tag paths, query by tag name
		logrus.Infof("queryManifestContentFromDB: tag path detected, using tag=%s for repo=%s", ref, fullRepoName)
		
		query = `
			SELECT m.manifest_bytes 
			FROM manifest m
			INNER JOIN repository r ON m.repository_id = r.id
			LEFT JOIN tag t ON t.manifest_id = m.id AND t.repository_id = r.id
			WHERE r.name = $1 AND t.name = $2
			  AND t.hidden = false AND t.lifetime_end_ms IS NULL
			LIMIT 1
		`
		queryParam = ref
	}
	
	logrus.Infof("queryManifestContentFromDB: executing query for fullRepoName=%s param=%s", fullRepoName, queryParam)
	
	// Log the actual query with parameters for debugging
	logrus.Infof("queryManifestContentFromDB: SQL query: %s", strings.ReplaceAll(strings.ReplaceAll(query, "\n", " "), "\t", ""))
	logrus.Infof("queryManifestContentFromDB: Query parameters: $1='%s' $2='%s'", fullRepoName, queryParam)
	
	var manifestBytes []byte
	err = d.db.QueryRowContext(ctx, query, repo, queryParam).Scan(&manifestBytes)
	if err != nil {
		if err == sql.ErrNoRows {
			logrus.Warnf("queryManifestContentFromDB: manifest not found for fullRepoName=%s param=%s", fullRepoName, queryParam)
			return nil, storagedriver.PathNotFoundError{Path: path, DriverName: driverName}
		}
		logrus.Errorf("queryManifestContentFromDB: database query failed for fullRepoName=%s param=%s: %v", fullRepoName, queryParam, err)
		return nil, err
	}

	logrus.Infof("queryManifestContentFromDB: successfully retrieved manifest for fullRepoName=%s param=%s (size: %d bytes)", fullRepoName, queryParam, len(manifestBytes))
	logrus.Infof("queryManifestContentFromDB: FULL RESPONSE - manifest content: %s", string(manifestBytes))
	return manifestBytes, nil
}

// getManifestContent fetches manifest data from the database
func (d *driver) getManifestContent(ctx context.Context, path string) ([]byte, error) {
	logrus.Infof("getManifestContent called for path=%s", path)
	return d.queryManifestContentFromDB(ctx, path)
}

// getManifestDigest fetches manifest digest from the database for tag link requests
func (d *driver) getManifestDigest(ctx context.Context, path string) ([]byte, error) {
	logrus.Infof("getManifestDigest called for path=%s", path)
	digest, err := d.queryManifestDigestFromDB(ctx, path)
	if err != nil {
		return nil, err
	}
	return []byte(digest), nil
}

// getBlobSignedURL generates an S3 signed URL for blob access
func (d *driver) getBlobSignedURL(ctx context.Context, path string) (string, error) {
	logrus.Infof("getBlobSignedURL called for path=%s", path)

	// Parse the path to extract blob information
	blobID, err := d.parseBlobPath(path)
	if err != nil {
		logrus.Errorf("getBlobSignedURL: failed to parse blob path %s: %v", path, err)
		return "", err
	}
	
	logrus.Infof("getBlobSignedURL: extracted blobID=%s from path=%s", blobID, path)

	// Construct S3 storage path directly: ${rootdirectory}/sha256/<first-two-chars>/<full-hash>
	// Example: /quay/registry/sha256/96/92ef0a47908bfd208020e28a3f34eb6cf2f8698982a6a8b0a4a725842b148668
	
	// Extract algorithm and hash from blobID (format: "sha256:hash")

	algorithm :=  "sha256" // e.g., "sha256"
	hash :=  blobID      // e.g., "92ef0a47908bfd208020e28a3f34eb6cf2f8698982a6a8b0a4a725842b148668"
	
	if len(hash) < 2 {
		return "", fmt.Errorf("hash too short: %s", hash)
	}
	
	// Get first two characters of hash for directory structure
	prefix := hash[:2]
	
	// Construct the storage path using configurable root directory
	storagePath := fmt.Sprintf("%s/%s/%s/%s", d.parameters.S3RootDirectory, algorithm, prefix, hash)
	
	logrus.Infof("getBlobSignedURL: constructed storage path=%s for blobID=%s", storagePath, blobID)

	// Generate signed URL for S3 object
	logrus.Infof("getBlobSignedURL: generating signed URL for bucket=%s key=%s duration=%v", 
		d.parameters.S3Bucket, storagePath, d.parameters.SignedURLDuration)
		
	req, _ := d.s3Client.GetObjectRequest(&s3.GetObjectInput{
		Bucket: aws.String(d.parameters.S3Bucket),
		Key:    aws.String(storagePath),
	})

	signedURL, err := req.Presign(d.parameters.SignedURLDuration)
	if err != nil {
		logrus.Errorf("getBlobSignedURL: failed to generate signed URL for path=%s storagePath=%s: %v", path, storagePath, err)
		return "", fmt.Errorf("failed to generate signed URL: %w", err)
	}

	logrus.Infof("getBlobSignedURL: successfully generated signed URL for path=%s (URL length: %d chars)", path, len(signedURL))
	return signedURL, nil
}

// parseQuayManifestPath extracts organization, repository, and tag/digest from a Quay manifest path
// Example tag path: "/docker/registry/v2/repositories/admin/redis/_manifests/tags/latest/current/link"
// Returns: org="admin", repo="redis", ref="latest"
// Example revision path: "/docker/registry/v2/repositories/admin/redis/_manifests/revisions/sha256/e7cc1f0e4e4f930828d743838a7fb4c5f3fc21f769f41a9fcd06ee28242a24d4/link"
// Returns: org="admin", repo="redis", ref="sha256/e7cc1f0e4e4f930828d743838a7fb4c5f3fc21f769f41a9fcd06ee28242a24d4"
func (d *driver) parseQuayManifestPath(path string) (org, repo, ref string, err error) {
	logrus.Infof("parseQuayManifestPath called for path=%s", path)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	
	// Find "repositories" index
	repositoriesIndex := -1
	for i, part := range parts {
		if part == "repositories" {
			repositoriesIndex = i
			break
		}
	}
	
	if repositoriesIndex == -1 {
		return "", "", "", fmt.Errorf("invalid manifest path - no 'repositories' found: %s", path)
	}
	
	// Find "_manifests" index
	manifestIndex := -1
	for i, part := range parts {
		if part == "_manifests" {
			manifestIndex = i
			break
		}
	}
	
	if manifestIndex == -1 {
		return "", "", "", fmt.Errorf("invalid manifest path - no '_manifests' found: %s", path)
	}
	
	// Ensure we have enough parts after repositories and before _manifests
	if repositoriesIndex+1 >= manifestIndex {
		return "", "", "", fmt.Errorf("invalid manifest path - insufficient parts between repositories and _manifests: %s", path)
	}
	
	// Extract org and repo from the parts between "repositories" and "_manifests"
	repoParts := parts[repositoriesIndex+1:manifestIndex]
	if len(repoParts) < 2 {
		return "", "", "", fmt.Errorf("invalid manifest path - expected at least org/repo format: %s", path)
	}
	
	org = repoParts[0]
	repo = strings.Join(repoParts[1:], "/") // Join remaining parts as repo can contain slashes
	
	// Extract tag or digest from the manifest path
	// Expected format after _manifests: tags/<tag>/current/link or revisions/<algorithm>/<hash>/link
	if manifestIndex+1 < len(parts) && parts[manifestIndex+1] == "tags" {
		if manifestIndex+2 < len(parts) {
			ref = parts[manifestIndex+2]
		} else {
			return "", "", "", fmt.Errorf("invalid manifest path - no tag found after 'tags': %s", path)
		}
	} else if manifestIndex+1 < len(parts) && parts[manifestIndex+1] == "revisions" {
		// For revisions, we need both algorithm and hash: revisions/sha256/e7cc1f0e.../link
		if manifestIndex+3 < len(parts) {
			algorithm := parts[manifestIndex+2]
			hash := parts[manifestIndex+3]
			ref = algorithm + "/" + hash
		} else {
			return "", "", "", fmt.Errorf("invalid manifest path - incomplete digest after 'revisions': %s", path)
		}
	} else {
		return "", "", "", fmt.Errorf("invalid manifest path - expected 'tags' or 'revisions' after '_manifests': %s", path)
	}
	
	return org, repo, ref, nil
}

// parseManifestPath extracts repository and reference from manifest path
func (d *driver) parseManifestPath(path string) (repo, ref string, err error) {
	logrus.Infof("parseManifestPath called for path=%s", path)
	// Example path: /docker/registry/v2/repositories/myrepo/_manifests/tags/latest
	parts := strings.Split(strings.Trim(path, "/"), "/")
	
	manifestIndex := -1
	for i, part := range parts {
		if part == "_manifests" {
			manifestIndex = i
			break
		}
	}
	
	if manifestIndex == -1 || manifestIndex+1 >= len(parts) {
		return "", "", fmt.Errorf("invalid manifest path: %s", path)
	}
	
	// Find "repositories" index to correctly extract repo name
	repositoriesIndex := -1
	for i, part := range parts {
		if part == "repositories" {
			repositoriesIndex = i
			break
		}
	}
	
	if repositoriesIndex == -1 || repositoriesIndex+1 >= manifestIndex {
		return "", "", fmt.Errorf("invalid manifest path: %s", path)
	}
	
	// Repository is everything between "repositories" and "manifests"
	repo = strings.Join(parts[repositoriesIndex+1:manifestIndex], "/")
	ref = parts[manifestIndex+2]
	
	return repo, ref, nil
}

// parseBlobPath extracts blob ID from blob path
func (d *driver) parseBlobPath(path string) (string, error) {
	logrus.Infof("parseBlobPath called for path=%s", path)

	// Example path: /docker/registry/v2/blobs/sha256/ab/abcd1234.../data
	parts := strings.Split(strings.Trim(path, "/"), "/")
	
	logrus.Infof("quaystorage: parseBlobPath - split path into %d parts: %v", len(parts), parts)
	
	// Find the blob hash - typically after "blobs/sha256/xx/"
	for i, part := range parts {
		if part == "blobs" && i+3 < len(parts) {
			// parts[i+1] is algorithm (sha256), parts[i+2] is prefix, parts[i+3] is full hash
			blobID := parts[i+3]
			logrus.Infof("quaystorage: parseBlobPath - found blob ID: %s", blobID)
			return blobID, nil // Return the full hash
		}
	}
	
	logrus.Errorf("quaystorage: parseBlobPath - invalid blob path format: %s (parts: %v)", path, parts)
	
	return "", fmt.Errorf("invalid blob path: %s", path)
}

// statManifest returns file info for a manifest
func (d *driver) statManifest(ctx context.Context, path string) (storagedriver.FileInfo, error) {
	logrus.Infof("statManifest called for path=%s", path)
	content, err := d.getManifestContent(ctx, path)
	if err != nil {
		return nil, err
	}

	return storagedriver.FileInfoInternal{
		FileInfoFields: storagedriver.FileInfoFields{
			Path:    path,
			Size:    int64(len(content)),
			ModTime: time.Now(), // Could be fetched from DB if timestamp is stored
			IsDir:   false,
		},
	}, nil
}

// statBlob returns file info for a blob
func (d *driver) statBlob(ctx context.Context, path string) (storagedriver.FileInfo, error) {
	logrus.Infof("statBlob called for path=%s", path)
	blobID, err := d.parseBlobPath(path)
	if err != nil {
		return nil, err
	}

	algorithm :=  "sha256" // e.g., "sha256"
	hash :=  blobID      // e.g., "92ef0a47908bfd208020e28a3f34eb6cf2f8698982a6a8b0a4a725842b148668"
	
	if len(hash) < 2 {
		return nil, fmt.Errorf("hash too short: %s", hash)
	}
	
	// Get first two characters of hash for directory structure
	prefix := hash[:2]
	
	// Construct the storage path using configurable root directory
	storagePath := fmt.Sprintf("%s/%s/%s/%s", d.parameters.S3RootDirectory, algorithm, prefix, hash)
	
	logrus.Infof("statBlob: constructed storage path=%s for blobID=%s", storagePath, blobID)

	// Use S3 HEAD request to get object metadata
	headInput := &s3.HeadObjectInput{
		Bucket: aws.String(d.parameters.S3Bucket),
		Key:    aws.String(storagePath),
	}

	headOutput, err := d.s3Client.HeadObjectWithContext(ctx, headInput)
	if err != nil {
		logrus.Errorf("statBlob: S3 HEAD request failed for bucket=%s storagePath=%s: %v", d.parameters.S3Bucket, storagePath, err)
		return nil, storagedriver.PathNotFoundError{Path: path, DriverName: driverName}
	}

	// Get size from S3 object metadata
	size := int64(0)
	if headOutput.ContentLength != nil {
		size = *headOutput.ContentLength
	}

	// Get last modified time from S3 object metadata
	modTime := time.Now()
	if headOutput.LastModified != nil {
		modTime = *headOutput.LastModified
	}

	logrus.Infof("statBlob: found S3 object for path=%s (size: %d bytes)", path, size)

	return storagedriver.FileInfoInternal{
		FileInfoFields: storagedriver.FileInfoFields{
			Path:    path,
			Size:    size,
			ModTime: modTime,
			IsDir:   false,
		},
	}, nil
}
