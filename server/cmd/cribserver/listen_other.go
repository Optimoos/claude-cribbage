//go:build !unix

package main

import "net"

// listenPrivate listens on a Unix socket; serveAdmin then restricts it.
func listenPrivate(path string) (net.Listener, error) { return net.Listen("unix", path) }
