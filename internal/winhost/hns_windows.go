// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procHNSCall = windows.NewLazySystemDLL("vmcompute.dll").NewProc("HNSCall")

// hnsCall is HNSCall of vmcompute.dll, the call hcsshim wraps; replaceable in
// tests.
var hnsCall = func(method, path string) (string, error) {
	m, _ := windows.UTF16PtrFromString(method)
	p, _ := windows.UTF16PtrFromString(path)
	req, _ := windows.UTF16PtrFromString("")
	var resp *uint16
	if err := procHNSCall.Find(); err != nil {
		return "", err
	}
	hr, _, _ := procHNSCall.Call(uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(req)),
		uintptr(unsafe.Pointer(&resp)))
	if resp != nil {
		defer windows.CoTaskMemFree(unsafe.Pointer(resp))
	}
	if hr != 0 {
		return "", fmt.Errorf("HNSCall %s %s: 0x%x", method, path, hr)
	}
	return windows.UTF16PtrToString(resp), nil
}

func hnsEndpoints() ([]Endpoint, error) {
	s, err := hnsCall("GET", "/endpoints/")
	if err != nil {
		return nil, err
	}
	var r struct {
		Success bool
		Error   string
		Output  []struct {
			ID             string
			VirtualNetwork string
		}
	}
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, err
	}
	if !r.Success {
		return nil, fmt.Errorf("HNS: %s", r.Error)
	}
	out := make([]Endpoint, 0, len(r.Output))
	for _, e := range r.Output {
		out = append(out, Endpoint{ID: e.ID, Network: e.VirtualNetwork})
	}
	return out, nil
}
