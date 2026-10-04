//go:build darwin && !cgo

package fskit

import "time"

type errNoBrowseUnavailable struct{ reason string }

func (e errNoBrowseUnavailable) Error() string { return e.reason }

type errNotLoaded struct{ reason string }

func (e errNotLoaded) Error() string { return "FSKit loading failed: " + e.reason }

// mountNoBrowse needs cgo; without it the volume is mounted by /sbin/mount
// and shows on the Desktop.
func mountNoBrowse(stateDir, mountPoint string, timeout time.Duration) error {
	return errNoBrowseUnavailable{"artifact-fs was built without cgo"}
}
