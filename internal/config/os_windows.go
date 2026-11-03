// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package config

const (
	windows           = true
	defaultStorage    = `C:\data`
	defaultHostSocket = `\\.\pipe\hatchlens-host`
)
