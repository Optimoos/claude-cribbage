//go:build unix

package main

import (
	"net"
	"syscall"
)

// listenPrivate listens on a Unix socket created with mode 0600, so no one
// else can connect in the moment before serveAdmin's chmod. The umask is
// process-wide; while it is changed, other new files are only made more
// private.
func listenPrivate(path string) (net.Listener, error) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}
