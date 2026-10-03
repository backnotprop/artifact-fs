//go:build darwin

package fskit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the driver's decisions only. Nothing here runs
// /sbin/mount, umount or pluginkit.

func TestParsePluginkit(t *testing.T) {
	cases := map[string]extensionStatus{
		"+    " + ExtensionBundleID + "(1.0)\n": extensionEnabled,
		"-    " + ExtensionBundleID + "(1.0)\n": extensionDisabled,
		"=    " + ExtensionBundleID + "(1.0)\n": extensionEnabled,
		"":                                      extensionNotRegistered,
		"+    com.example.other(2.0)\n":         extensionNotRegistered,
	}
	for out, want := range cases {
		if got := parsePluginkit(out); got != want {
			t.Fatalf("parsePluginkit(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestExplainMountError(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	exit := errors.New("exit status 1")
	cases := []struct {
		mountPoint, output string
		state              extensionStatus
		want               string
	}{
		{filepath.Join(home, "Documents", "repo"), "mount: Operation not permitted", extensionEnabled, "~/Documents, a protected folder"},
		{"/tmp/pando-x", "mount: Operation not permitted", extensionEnabled, "privacy setting, not the extension"},
		{"/tmp/pando-x", "Module ai.plannotator.pando.extension is disabled!", extensionUnknown, "turned off"},
		{"/tmp/pando-x", "mount: whatever", extensionDisabled, "turned off"},
		{"/tmp/pando-x", "mount: Unable to invoke task", extensionUnknown, "Open Pando.app once"},
		{"/tmp/pando-x", "mount: something else", extensionNotRegistered, "Open Pando.app once"},
		{"/tmp/pando-x", "mount: something else", extensionEnabled, "mounting /tmp/pando-x failed"},
	}
	for _, tc := range cases {
		got := explainMountError(tc.mountPoint, tc.output, exit, tc.state).Error()
		if !strings.Contains(got, tc.want) {
			t.Fatalf("explainMountError(%q, %q) = %q, want it to contain %q", tc.mountPoint, tc.output, got, tc.want)
		}
		if strings.Contains(got, "—") {
			t.Fatalf("message has an em dash: %q", got)
		}
	}
}

func TestProtectedFolder(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	if got := protectedFolder(filepath.Join(home, "Desktop", "a", "b")); got != "~/Desktop" {
		t.Fatalf("Desktop = %q", got)
	}
	if got := protectedFolder(filepath.Join(home, "Library", "Mobile Documents", "x")); got != "iCloud Drive" {
		t.Fatalf("iCloud = %q", got)
	}
	if got := protectedFolder(filepath.Join(home, "pando", "projects")); got != "" {
		t.Fatalf("home folder = %q", got)
	}
	if got := protectedFolder(filepath.Join(home, "DocumentsOther")); got != "" {
		t.Fatalf("prefix match = %q", got)
	}
}

func TestMountStateOnPlainFolders(t *testing.T) {
	dir := t.TempDir()
	if mounted, known := mountState(dir); mounted || !known {
		t.Fatalf("a temp folder: mounted %v known %v", mounted, known)
	}
	if mounted, known := mountState(filepath.Join(dir, "missing")); mounted || !known {
		t.Fatalf("a missing folder: mounted %v known %v", mounted, known)
	}
	if mounted, known := mountState("/"); !known || mounted {
		// The root's parent is itself.
		t.Fatalf("root: mounted %v known %v", mounted, known)
	}
}
