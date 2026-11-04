// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TokenFile in DataDir holds the token for the TCP listener (-tcp), which a
// Linux container under Docker Desktop reaches as host.docker.internal.
const TokenFile = "host-token"

// Administrators, SYSTEM and the owner (the creating account).
const tokenSDDL = "D:P(A;;GA;;;BA)(A;;GA;;;SY)(A;;GA;;;OW)"

func loopback(addr string) error {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-tcp %s: only a loopback address is allowed", addr)
	}
	return nil
}

// Token reads the token from DataDir, creating it readable by administrators
// and SYSTEM only. A directory or token planted by someone else is refused.
func Token() (string, error) {
	dir := logDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := filepath.Join(dir, TokenFile)
	for _, p := range []string{dir, name} {
		if err := CheckOwner(p); err != nil {
			return "", err
		}
	}
	if b, err := os.ReadFile(name); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b)), nil
	}
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	return tok, writeSecret(name, tok)
}

func writeSecret(name, s string) error {
	sd, _ := windows.SecurityDescriptorFromString(tokenSDDL)
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	p, _ := windows.UTF16PtrFromString(name)
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, sa, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), name)
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}

func auth(token string, h http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
