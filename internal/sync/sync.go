package sync

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Backend abstracts a remote storage provider for cloud sync.
type Backend interface {
	// Push uploads data to the remote path.
	Push(ctx context.Context, data []byte) error
	// Pull downloads data from the remote path.
	Pull(ctx context.Context) ([]byte, error)
	// Test verifies the connection and permissions.
	Test(ctx context.Context) error
}

// Config holds the cloud sync configuration.
type Config struct {
	Type     string // "webdav", "s3", "disabled"
	URL      string // WebDAV URL or S3 endpoint
	Bucket   string // S3 bucket name (unused for WebDAV)
	Region   string // S3 region (unused for WebDAV)
	KeyID    string // Username (WebDAV) or Access Key ID (S3)
	Secret   string // Password (WebDAV) or Secret Access Key (S3)
	Path     string // Remote file path
	Auto     bool   // Enable automatic sync
	Interval int    // Auto sync interval in minutes
}

// DefaultPath is the default remote file path for sync.
const DefaultPath = "/ccp-switcher/providers.json"

// NewBackend creates a Backend from the given config.
func NewBackend(cfg Config) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "webdav":
		return NewWebDAV(cfg), nil
	case "s3":
		return NewS3(cfg), nil
	case "", "disabled":
		return nil, fmt.Errorf("sync is disabled")
	default:
		return nil, fmt.Errorf("unknown sync type: %s", cfg.Type)
	}
}

// SyncStatus holds the result of the last sync operation.
type SyncStatus struct {
	LastPushAt  time.Time
	LastPullAt  time.Time
	LastError   string
	LastErrorAt time.Time
}
