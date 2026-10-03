# Pando FSKit wire, version 3

This is the contract between the Pando FSKit extension (Swift, inside
Pando.app) and the ArtifactFS daemon (Go, `internal/fskit`). Both halves
implement exactly this. A change to anything here bumps the version.

Version 3 descends from Oak's version 2 wire (Apache 2.0, credited in
`NOTICE`). It keeps Oak's operations, folds `truncate` into `setattr`, adds
`symlink` and `forget`, pages directory listings, and carries file bytes raw.

## 1. Who talks to whom

```
FSKit extension  --XPC-->  broker  --unix socket-->  daemon
(sandboxed)                (pando-broker)            (artifact-fs)
```

- The extension sends each request to the broker's mach service
  `ai.plannotator.pando.broker` as an XPC dictionary:
  `{ "mount": <mount id string>, "frame": <data> }`.
  The broker replies with `{ "frame": <data> }`.
- The broker forwards the frame bytes unchanged to the daemon's socket for
  that mount and sends back the reply frame unchanged. It never parses the
  header, except to match replies to requests by `id` if it multiplexes.
- The broker keeps persistent connections to the daemon. It never connects
  once per request.

### 1.1 Names and paths

| Thing | Value |
| --- | --- |
| FSKit short name | `pando` |
| Extension bundle id | `ai.plannotator.pando.extension` |
| Host app bundle id | `ai.plannotator.pando` |
| Broker mach service | `ai.plannotator.pando.broker` |
| Mount id | 1 to 48 characters from `A-Z a-z 0-9 . _ -`, first character a letter or digit |
| State dir | `$HOME/.pando/fskit/<mount id>/` |
| Daemon socket | `<state dir>/fskit.sock` |
| Volume name file | `<state dir>/volname` (one line, UTF-8; Finder shows it) |
| Mount command | `/sbin/mount -F -t pando -o nobrowse <state dir> <mount point>` |

- The extension receives the state dir as its `FSPathURLResource` (a
  security-scoped URL). The mount id is the state dir's last path component.
- The daemon uses the repo's id as the mount id when it fits the pattern,
  and otherwise `m-` followed by 16 hex digits of the id's SHA-256.
- The broker builds the socket path itself from `$HOME` and the mount id. It
  rejects a mount id that breaks the pattern above, and it never accepts a
  path from the extension.
- Unix socket paths are limited to 104 bytes on macOS, and a long user name
  with a long mount id goes over. The socket file always lives at
  `<state dir>/fskit.sock`. When that path is too long, the daemon binds it
  through a short symlink to the state dir (`/tmp/pfs*/d`, removed after
  binding), and the broker connects relative to the state dir.
- `PANDO_FSKIT_DIR` replaces `$HOME/.pando/fskit` in tests only. The broker
  and the daemon both honour it; a shipped setup never sets it.

## 2. Framing

The same bytes travel on both hops. All integers are big-endian.

```
u32  frame_len    number of bytes after this field (= 4 + header_len + payload length)
u32  header_len   number of bytes of JSON header
[header_len]      header: one UTF-8 JSON object
[rest]            payload: raw bytes
```

- The payload is file data in `read` replies and `write` requests. It is
  empty in every other frame, and the daemon ignores a payload it does not
  expect.
- Limits: `header_len` at most 4 MiB; `frame_len` at most 16 MiB. A peer that
  receives a frame over these limits, or a header that is not a JSON object,
  closes the connection. The broker then answers every request still waiting
  on that connection with `errno` 5 (EIO).

### 2.1 Headers

Request header:

```json
{"id": 41, "op": "lookup", "parent": 2, "name": "README.md"}
```

Reply header:

```json
{"id": 41, "errno": 0, "attr": {"ino": 7, "kind": 1, "mode": 420, "size": 1532, "mtime_ns": 1759400000000000000, "ctime_ns": 1759400000000000000}}
```

- `id` is a u64 the sender chooses. It must be unique among the requests in
  flight on one connection. The reply carries the same `id`.
- `op` is the operation name (section 4). The operation's fields sit beside
  it in the same object.
- `errno` is 0 on success. Otherwise it is a macOS errno (section 6), and the
  result fields are absent.
- Unknown fields are ignored on both sides.

## 3. Concurrency and the version rule

- The daemon reads each connection in one goroutine and runs each request in
  its own goroutine. Replies come back in any order. A client that needs one
  request to finish before another starts waits for the first reply.
- Every request gets exactly one reply.
- `hello` is the first request after `loadResource`. If the versions differ,
  the extension refuses the mount with EPROTONOSUPPORT and logs both builds.
  The daemon replies to `hello` with its own version whatever the extension
  sent; the extension decides.
- The daemon answers an unknown `op` with ENOSYS (78), so a mismatch shows.
- No feature bits. Pando ships both halves together.

## 4. Operations

Inode numbers (`ino`) are u64:

- The root directory is always `ino` 2, which is FSKit's
  `FSItem.Identifier.rootDirectory`. Every other item is 3 or higher. The
  extension uses `ino` values as FSKit item identifiers unchanged and never
  uses 1 (`parentOfRoot`) except where section 4.4 says so.
- A path keeps its number for the life of the mount, also after `forget`. A
  rename moves the number with the item (and with everything under a renamed
  directory). Numbers are never reused, so a new file at the name of a
  removed one gets a new number.
- An `ino` the daemon never issued, or one freed by `forget`, gets ESTALE
  (70).

Kinds: `0` directory, `1` regular file, `2` symbolic link.

Names are single path components as JSON strings: never empty, never `.` or
`..`, no `/`, at most 255 bytes (EINVAL, or ENAMETOOLONG for length).

### 4.1 The attribute record

```json
{"ino": 7, "kind": 1, "mode": 420, "size": 1532, "mtime_ns": 1759400000000000000, "ctime_ns": 1759400000000000000}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `ino` | u64 | the item's number |
| `kind` | u8 | 0 directory, 1 file, 2 symlink |
| `mode` | u32 | permission bits only, `mode & 0o7777` (no type bits) |
| `size` | u64 | bytes; a symlink's size is its target length |
| `mtime_ns` | i64 | modification time, nanoseconds since the Unix epoch |
| `ctime_ns` | i64 | status change time, nanoseconds since the Unix epoch |

The extension fills the rest of `FSItem.Attributes` itself:

- `type` from `kind`, `mode` from `mode`, `size` from `size`.
- `fileID` = `ino` (the root is 2, `.rootDirectory`); `parentID` when known.
- `linkCount` = 1 for every kind.
- `uid` and `gid` = the extension's own `getuid()` and `getgid()`.
- `allocSize` = `size` rounded up to a multiple of 4096.
- `accessTime` = mtime, `modifyTime` = mtime, `changeTime` = ctime,
  `birthTime` = ctime.
- Convert nanoseconds with integer division (`tv_sec = ns / 1e9`,
  `tv_nsec = ns % 1e9`), never through `Double`.

### 4.2 The 15 operations

| # | op | Request fields | Reply fields |
| --- | --- | --- | --- |
| 1 | `hello` | `version` (u32, 3), `build` (string) | `version`, `build`, `max_read` (u32), `max_write` (u32) |
| 2 | `lookup` | `parent`, `name` | `attr` |
| 3 | `getattr` | `ino` | `attr` |
| 4 | `readdir` | `ino`, `cookie` (u64), `verifier` (u64), `max_entries` (u32), `with_attrs` (bool) | `entries`, `verifier`, `eof` |
| 5 | `read` | `ino`, `offset` (i64), `size` (u32) | payload: the bytes |
| 6 | `write` | `ino`, `offset` (i64); payload: the bytes | `written` (u32), `size` (u64, file size after the write) |
| 7 | `readlink` | `ino` | `target` (string) |
| 8 | `create` | `parent`, `name`, `mode` | `attr` |
| 9 | `mkdir` | `parent`, `name`, `mode` | `attr` |
| 10 | `symlink` | `parent`, `name`, `target` | `attr` |
| 11 | `unlink` | `parent`, `name` | nothing |
| 12 | `rmdir` | `parent`, `name` | nothing |
| 13 | `rename` | `parent`, `name`, `newparent`, `newname` | nothing |
| 14 | `setattr` | `ino`, optional `size` (u64), `mode` (u32), `mtime_ns` (i64), `atime_ns` (i64) | `attr`, `consumed` (array of strings) |
| 15 | `forget` | `inos` (array of u64) | nothing |

### 4.3 Rules per operation

**hello.** The daemon's reply today: `version` 3, `max_read` 8388608,
`max_write` 8388608 (8 MiB each).

**lookup, getattr.** ENOENT when the name or item does not exist. ENOTDIR
when `parent` is not a directory. `getattr` on an item that was removed but
not yet forgotten still answers (its last attributes).

**read.** Returns up to `size` bytes from `offset`. A reply shorter than
`size` means end of file; at or past the end the payload is empty. `size`
over `max_read` gets EINVAL, so the extension splits a bigger FSKit read into
several requests and stops at the first short one. EISDIR on a directory.
A read of a file that is not yet downloaded waits for the download; a failed
download is EIO.

**write.** Writes the whole payload at `offset`. A payload over `max_write`
gets EINVAL; the extension splits. EISDIR on a directory. `written` always
equals the payload length on success.

**readlink.** `target` is the link text. EINVAL when the item is not a
symlink.

**create, mkdir, symlink.** The daemon checks the name and creates the item
under one lock. If the name exists in any form, the reply is EEXIST (17), and
nothing changes. FSKit relies on this for `O_CREAT|O_EXCL`, lock files and
`mkdir` locks; for a plain `O_CREAT` the kernel looks the name up again after
EEXIST. `mode` is the full requested mode as FSKit passes it in
`attributes.mode` (the kernel has applied the umask); the daemon keeps
`mode & 0o7777`. A symlink `target` over 4096 bytes gets ENAMETOOLONG.

**unlink, rmdir.** The extension sends `unlink` for files and symlinks and
`rmdir` for directories, choosing from the kind it remembered at lookup; it
does not call `getattr` first. `unlink` on a directory gets EPERM; `rmdir` on
a non-directory gets ENOTDIR; `rmdir` on a non-empty directory gets
ENOTEMPTY. Removal takes the name away at once. The item itself lives on
until `forget`: reads, writes, `getattr` and `setattr` on its `ino` keep
working, so a process that holds the file open is not disturbed.

**rename.** Atomic. If `newname` exists, it is replaced in the same step,
and the replaced item lives on until `forget`, like a removed one. Errors:
ENOENT when the source is missing; EINVAL when a directory would move into
itself or below itself; ENOTDIR when a directory would replace a
non-directory; EISDIR when a non-directory would replace a directory;
ENOTEMPTY when a directory would replace a non-empty directory. Renaming an
item onto its own name succeeds and changes nothing. The extension ignores
FSKit's `overItem`; the daemon does the replace.

**setattr.** Applies the fields present, in this order: `size`, `mode`,
`mtime_ns`. `atime_ns` is accepted and dropped (mounts are `noatime`; atime
reads back as mtime). The reply's `consumed` lists what was applied, from
`"size"`, `"mode"`, `"mtime"`, `"atime"`. The extension copies it into
`FSItem.SetAttributesRequest.consumedAttributes`. An attribute that does not
apply to the item (a mode or time on a symlink, the root, or the synthesized
files in section 5) is left out of `consumed` without an error, as FSKit
asks. `size` on a directory gets EISDIR; `size` on a symlink gets EINVAL.
uid and gid are never sent.

**forget.** Sent from `reclaimItem`; the extension may batch several `ino`s
in one request. For a removed or replaced item, `forget` frees it and its
data. For a live item, it only records that the kernel let go; the number
stays with the path. Unknown numbers, the root, and the synthesized files
are ignored. The reply is always errno 0.

### 4.4 readdir paging

The daemon owns cookies and the verifier.

**Positions.** A directory listing is the list
`[".", "..", child 0, child 1, ...]`, children sorted by name bytes. A
`cookie` is the position to start at, so the first request uses cookie 0.
Each entry carries `next_cookie` = its position + 1, which is the cookie for
the request that continues after it.

**Two modes.**

- `with_attrs: false` (FSKit passed `attributes == nil`): the reply includes
  `.` and `..` at positions 0 and 1, with no `attr`. `.` carries the
  directory's own `ino`; `..` carries the parent's `ino`, and 1
  (`parentOfRoot`) for the root.
- `with_attrs: true`: the reply never includes `.` or `..` (FSKit says not
  to pack them in this mode); it starts at position `max(cookie, 2)`, and
  every entry carries `attr`. Positions mean the same thing in both modes.

**Reply.**

```json
{"id": 9, "errno": 0, "verifier": 7313905467384922741, "eof": false,
 "entries": [
   {"name": "README.md", "ino": 7, "kind": 1, "next_cookie": 3, "attr": {"ino": 7, "kind": 1, "mode": 420, "size": 1532, "mtime_ns": 1759400000000000000, "ctime_ns": 1759400000000000000}},
   {"name": "src", "ino": 8, "kind": 0, "next_cookie": 4, "attr": {"ino": 8, "kind": 0, "mode": 493, "size": 4096, "mtime_ns": 1759400000000000000, "ctime_ns": 1759400000000000000}}
 ]}
```

- At most `max_entries` entries (0 means the daemon's default of 256; the
  daemon caps it at 4096). `eof` is true when the last position is in this
  reply, or when there is nothing left.
- The extension packs entries in order with `packEntry`, using `next_cookie`
  as the entry's `nextCookie`, until `packEntry` returns false or the
  entries run out. It then calls the reply handler with the reply's
  `verifier`. If `eof` is false and the buffer still has room, it sends the
  next request with the last `next_cookie`. If the first entry of a call does
  not fit, that is an error (EINVAL), as in Oak.

**Verifier.** A nonzero u64 that changes whenever the directory might have
changed. The daemon derives it from the snapshot generation and the overlay's
change counter.

- Cookie 0: the daemon lists the directory now and returns the current
  verifier, whatever verifier the request carried.
- Cookie above 0: the request must carry the verifier from an earlier reply
  for this directory. The daemon keeps each listing it served, keyed by
  directory and verifier, for a while, so a listing in progress stays
  consistent while other files change. If that listing is gone and the
  directory's current verifier differs, the reply is errno 10003. The
  extension reports 10003 to FSKit as
  `FSError(.invalidDirectoryCookie)`, and FSKit starts over.
- 10003 is a wire code, not a POSIX errno. It matches NFS's
  `NFSERR_BAD_COOKIE` and stays clear of real errno values.

## 5. Daemon-side behaviour with no wire op

- **`.git` at the root.** The daemon synthesizes a read-only file `.git`
  (`gitdir: <git dir>`), as the FUSE front end does. It appears in the root
  listing; changes to it get EPERM.
- **`.metadata_never_index` at the root.** An empty read-only file, so
  Spotlight does not index (and download) the lazy tree. `nobrowse` is
  reported ignored on FSKit. Changes to it get EPERM.
- **macOS litter is hidden from git.** At mount time the daemon adds these
  lines to `<git dir>/info/exclude`, once, between marker comments:
  `._*`, `.DS_Store`, `.fseventsd/`, `.Spotlight-V100/`, `.Trashes/`,
  `.TemporaryItems/`, `/.metadata_never_index`.
- **Server changes are written through the mount.** On macOS 26 the daemon
  cannot tell the kernel that a file changed underneath it. When the daemon
  applies a server change to a mounted worktree (for example
  `worktrees/main` after a merge), it writes the change through the mount
  path like any program: a temp file named `.pando-wt-<random>` in the same
  directory, then a rename over the target; a delete is an unlink. The
  extension needs no special handling: these arrive as ordinary `create`,
  `write`, `setattr` and `rename` requests.
- **When the daemon writes through.** When the repo's HEAD moves (for
  example an `update-ref` in its git dir after a fetch), the daemon lists
  what changed between the old and new snapshot, skips every path with a
  local edit, writes the rest through the mount, and then switches to the
  new snapshot. The overlay entries the writes made match the new snapshot,
  so the switch removes them and the worktree stays clean. Files over
  64 MiB are skipped; the kernel picks them up on the next open.
- **Read-only mounts.** `artifact-fs add-repo --read-only` mounts a repo
  read-only (`worktrees/main`, references). Every change then gets EROFS
  (30), except the daemon's own write-through changes above. For that
  reason the Mac volume is not mounted with `-o rdonly`: the daemon refuses
  changes itself. On Linux the FUSE mount is read-only in the kernel, which
  needs no write-through because FUSE attribute caches expire in about a
  second.

## 6. Errors

`errno` values are macOS errno numbers.

| errno | Name | When |
| --- | --- | --- |
| 1 | EPERM | `unlink` on a directory; a change to `.git` or `.metadata_never_index` |
| 2 | ENOENT | the name or item does not exist |
| 5 | EIO | download or git failure, a broken connection, anything not listed here |
| 13 | EACCES | the server withheld the content |
| 17 | EEXIST | `create`, `mkdir` or `symlink` on a name that exists |
| 20 | ENOTDIR | a parent is not a directory; `rmdir` on a non-directory; a directory renamed over a non-directory |
| 21 | EISDIR | `read`, `write` or `size` on a directory; a non-directory renamed over a directory |
| 22 | EINVAL | a bad name or field, `readlink` on a non-symlink, a directory moved into itself, `size` over the limit, a malformed request |
| 28 | ENOSPC | the daemon's disk is full |
| 30 | EROFS | a change on a read-only mount |
| 43 | EPROTONOSUPPORT | used by the extension only: the `hello` versions differ |
| 63 | ENAMETOOLONG | a name over 255 bytes; a symlink target over 4096 bytes |
| 66 | ENOTEMPTY | `rmdir` or a rename over a non-empty directory |
| 70 | ESTALE | an `ino` the daemon never issued, or one freed by `forget` |
| 78 | ENOSYS | an unknown `op` |
| 10003 | (wire) bad cookie | section 4.4; the extension maps it to `FSError(.invalidDirectoryCookie)` |

The extension turns every other value into `fs_errorForPOSIXError(errno)`.

## 7. What the extension does (needed for the demo)

From the diligence report, section 6.7. None of this changes the wire.

- Never block in an FSKit callback. Send with
  `xpc_connection_send_message_with_reply` on a concurrent queue, return,
  and call FSKit's reply handler from the XPC reply block.
- Cap requests in flight (64) and give each a deadline (120 s for `read`,
  `write` and `setattr` with a size, 30 s otherwise); a timeout replies EIO.
- The broker may add a `message` field to an error reply it makes itself
  (for example when the daemon is not running); the extension logs it.
- Report the root as `.rootDirectory`; set
  `supportsPersistentObjectIDs = false`; implement `maximumFileSize`.
- Fill attributes as in section 4.1; set `consumedAttributes` from the
  `setattr` reply.
- Pass cookies and the verifier through from the daemon.
- Remember each item's kind, so `removeItem` needs no `getattr`.
- Send `forget` from `reclaimItem`.
- `createLink` (hard links) stays ENOTSUP in the extension.

## 8. Not in version 3

open and close, xattr operations, `fsync` and `synchronize` (FSKit does not
call them on URL volumes), locks (they stay in the kernel), statfs (the
extension's `volumeStatistics` stays synthetic), macOS 27 cache control
(`setCacheState`), passing the socket descriptor past the broker, and a
binary header codec.
