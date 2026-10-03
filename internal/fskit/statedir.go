package fskit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Names fixed by the contract (section 1.1).
const (
	FSShortName       = "pando"
	ExtensionBundleID = "ai.plannotator.pando.extension"
	HostBundleID      = "ai.plannotator.pando"
	BrokerService     = "ai.plannotator.pando.broker"
	SocketName        = "fskit.sock"
	VolumeNameFile    = "volname"

	// maxSocketPath is the usable length of sun_path on macOS (104 bytes
	// with the terminating NUL).
	maxSocketPath = 103
)

var mountIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,47}$`)

// MountIDFor returns the mount id for a repo: its id when that fits the
// contract's pattern, otherwise "m-" and a hash of it.
func MountIDFor(repoID string) string {
	if mountIDPattern.MatchString(repoID) {
		return repoID
	}
	sum := sha256.Sum256([]byte(repoID))
	return "m-" + hex.EncodeToString(sum[:8])
}

// MountIDForMount returns the mount id for a repo mounted at mountPoint: the
// repo name (cut to 39 characters, other characters made '-') and 8 hex
// digits of a hash of the mount point. The state root is one folder per
// person, shared by every daemon, so two repos named the same (every
// worktrees/main, say) must not share a socket.
func MountIDForMount(name, mountPoint string) string {
	if abs, err := filepath.Abs(mountPoint); err == nil {
		mountPoint = abs
	}
	sum := sha256.Sum256([]byte(mountPoint))
	b := []byte(name)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	base := strings.TrimLeft(string(b), "._-")
	if len(base) > 39 {
		base = base[:39]
	}
	if base == "" {
		base = "m"
	}
	return base + "-" + hex.EncodeToString(sum[:4])
}

// ValidMountID reports whether id fits the contract's pattern.
func ValidMountID(id string) bool { return mountIDPattern.MatchString(id) }

// StateRoot is $HOME/.pando/fskit, the folder the broker also derives.
// PANDO_FSKIT_DIR overrides it, for tests only: the broker ignores it.
func StateRoot() (string, error) {
	if dir := os.Getenv("PANDO_FSKIT_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pando", "fskit"), nil
}

// PrepareStateDir creates <state root>/<mount id> with the volume name file
// Finder shows, and returns its path.
func PrepareStateDir(mountID, volumeName string) (string, error) {
	if !ValidMountID(mountID) {
		return "", fmt.Errorf("fskit: invalid mount id %q", mountID)
	}
	root, err := StateRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, mountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := strings.TrimSpace(volumeName)
	if name == "" {
		name = mountID
	}
	if err := os.WriteFile(filepath.Join(dir, VolumeNameFile), []byte(name+"\n"), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// socketListener removes the socket file it was given on Close, whatever path
// was used to bind it.
type socketListener struct {
	*net.UnixListener
	path string
}

func (l *socketListener) Close() error {
	err := l.UnixListener.Close()
	_ = os.Remove(l.path)
	return err
}

// ListenUnix binds the daemon socket at path, replacing a stale one, and
// makes it private to the user. A path over the unix socket limit is bound
// through a short symlink to its folder in /tmp, so a long home folder or
// mount id still works; the socket file itself is created at path.
func ListenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	bindPath := path
	if len(path) > maxSocketPath {
		short, err := os.MkdirTemp("/tmp", "pfs")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(short)
		link := filepath.Join(short, "d")
		if err := os.Symlink(filepath.Dir(path), link); err != nil {
			return nil, err
		}
		bindPath = filepath.Join(link, filepath.Base(path))
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: bindPath, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &socketListener{UnixListener: ln, path: path}, nil
}
