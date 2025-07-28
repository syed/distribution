package quaystorage

import (
	"context"
	"testing"
	"time"

	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
)

func TestQuayStorageDriverFactory(t *testing.T) {
	factory := &quayStorageDriverFactory{}
	
	// Test with missing required parameters
	_, err := factory.Create(context.Background(), map[string]interface{}{})
	if err == nil {
		t.Error("Expected error for missing parameters")
	}
	
	// Test with minimal valid parameters
	params := map[string]interface{}{
		"databaseurl": "postgres://test:test@localhost/test",
		"s3bucket":    "test-bucket",
	}
	
	// This will fail due to database connection, but should pass parameter validation
	_, err = factory.Create(context.Background(), params)
	if err == nil {
		t.Error("Expected error due to database connection failure")
	}
	
	// Check that it's not a parameter validation error
	if err.Error() == "databaseurl parameter is required" || err.Error() == "s3bucket parameter is required" {
		t.Errorf("Unexpected parameter validation error: %v", err)
	}
}

func TestParseParameters(t *testing.T) {
	tests := []struct {
		name        string
		parameters  map[string]interface{}
		expectError bool
		expected    DriverParameters
	}{
		{
			name:        "missing database URL",
			parameters:  map[string]interface{}{},
			expectError: true,
		},
		{
			name: "missing S3 bucket",
			parameters: map[string]interface{}{
				"databaseurl": "postgres://test:test@localhost/test",
			},
			expectError: true,
		},
		{
			name: "minimal valid config",
			parameters: map[string]interface{}{
				"databaseurl": "postgres://test:test@localhost/test",
				"s3bucket":    "test-bucket",
			},
			expectError: false,
			expected: DriverParameters{
				DatabaseURL:       "postgres://test:test@localhost/test",
				DatabaseDriver:    "postgres",
				S3Region:          "us-east-1",
				S3Bucket:          "test-bucket",
				SignedURLDuration: 15 * time.Minute,
			},
		},
		{
			name: "full config",
			parameters: map[string]interface{}{
				"databaseurl":    "postgres://test:test@localhost/test",
				"databasedriver": "postgres",
				"s3region":       "us-west-2",
				"s3bucket":       "my-bucket",
				"s3accesskey":    "access123",
				"s3secretkey":    "secret456",
				"s3endpoint":     "https://s3.amazonaws.com",
			},
			expectError: false,
			expected: DriverParameters{
				DatabaseURL:       "postgres://test:test@localhost/test",
				DatabaseDriver:    "postgres",
				S3Region:          "us-west-2",
				S3Bucket:          "my-bucket",
				S3AccessKey:       "access123",
				S3SecretKey:       "secret456",
				S3Endpoint:        "https://s3.amazonaws.com",
				SignedURLDuration: 15 * time.Minute,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseParameters(tt.parameters)
			
			if tt.expectError && err == nil {
				t.Error("Expected error but got none")
			}
			
			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			
			if !tt.expectError {
				if result.DatabaseURL != tt.expected.DatabaseURL {
					t.Errorf("Expected DatabaseURL %s, got %s", tt.expected.DatabaseURL, result.DatabaseURL)
				}
				if result.S3Bucket != tt.expected.S3Bucket {
					t.Errorf("Expected S3Bucket %s, got %s", tt.expected.S3Bucket, result.S3Bucket)
				}
			}
		})
	}
}

func TestDriverName(t *testing.T) {
	d := &driver{}
	if d.Name() != "quaystorage" {
		t.Errorf("Expected driver name 'quaystorage', got '%s'", d.Name())
	}
}

func TestIsManifestPath(t *testing.T) {
	d := &driver{}
	
	tests := []struct {
		path     string
		expected bool
	}{
		{"/docker/registry/v2/repositories/myrepo/manifests/latest", true},
		{"/docker/registry/v2/repositories/myrepo/manifests/sha256:abc123", true},
		{"/docker/registry/v2/blobs/sha256/ab/abc123/data", false},
		{"/docker/registry/v2/repositories/myrepo/blobs/sha256:def456", false},
		{"/some/other/path", false},
	}
	
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := d.isManifestPath(tt.path)
			if result != tt.expected {
				t.Errorf("For path %s, expected %v, got %v", tt.path, tt.expected, result)
			}
		})
	}
}

func TestParseManifestPath(t *testing.T) {
	d := &driver{}
	
	tests := []struct {
		path         string
		expectedRepo string
		expectedRef  string
		expectError  bool
	}{
		{
			path:         "/docker/registry/v2/repositories/myrepo/manifests/latest",
			expectedRepo: "myrepo",
			expectedRef:  "latest",
			expectError:  false,
		},
		{
			path:         "/docker/registry/v2/repositories/org/repo/manifests/sha256:abc123",
			expectedRepo: "org/repo",
			expectedRef:  "sha256:abc123",
			expectError:  false,
		},
		{
			path:        "/invalid/path",
			expectError: true,
		},
		{
			path:        "/docker/registry/v2/repositories/myrepo/blobs/sha256:def456",
			expectError: true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			repo, ref, err := d.parseManifestPath(tt.path)
			
			if tt.expectError && err == nil {
				t.Error("Expected error but got none")
			}
			
			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			
			if !tt.expectError {
				if repo != tt.expectedRepo {
					t.Errorf("Expected repo %s, got %s", tt.expectedRepo, repo)
				}
				if ref != tt.expectedRef {
					t.Errorf("Expected ref %s, got %s", tt.expectedRef, ref)
				}
			}
		})
	}
}

func TestParseBlobPath(t *testing.T) {
	d := &driver{}
	
	tests := []struct {
		path         string
		expectedBlob string
		expectError  bool
	}{
		{
			path:         "/docker/registry/v2/blobs/sha256/ab/abc123/data",
			expectedBlob: "abc123",
			expectError:  false,
		},
		{
			path:         "/docker/registry/v2/blobs/sha256/12/123456789/data",
			expectedBlob: "123456789",
			expectError:  false,
		},
		{
			path:        "/invalid/path",
			expectError: true,
		},
		{
			path:        "/docker/registry/v2/repositories/myrepo/manifests/latest",
			expectError: true,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			blob, err := d.parseBlobPath(tt.path)
			
			if tt.expectError && err == nil {
				t.Error("Expected error but got none")
			}
			
			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			
			if !tt.expectError && blob != tt.expectedBlob {
				t.Errorf("Expected blob %s, got %s", tt.expectedBlob, blob)
			}
		})
	}
}

// Note: Full integration tests would require actual database connection
// This test focuses on parameter parsing and path handling logic

func TestUnsupportedMethods(t *testing.T) {
	d := &driver{}
	
	// Test unsupported methods
	err := d.PutContent(context.Background(), "/test/path", []byte("content"))
	if _, ok := err.(storagedriver.ErrUnsupportedMethod); !ok {
		t.Errorf("Expected ErrUnsupportedMethod for PutContent, got %T", err)
	}
	
	_, err = d.Writer(context.Background(), "/test/path", false)
	if _, ok := err.(storagedriver.ErrUnsupportedMethod); !ok {
		t.Errorf("Expected ErrUnsupportedMethod for Writer, got %T", err)
	}
	
	_, err = d.List(context.Background(), "/test/path")
	if _, ok := err.(storagedriver.ErrUnsupportedMethod); !ok {
		t.Errorf("Expected ErrUnsupportedMethod for List, got %T", err)
	}
	
	err = d.Move(context.Background(), "/src", "/dst")
	if _, ok := err.(storagedriver.ErrUnsupportedMethod); !ok {
		t.Errorf("Expected ErrUnsupportedMethod for Move, got %T", err)
	}
	
	err = d.Delete(context.Background(), "/test/path")
	if _, ok := err.(storagedriver.ErrUnsupportedMethod); !ok {
		t.Errorf("Expected ErrUnsupportedMethod for Delete, got %T", err)
	}
	
	err = d.Walk(context.Background(), "/test/path", nil)
	if _, ok := err.(storagedriver.ErrUnsupportedMethod); !ok {
		t.Errorf("Expected ErrUnsupportedMethod for Walk, got %T", err)
	}
}