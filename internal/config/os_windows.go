// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package config

// On Windows only the image part runs.
const (
	windows        = true
	defaultStorage = `C:\data`
)
