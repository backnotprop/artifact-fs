//go:build darwin

package fskit

import (
	"errors"
	"os"
	"os/exec"
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
		"     " + ExtensionBundleID + "(1.0)\n": extensionUnknown,
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
		// macOS 26.3, after Pando.app was replaced while fskit_agent ran.
		{"/tmp/pando-x", "mount: Probing resource: The operation couldn’t be completed. (com.apple.extensionKit.errorDomain error 2.)\nmount: Unable to invoke task", extensionEnabled, "killall -9 fskit_agent"},
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

func TestCombineState(t *testing.T) {
	list := func(ids ...string) []byte {
		b := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<array>
`
		for _, id := range ids {
			b += "\t<string>" + id + "</string>\n"
		}
		return []byte(b + "</array>\n</plist>\n")
	}
	cases := []struct {
		name string
		pk   extensionStatus
		list []byte
		want extensionStatus
	}{
		// The case seen on macOS 26.3: pluginkit "+", FSKit says no.
		{"plus but not in FSKit's list", extensionEnabled, list("com.apple.fskit.apfs"), extensionDisabled},
		{"plus and in the list", extensionEnabled, list("com.apple.fskit.apfs", ExtensionBundleID), extensionEnabled},
		{"no flag, in the list", extensionUnknown, list(ExtensionBundleID), extensionEnabled},
		{"not registered wins", extensionNotRegistered, list(ExtensionBundleID), extensionNotRegistered},
		{"turned off wins", extensionDisabled, list(ExtensionBundleID), extensionDisabled},
		{"unreadable list leaves pluginkit", extensionEnabled, []byte("garbage"), extensionEnabled},
		{"a longer id is not ours", extensionEnabled, list(ExtensionBundleID + ".other"), extensionDisabled},
	}
	for _, tc := range cases {
		if got := combineState(tc.pk, tc.list); got != tc.want {
			t.Fatalf("%s: combineState = %v, want %v", tc.name, got, tc.want)
		}
	}
	// FSKit could write a binary plist; plutil converts it.
	cmd := exec.Command("/usr/bin/plutil", "-convert", "binary1", "-o", "-", "-")
	cmd.Stdin = strings.NewReader(string(list(ExtensionBundleID)))
	bin, err := cmd.Output()
	if err != nil {
		t.Skip("plutil:", err)
	}
	if got := combineState(extensionEnabled, bin); got != extensionEnabled {
		t.Fatalf("binary plist: %v", got)
	}
}

func TestMountIDForMount(t *testing.T) {
	a := MountIDForMount("main", "/Users/x/mono/projects/web/worktrees/main")
	b := MountIDForMount("main", "/Users/x/mono/projects/api/worktrees/main")
	if a == b || !ValidMountID(a) || !ValidMountID(b) || !strings.HasPrefix(a, "main-") {
		t.Fatalf("two mains: %q %q", a, b)
	}
	if MountIDForMount("main", "/Users/x/mono/projects/web/worktrees/main") != a {
		t.Fatal("not stable")
	}
	for _, name := range []string{"has space", strings.Repeat("x", 80), ".hidden", "a/b", "", "---"} {
		if got := MountIDForMount(name, "/tmp/m"); !ValidMountID(got) {
			t.Fatalf("MountIDForMount(%q) = %q", name, got)
		}
	}
}
