// Package strerror words an errno as the remote's C library does, so the remote helpers'
// messages read as the Python ones they replaced did.
package strerror

import (
	"strings"
	"syscall"
)

// Of is the C library's message for e, which Go's lacks only the capital of.
func Of(e syscall.Errno) string {
	m := e.Error()
	if m == "" {
		return m
	}
	return strings.ToUpper(m[:1]) + m[1:]
}
