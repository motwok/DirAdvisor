// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package docker

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

// DefaultSocket is the default of docker.socket.
const DefaultSocket = `\\.\pipe\docker_engine`

const errorPipeBusy = syscall.Errno(231)

// openPipe opens the pipe for overlapped I/O, so os.NewFile puts it on the runtime's
// completion port and concurrent reads and writes do not block each other.
var openPipe = func(name string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

type pipeConn struct {
	*os.File
	addr pipeAddr
}

func (c pipeConn) LocalAddr() net.Addr  { return c.addr }
func (c pipeConn) RemoteAddr() net.Addr { return c.addr }

func dialer(pipe string) dialFunc {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		for {
			f, err := openPipe(pipe)
			if err == nil {
				return pipeConn{f, pipeAddr(pipe)}, nil
			}
			if !errors.Is(err, errorPipeBusy) {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}
