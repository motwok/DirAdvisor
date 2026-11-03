// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

// Command hatchlens-host is the HatchLens Host service on Windows.
package main

import (
	"os"

	"github.com/motwok/HatchLens/internal/winhost"
)

func main() {
	os.Exit(winhost.Main(os.Args[1:], os.Stdout, os.Stderr))
}
