// SPDX-License-Identifier: AGPL-3.0-or-later

// Command diradvisor reports per-directory disk usage from project quotas.
package main

import (
	"os"

	"github.com/motwok/DirAdvisor/internal/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr))
}
