// Package fskit is the macOS front end of ArtifactFS: a unix socket server
// that speaks the Pando FSKit wire (docs/pando-fskit-wire-v3.md) to an FSKit
// extension and serves each operation from the same Resolver and Engine the
// Linux FUSE front end uses.
//
// The framing, the operation set and the mount driver follow Oak's FSKit
// backend (Apache 2.0; see NOTICE), translated to Go and extended to wire
// version 3.
package fskit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// ProtocolVersion is the wire version. The extension refuses to mount when
// its own version differs.
const ProtocolVersion = 3

// I/O and framing limits (section 2 of the contract).
const (
	MaxRead        = 8 << 20
	MaxWrite       = 8 << 20
	maxHeaderLen   = 4 << 20
	maxFrameLen    = 16 << 20
	defaultEntries = 256
	maxEntries     = 4096
)

// Item numbers. The root is FSKit's rootDirectory; 1 is FSKit's parentOfRoot
// and is used only as the ".." entry of the root.
const (
	inoParentOfRoot uint64 = 1
	inoRoot         uint64 = 2
	firstIno        uint64 = 3
)

// Kinds on the wire.
const (
	kindDir     uint8 = 0
	kindFile    uint8 = 1
	kindSymlink uint8 = 2
)

// Errno values are macOS numbers whatever platform the daemon runs on, so the
// contract holds in a Linux test container too.
const (
	errOK              int32 = 0
	errEPERM           int32 = 1
	errENOENT          int32 = 2
	errEIO             int32 = 5
	errEACCES          int32 = 13
	errEEXIST          int32 = 17
	errENOTDIR         int32 = 20
	errEISDIR          int32 = 21
	errEINVAL          int32 = 22
	errEFBIG           int32 = 27
	errENOSPC          int32 = 28
	errEROFS           int32 = 30
	errENOTSUP         int32 = 45
	errENAMETOOLONG    int32 = 63
	errENOTEMPTY       int32 = 66
	errEDQUOT          int32 = 69
	errESTALE          int32 = 70
	errENOSYS          int32 = 78
	errBadCookie       int32 = 10003 // wire code: FSError(.invalidDirectoryCookie)
	maxNameBytes             = 255
	maxSymlinkTargetSz       = 4096
)

// Attr is the wire attribute record (section 4.1).
type Attr struct {
	Ino     uint64 `json:"ino"`
	Kind    uint8  `json:"kind"`
	Mode    uint32 `json:"mode"`
	Size    uint64 `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns"`
}

// DirEntry is one readdir entry (section 4.4).
type DirEntry struct {
	Name       string `json:"name"`
	Ino        uint64 `json:"ino"`
	Kind       uint8  `json:"kind"`
	NextCookie uint64 `json:"next_cookie"`
	Attr       *Attr  `json:"attr,omitempty"`
}

// request is every request header. Fields an operation does not use are zero.
type request struct {
	ID         uint64   `json:"id"`
	Op         string   `json:"op"`
	Version    uint32   `json:"version"`
	Build      string   `json:"build"`
	Ino        uint64   `json:"ino"`
	Parent     uint64   `json:"parent"`
	Name       string   `json:"name"`
	NewParent  uint64   `json:"newparent"`
	NewName    string   `json:"newname"`
	Offset     int64    `json:"offset"`
	Size       *uint64  `json:"size"`
	Mode       *uint32  `json:"mode"`
	MtimeNs    *int64   `json:"mtime_ns"`
	AtimeNs    *int64   `json:"atime_ns"`
	Target     string   `json:"target"`
	Cookie     uint64   `json:"cookie"`
	Verifier   uint64   `json:"verifier"`
	MaxEntries uint32   `json:"max_entries"`
	WithAttrs  bool     `json:"with_attrs"`
	Inos       []uint64 `json:"inos"`
}

// reply is every reply header. Optional fields are pointers so a zero value
// that matters (written 0, an empty listing) still appears.
type reply struct {
	ID       uint64      `json:"id"`
	Errno    int32       `json:"errno"`
	Version  uint32      `json:"version,omitempty"`
	Build    string      `json:"build,omitempty"`
	MaxRead  uint32      `json:"max_read,omitempty"`
	MaxWrite uint32      `json:"max_write,omitempty"`
	Attr     *Attr       `json:"attr,omitempty"`
	Entries  *[]DirEntry `json:"entries,omitempty"`
	Verifier uint64      `json:"verifier,omitempty"`
	EOF      *bool       `json:"eof,omitempty"`
	Written  *uint32     `json:"written,omitempty"`
	Size     *uint64     `json:"size,omitempty"`
	Target   *string     `json:"target,omitempty"`
	Consumed *[]string   `json:"consumed,omitempty"`
}

var errFrameTooLarge = errors.New("fskit: frame over the size limit")

// readFrame reads one frame: u32 frame length, u32 header length, header,
// payload. It returns io.EOF on a clean close at a frame boundary.
func readFrame(r io.Reader) (header []byte, payload []byte, err error) {
	var lens [8]byte
	if _, err := io.ReadFull(r, lens[:4]); err != nil {
		return nil, nil, err
	}
	frameLen := binary.BigEndian.Uint32(lens[:4])
	if frameLen > maxFrameLen {
		return nil, nil, errFrameTooLarge
	}
	if frameLen < 4 {
		return nil, nil, fmt.Errorf("fskit: frame length %d is shorter than its header length field", frameLen)
	}
	if _, err := io.ReadFull(r, lens[4:]); err != nil {
		return nil, nil, unexpected(err)
	}
	headerLen := binary.BigEndian.Uint32(lens[4:])
	if headerLen > maxHeaderLen || headerLen > frameLen-4 {
		return nil, nil, fmt.Errorf("fskit: header length %d does not fit frame length %d", headerLen, frameLen)
	}
	buf := make([]byte, frameLen-4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, nil, unexpected(err)
	}
	return buf[:headerLen], buf[headerLen:], nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// writeFrame writes one frame with a single writev, so the payload is not
// copied. Callers serialize writes on a connection.
func writeFrame(w io.Writer, header []byte, payload []byte) error {
	frameLen := 4 + len(header) + len(payload)
	if len(header) > maxHeaderLen || frameLen > maxFrameLen {
		return errFrameTooLarge
	}
	var lens [8]byte
	binary.BigEndian.PutUint32(lens[:4], uint32(frameLen))
	binary.BigEndian.PutUint32(lens[4:], uint32(len(header)))
	bufs := net.Buffers{lens[:], header}
	if len(payload) > 0 {
		bufs = append(bufs, payload)
	}
	_, err := bufs.WriteTo(w)
	return err
}
