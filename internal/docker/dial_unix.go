// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package docker

import (
	"context"
	"net"
)

// DefaultSocket is the default of docker.socket.
const DefaultSocket = "/var/run/docker.sock"

func dialer(socket string) dialFunc {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
}
