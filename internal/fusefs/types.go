package fusefs

import "context"

// MountedFS matches tigrisfs's interface for mount lifecycle. The FUSE front
// end (Linux) and the FSKit front end (macOS, internal/fskit) both return one.
type MountedFS interface {
	Join(ctx context.Context) error
	Unmount() error
}

// ReaddirEntry holds child metadata, avoiding per-child Getattr or snapshot lookups.
type ReaddirEntry struct {
	Name        string
	Type        string // file, dir, symlink
	Mode        uint32
	ObjectOID   string
	SizeState   string
	SizeBytes   int64
	FromOverlay bool
	MtimeUnixNs int64
	CtimeUnixNs int64
}
