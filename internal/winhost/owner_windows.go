// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// Replaceable in tests.
var fileOwner = func(name string) (*windows.SID, error) {
	sd, err := windows.GetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, _, err := sd.Owner()
	return owner, err
}

// CheckOwner fails when name exists and is owned by anyone but SYSTEM or the
// Administrators group: a user may have planted it before the service ran.
func CheckOwner(name string) error {
	owner, err := fileOwner(name)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return nil
	}
	return fmt.Errorf("%s is owned by %s, not by SYSTEM or Administrators", name, owner)
}
