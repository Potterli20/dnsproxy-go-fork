//go:build windows

package upstream

import (
	"github.com/Potterli20/golibs-fork/errors"
	"golang.org/x/sys/windows"
)

// isConnBroken returns true if err means that a connection is broken.
func isConnBroken(err error) (ok bool) {
	return errors.Is(err, windows.WSAECONNABORTED) || errors.Is(err, windows.WSAECONNRESET)
}
