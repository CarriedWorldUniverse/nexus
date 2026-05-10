package rewriter

import (
	"errors"
	"strings"
	"syscall"
)

// isFileBusyErr reports whether the rename failure was caused by the
// destination file being open in another process. On Windows this
// surfaces as ERROR_SHARING_VIOLATION (errno 32). On Unix the rename
// succeeds atomically even with the file open, so this is effectively
// always false there.
//
// Implemented in a single file rather than _windows.go / _unix.go to
// keep the package compilable on either platform without build-tag
// gymnastics. The errno check is a no-op on platforms that don't
// emit it, which is the correct behavior.
func isFileBusyErr(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		// Windows ERROR_SHARING_VIOLATION = 32, ERROR_LOCK_VIOLATION = 33.
		// Both indicate the file is open by another process.
		if errno == syscall.Errno(32) || errno == syscall.Errno(33) {
			return true
		}
	}
	// Defensive textual fallback — the os.LinkError on Windows
	// includes "being used by another process" in its message even
	// when the embedded errno isn't numeric on some Go versions.
	if msg := err.Error(); strings.Contains(msg, "being used by another process") ||
		strings.Contains(msg, "sharing violation") {
		return true
	}
	return false
}
