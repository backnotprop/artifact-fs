//go:build darwin && cgo

package fskit

// The volume is mounted with nobrowse, so it is not a drive icon on the
// Desktop or in Finder's sidebar; it is an ordinary folder at its path.
//
// /sbin/mount cannot do it on macOS 26.3: its FSKit path ends in LiveFS's
// mountVolume with an empty options string, so every -o option is dropped
// (Apple's own msdos module loses nobrowse and rdonly the same way; Apple
// DTS, developer forums thread 820931: "it should be" supported). So the
// daemon does /sbin/mount's FSKit steps itself, through FSKit's and LiveFS's
// Objective-C classes, and passes "nobrowse" where /sbin/mount passes "".
// These classes are private: every class and method is looked up at run
// time, and when one is missing mountNoBrowse reports it and the caller runs
// /sbin/mount instead (a visible volume, still a working mount).

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>
#import <objc/runtime.h>
#import <objc/message.h>
#include <dlfcn.h>
#include <stdlib.h>

static NSString *const kProvider = @"com.apple.filesystems.UserFS.FileProvider";

static BOOL has(const char *cls, const char *sel, BOOL meta) {
	Class c = objc_getClass(cls);
	if (!c) return NO;
	return meta ? class_respondsToSelector(object_getClass(c), sel_registerName(sel))
	            : class_respondsToSelector(c, sel_registerName(sel));
}

static char *fail(int step, NSError *e) {
	NSString *s = [NSString stringWithFormat:@"%d|%@ %ld: %@", step, e.domain, (long)e.code, e.localizedDescription];
	return strdup(s.UTF8String);
}

// pando_mount_nobrowse returns NULL when mounted, else "<step>|<message>".
// Step 0: the private classes are missing; 1: load; 2: activate; 3: mount.
static char *pando_mount_nobrowse(const char *shortName, const char *stateDir, const char *mountPoint, double timeout) {
	@autoreleasepool {
		dlopen("/System/Library/Frameworks/FSKit.framework/FSKit", RTLD_NOW);
		dlopen("/System/Library/PrivateFrameworks/LiveFS.framework/LiveFS", RTLD_NOW);
		if (!has("FSPathURLResource", "secureResourceWithURL:readonly:", YES) ||
		    !has("FSTaskOptionsBundle", "bundleForArguments:count:syntaxDictionary:errorHandler:", YES) ||
		    !has("FSClient", "loadResource:shortName:options:synchronousReplyHandler:", NO) ||
		    !has("FSClient", "activateVolume:shortName:options:replyHandler:", NO) ||
		    !has("FSClient", "unloadResource:shortName:options:replyHandler:", NO) ||
		    !has("LiveFSMountClient", "newClientForProvider:", YES) ||
		    !has("LiveFSMountClient", "mountVolume:fileSystem:displayName:provider:domainError:on:how:options:", NO)) {
			return strdup("0|this macOS does not have the FSKit calls the daemon uses for nobrowse");
		}
		NSString *name = @(shortName);
		id res = ((id(*)(id, SEL, id, BOOL))objc_msgSend)(objc_getClass("FSPathURLResource"),
			sel_registerName("secureResourceWithURL:readonly:"), [NSURL fileURLWithPath:@(stateDir)], NO);
		char *none[] = {NULL};
		id opts = ((id(*)(id, SEL, char **, int, id, id))objc_msgSend)(objc_getClass("FSTaskOptionsBundle"),
			sel_registerName("bundleForArguments:count:syntaxDictionary:errorHandler:"), none, 0, @{}, ^(NSError *e) {});
		id client = ((id(*)(id, SEL))objc_msgSend)(objc_getClass("FSClient"), sel_registerName("sharedInstance"));
		if (!res || !opts || !client) {
			return strdup("0|FSKit gave no resource, options or client");
		}
		dispatch_time_t deadline = dispatch_time(DISPATCH_TIME_NOW, (int64_t)(timeout * NSEC_PER_SEC));

		__block NSArray *vols = nil;
		__block NSError *loadErr = nil;
		((void(*)(id, SEL, id, id, id, id))objc_msgSend)(client,
			sel_registerName("loadResource:shortName:options:synchronousReplyHandler:"), res, name, opts,
			^(NSArray *v, NSError *e) { vols = v; loadErr = e; });
		if (loadErr || vols.count == 0) {
			return loadErr ? fail(1, loadErr) : strdup("1|FSKit loaded the resource but added no volume");
		}
		id vol = vols[0];
		id volumeID = [vol valueForKey:@"volumeID"];

		// From here on, a failure must unload the resource: fskitd keeps it
		// loaded otherwise, and the next mount fails with "Resource busy".
		void (^unload)(void) = ^{
			dispatch_semaphore_t done = dispatch_semaphore_create(0);
			((void(*)(id, SEL, id, id, id, id))objc_msgSend)(client,
				sel_registerName("unloadResource:shortName:options:replyHandler:"), res, name, opts,
				^(NSError *e) { dispatch_semaphore_signal(done); });
			dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC));
		};

		dispatch_semaphore_t activated = dispatch_semaphore_create(0);
		__block NSError *activateErr = nil;
		((void(*)(id, SEL, id, id, id, id))objc_msgSend)(client,
			sel_registerName("activateVolume:shortName:options:replyHandler:"), volumeID, name, opts,
			^(NSError *e) { activateErr = e; dispatch_semaphore_signal(activated); });
		if (dispatch_semaphore_wait(activated, deadline) != 0) {
			unload();
			return strdup("2|FSKit did not activate the volume in time");
		}
		if (activateErr) {
			unload();
			return fail(2, activateErr);
		}

		NSString *uuid = [[volumeID valueForKey:@"uuid"] description];
		id volName = [vol valueForKey:@"volumeName"];
		NSString *display = volName ? [volName valueForKey:@"string"] : @"";
		id mounter = ((id(*)(id, SEL, id))objc_msgSend)(objc_getClass("LiveFSMountClient"),
			sel_registerName("newClientForProvider:"), kProvider);
		NSError *mountErr = ((id(*)(id, SEL, id, id, id, id, id, id, uint64_t, id))objc_msgSend)(mounter,
			sel_registerName("mountVolume:fileSystem:displayName:provider:domainError:on:how:options:"),
			uuid, name, display ?: @"", kProvider, nil, @(mountPoint), (uint64_t)0, @"nobrowse");
		if (mountErr) {
			unload();
			return fail(3, mountErr);
		}
		return NULL;
	}
}
*/
import "C"

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unsafe"
)

// errNoBrowseUnavailable means this macOS lacks the FSKit calls; the caller
// mounts with /sbin/mount instead.
type errNoBrowseUnavailable struct{ reason string }

func (e errNoBrowseUnavailable) Error() string { return e.reason }

// errNotLoaded means FSKit refused the resource and added no volume, so a
// /sbin/mount attempt cannot end up mounted twice.
type errNotLoaded struct{ reason string }

func (e errNotLoaded) Error() string { return "FSKit loading failed: " + e.reason }

// mountNoBrowse mounts the FSKit volume for stateDir at mountPoint with
// nobrowse. Like /sbin/mount, it uses the mount point's real path.
func mountNoBrowse(stateDir, mountPoint string, timeout time.Duration) error {
	if real, err := filepath.EvalSymlinks(mountPoint); err == nil {
		mountPoint = real
	}
	cName, cState, cMount := C.CString(FSShortName), C.CString(stateDir), C.CString(mountPoint)
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cState))
	defer C.free(unsafe.Pointer(cMount))
	res := C.pando_mount_nobrowse(cName, cState, cMount, C.double(timeout.Seconds()))
	if res == nil {
		return nil
	}
	msg := C.GoString(res)
	C.free(unsafe.Pointer(res))
	step, detail, _ := strings.Cut(msg, "|")
	switch step {
	case "0":
		return errNoBrowseUnavailable{detail}
	case "1":
		return errNotLoaded{detail}
	}
	what := map[string]string{"1": "loading", "2": "activating", "3": "mounting"}[step]
	return fmt.Errorf("FSKit %s failed: %s", what, detail)
}
