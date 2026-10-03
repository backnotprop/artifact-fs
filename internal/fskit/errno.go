package fskit

import (
	"errors"
	"io/fs"
	"strconv"
	"syscall"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// wireErr is an error that carries its wire errno directly.
type wireErr int32

func (e wireErr) Error() string { return "fskit errno " + strconv.Itoa(int(e)) }

// errnoFor maps an engine, overlay or OS error to a wire errno, one case at a
// time (contract section 6). Anything not recognised is EIO.
func errnoFor(err error) int32 {
	if err == nil {
		return errOK
	}
	var we wireErr
	if errors.As(err, &we) {
		return int32(we)
	}
	switch {
	// ENOTEMPTY first: syscall.ENOTEMPTY also matches fs.ErrExist.
	case errors.Is(err, syscall.ENOTEMPTY):
		return errENOTEMPTY
	case errors.Is(err, fs.ErrNotExist):
		return errENOENT
	case errors.Is(err, fs.ErrExist):
		return errEEXIST
	case errors.Is(err, syscall.ENOTDIR):
		return errENOTDIR
	case errors.Is(err, syscall.EISDIR):
		return errEISDIR
	case errors.Is(err, syscall.ENAMETOOLONG), errors.Is(err, model.ErrBlobTooLarge):
		return errENAMETOOLONG
	case errors.Is(err, syscall.ENOSPC):
		return errENOSPC
	case errors.Is(err, syscall.EDQUOT):
		return errEDQUOT
	case errors.Is(err, syscall.EFBIG):
		return errEFBIG
	case errors.Is(err, syscall.EROFS):
		return errEROFS
	case errors.Is(err, fs.ErrPermission):
		return errEACCES
	case errors.Is(err, fs.ErrInvalid):
		return errEINVAL
	default:
		// Download, git and network failures, timeouts and cancellation.
		return errEIO
	}
}
