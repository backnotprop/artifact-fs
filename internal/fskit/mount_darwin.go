//go:build darwin

package fskit

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// The macOS mount driver, translated from Oak's fskit/mod.rs (Apache 2.0):
// start the daemon socket, run /sbin/mount for the FSKit volume, and turn
// macOS failures into plain instructions.

const (
	mountTimeout  = 60 * time.Second
	watchInterval = 2 * time.Second
	settingsPath  = "System Settings > General > Login Items & Extensions > File System Extensions"
)

// MountOptions is what MountRepo needs to serve one repo.
type MountOptions struct {
	Repo     model.RepoConfig
	Resolver *fusefs.Resolver
	Engine   *fusefs.Engine
	Gate     *fusefs.ReadyGate
	Logger   *slog.Logger
	Build    string
}

// Mount is a live FSKit mount: the daemon socket plus the mounted volume.
// It satisfies fusefs.MountedFS.
type Mount struct {
	srv        *Server
	log        *slog.Logger
	mountPoint string
	stateDir   string
	done       chan struct{}
	closeOnce  sync.Once
	stopWatch  chan struct{}
}

// MountRepo starts the daemon socket for the repo and mounts the Pando FSKit
// volume at the repo's mount path.
func MountRepo(opts MountOptions) (*Mount, error) {
	cfg := opts.Repo
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if os.Geteuid() == 0 {
		return nil, errors.New("do not run the daemon with sudo: macOS runs the Pando file system extension per user, so a mount started as root cannot reach it. Run it again as yourself")
	}
	// Check the extension before running /sbin/mount, so a Mac without
	// Pando.app gets an instruction instead of a failed mount.
	switch extensionState() {
	case extensionNotRegistered:
		return nil, errExtensionMissing("")
	case extensionDisabled:
		return nil, errExtensionDisabled("")
	}
	mountID := MountIDForMount(cfg.Name, cfg.MountPath)
	stateDir, err := PrepareStateDir(mountID, cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("fskit state folder: %w", err)
	}
	if err := EnsureMacOSExcludes(cfg.GitDir); err != nil {
		log.Warn("fskit: could not add macOS patterns to info/exclude", "repo", cfg.Name, "error", err)
	}
	// A volume left over from an earlier daemon run: still mounted, or
	// mounted with nobody serving it, so that stat fails.
	if mounted, known := mountState(cfg.MountPath); mounted || !known {
		_ = unmount(cfg.MountPath)
	}
	if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
		return nil, err
	}
	srv := NewServer(Config{
		Repo:       cfg,
		Resolver:   opts.Resolver,
		Engine:     opts.Engine,
		Gate:       opts.Gate,
		ReadOnly:   cfg.ReadOnly,
		MountPoint: cfg.MountPath,
		Build:      opts.Build,
		Logger:     log,
	})
	ln, err := ListenUnix(filepath.Join(stateDir, SocketName))
	if err != nil {
		return nil, fmt.Errorf("fskit socket: %w", err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil {
			log.Warn("fskit: socket server stopped", "repo", cfg.Name, "error", err)
		}
	}()
	if err := runMount(stateDir, cfg.MountPath); err != nil {
		srv.Close()
		return nil, err
	}
	m := &Mount{
		srv:        srv,
		log:        log,
		mountPoint: cfg.MountPath,
		stateDir:   stateDir,
		done:       make(chan struct{}),
		stopWatch:  make(chan struct{}),
	}
	go m.watch()
	return m, nil
}

// Join waits until the volume is unmounted, by Unmount or from outside
// (Finder's eject, umount).
func (m *Mount) Join(ctx context.Context) error {
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unmount unmounts the volume and stops the daemon socket.
func (m *Mount) Unmount() error {
	err := unmount(m.mountPoint)
	if mounted, _ := mountState(m.mountPoint); err != nil && mounted {
		return err
	}
	m.finish()
	return nil
}

// WriteThrough applies server-side changes through the mount; see
// Server.WriteThrough.
func (m *Mount) WriteThrough(ctx context.Context, changes []Change) error {
	return m.srv.WriteThrough(ctx, changes)
}

func (m *Mount) finish() {
	m.closeOnce.Do(func() {
		close(m.stopWatch)
		m.srv.Close()
		close(m.done)
	})
}

// watch notices a volume unmounted from outside, so the daemon can report
// the repo degraded and mount it again.
func (m *Mount) watch() {
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stopWatch:
			return
		case <-t.C:
			// Only a definite answer counts: a stat that fails is not proof
			// the volume is gone.
			if mounted, known := mountState(m.mountPoint); known && !mounted {
				m.log.Warn("fskit: volume was unmounted", "mount", m.mountPoint)
				m.finish()
				return
			}
		}
	}
}

// mountState reports whether a volume is mounted at path: path is on a
// different device than its parent folder. known is false when path cannot
// be read (a volume nobody serves); a missing path is known and unmounted.
func mountState(path string) (mounted, known bool) {
	var st, parent syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return false, errors.Is(err, syscall.ENOENT)
	}
	if err := syscall.Lstat(filepath.Dir(path), &parent); err != nil {
		return false, false
	}
	return st.Dev != parent.Dev, true
}

// runMount brings up the volume. FSKit path-URL volumes cannot be mounted
// through DiskArbitration, so the daemon calls /sbin/mount itself; only
// standard -o options reach FSKit, and the extension derives the mount id and
// socket from the state folder it receives as its resource.
func runMount(stateDir, mountPoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/sbin/mount", "-F", "-t", FSShortName, "-o", "nobrowse", stateDir, mountPoint)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("mounting %s timed out after %s. Check that Pando.app is installed and the Pando extension is on in %s", mountPoint, mountTimeout, settingsPath)
	}
	return explainMountError(mountPoint, strings.TrimSpace(string(out)), err, extensionState())
}

// explainMountError turns a failed /sbin/mount into a plain instruction.
func explainMountError(mountPoint, output string, err error, state extensionStatus) error {
	detail := fmt.Sprintf("(/sbin/mount: %v: %s)", err, output)
	switch {
	case strings.Contains(output, "Operation not permitted"):
		// mount(2) refused after FSKit set up the volume: a privacy block,
		// not an extension problem.
		if label := protectedFolder(mountPoint); label != "" {
			return fmt.Errorf("macOS does not allow mounting into %s, a protected folder. Mount the worktree somewhere else, for example in a folder under your home folder, or give your terminal Full Disk Access in System Settings > Privacy & Security > Full Disk Access and reopen it %s", label, detail)
		}
		return fmt.Errorf("macOS refused to mount at %s (Operation not permitted). This is a privacy setting, not the extension. Mount somewhere under your home folder, or give your terminal Full Disk Access in System Settings > Privacy & Security > Full Disk Access and reopen it %s", mountPoint, detail)
	case strings.Contains(output, "is disabled") || state == extensionDisabled:
		return errExtensionDisabled(detail)
	case state == extensionEnabled && strings.Contains(output, "extensionKit.errorDomain"):
		// Seen on macOS 26.3 after Pando.app was installed or replaced:
		// fskit_agent still holds the old copy and cannot start it.
		return fmt.Errorf("macOS could not start the Pando extension, which happens when FSKit's agent still holds an older copy of Pando.app. Run killall -9 fskit_agent (it starts again by itself; no sudo), or log out and back in, then try again. Eject any other FSKit volume first, such as an exFAT or FAT drive %s", detail)
	case state == extensionNotRegistered || strings.Contains(output, "Unable to invoke task") || strings.Contains(output, "unknown") || strings.Contains(output, "not supported"):
		return errExtensionMissing(detail)
	}
	return fmt.Errorf("mounting %s failed %s", mountPoint, detail)
}

func errExtensionMissing(detail string) error {
	return errors.New(strings.TrimSpace(fmt.Sprintf("macOS does not list the Pando file system extension. Open Pando.app once so macOS registers it (after replacing the app: pluginkit -a /Applications/Pando.app/Contents/Extensions/PandoExtension.appex), then turn on Pando in %s. To check: pluginkit -m -i %s %s", settingsPath, ExtensionBundleID, detail)))
}

func errExtensionDisabled(detail string) error {
	return errors.New(strings.TrimSpace(fmt.Sprintf("the Pando file system extension is turned off. Turn on Pando in %s (show the list By Category and click the info button; replacing Pando.app turns it off again), then try again %s", settingsPath, detail)))
}

// protectedFolder names the privacy-protected folder that holds path, if any.
func protectedFolder(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	abs := resolveExisting(path)
	for _, p := range []struct{ dir, label string }{
		{filepath.Join(home, "Documents"), "~/Documents"},
		{filepath.Join(home, "Desktop"), "~/Desktop"},
		{filepath.Join(home, "Downloads"), "~/Downloads"},
		{filepath.Join(home, "Library", "Mobile Documents"), "iCloud Drive"},
	} {
		root := resolveExisting(p.dir)
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return p.label
		}
	}
	return ""
}

// resolveExisting resolves symlinks in the longest existing prefix of path.
func resolveExisting(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	tail := ""
	for cur := abs; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, tail)
		}
		if cur == filepath.Dir(cur) {
			return abs
		}
		tail = filepath.Join(filepath.Base(cur), tail)
	}
}

type extensionStatus int

const (
	extensionUnknown extensionStatus = iota
	extensionEnabled
	extensionDisabled
	extensionNotRegistered
)

// extensionState asks macOS whether the extension is registered and on.
// pluginkit says whether it is registered; whether FSKit will mount with it
// is FSKit's own per-person list (fskitEnabledList). pluginkit's "+" alone
// is not enough: on macOS 26.3 a failed switch in System Settings leaves "+"
// while FSKit still refuses ("Module ... is disabled!").
func extensionState() extensionStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/pluginkit", "-m", "-i", ExtensionBundleID).Output()
	var pk extensionStatus
	if err != nil && len(out) == 0 {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return extensionUnknown
		}
		pk = extensionNotRegistered
	} else {
		pk = parsePluginkit(string(out))
	}
	b, err := os.ReadFile(fskitEnabledList())
	if err != nil {
		return pk
	}
	return combineState(pk, b)
}

// fskitEnabledList is the file behind the switch in System Settings: the
// FSKit modules this person turned on. Apple calls it an implementation
// detail (developer forums thread 808594), so a file that cannot be read or
// parsed leaves the answer to pluginkit.
func fskitEnabledList() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Group Containers", "group.com.apple.fskit.settings", "enabledModules.plist")
}

// combineState merges pluginkit's answer with FSKit's list (a plist file).
func combineState(pk extensionStatus, list []byte) extensionStatus {
	if pk == extensionNotRegistered || pk == extensionDisabled {
		return pk
	}
	on, ok := plistHasString(list, ExtensionBundleID)
	switch {
	case !ok:
		return pk
	case on:
		return extensionEnabled
	}
	return extensionDisabled
}

// plistHasString reports whether a plist holds s as a string. ok is false
// when the file is not a plist this can read. FSKit writes the list as XML
// today; a binary plist is converted with plutil.
func plistHasString(b []byte, s string) (found, ok bool) {
	if bytes.HasPrefix(b, []byte("bplist")) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "xml1", "-o", "-", "-")
		cmd.Stdin = bytes.NewReader(b)
		out, err := cmd.Output()
		if err != nil {
			return false, false
		}
		b = out
	}
	if !bytes.Contains(b, []byte("<plist")) {
		return false, false
	}
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(s))
	return bytes.Contains(b, []byte("<string>"+esc.String()+"</string>")), true
}

// parsePluginkit reads `pluginkit -m -i <id>`: a line per plugin with a flag
// first, "+" (chosen) or "-" (turned off); a line with no flag has no choice
// made, which is not proof of either.
func parsePluginkit(out string) extensionStatus {
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.Contains(line, ExtensionBundleID) {
			continue
		}
		switch line[0] {
		case '-', '!':
			return extensionDisabled
		case '+', '=':
			return extensionEnabled
		}
		return extensionUnknown
	}
	return extensionNotRegistered
}

// unmount unmounts the volume, forcing it if a plain umount fails.
func unmount(mountPoint string) error {
	out, err := exec.Command("/sbin/umount", mountPoint).CombinedOutput()
	if err == nil {
		return nil
	}
	msg := string(out)
	if strings.Contains(msg, "not currently mounted") || strings.Contains(msg, "Invalid argument") {
		return nil
	}
	if out, ferr := exec.Command("/usr/sbin/diskutil", "unmount", "force", mountPoint).CombinedOutput(); ferr != nil {
		return fmt.Errorf("unmount %s: %s; forced: %s", mountPoint, strings.TrimSpace(msg), strings.TrimSpace(string(out)))
	}
	return nil
}
