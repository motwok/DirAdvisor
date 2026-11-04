// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fsctlQueryUsnJournal = 0x000900f4
	fsctlReadUsnJournal  = 0x000900bb

	attrDirectory = 0x10
	attrReparse   = 0x400

	reasonDataOverwrite  = 0x00000001
	reasonDataExtend     = 0x00000002
	reasonDataTruncation = 0x00000004
	reasonFileCreate     = 0x00000100
	reasonFileDelete     = 0x00000200
	reasonRenameNew      = 0x00002000
	reasonClose          = 0x80000000
	reasonData           = reasonDataOverwrite | reasonDataExtend | reasonDataTruncation

	share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

	fileIDBothDirectoryInfo        = 10
	fileIDBothDirectoryRestartInfo = 11
)

// Replaceable in tests.
var (
	ioctl         = windows.DeviceIoControl
	createFile    = windows.CreateFile
	dirInfo       = windows.GetFileInformationByHandleEx
	handleInfo    = windows.GetFileInformationByHandle
	volPathName   = windows.GetVolumePathName
	volMountName  = windows.GetVolumeNameForVolumeMountPoint
	volInfo       = windows.GetVolumeInformation
	finalPathName = windows.GetFinalPathNameByHandle
	processToken  = windows.OpenProcessToken
	privilege     = windows.LookupPrivilegeValue
)

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

func utf16le(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = le16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// extended prefixes a path with \\?\, so names Win32 would normalize away
// (trailing dots and spaces) and long paths open as they are.
func extended(path string) string {
	switch {
	case strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`):
		return path
	case strings.HasPrefix(path, `\\`):
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + filepath.Clean(path)
}

func open(path string, access uint32, flags uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(extended(path))
	if err != nil {
		return windows.InvalidHandle, err
	}
	return createFile(p, access, share, nil, windows.OPEN_EXISTING, flags, 0)
}

type journal struct{ id, next uint64 }

func queryJournal(vol windows.Handle) (journal, error) {
	out := make([]byte, 80)
	var n uint32
	if err := ioctl(vol, fsctlQueryUsnJournal, nil, 0, &out[0], uint32(len(out)), &n, nil); err != nil {
		return journal{}, err
	}
	return journal{id: le64(out[0:]), next: le64(out[16:])}, nil
}

type record struct {
	frn, parent uint64
	reason      uint32
	attrs       uint32
}

// readJournal returns the records from start on and the USN to go on with.
func readJournal(vol windows.Handle, id, start uint64) ([]record, uint64, error) {
	in := make([]byte, 40)
	out := make([]byte, 1<<20)
	var recs []record
	for {
		binary.LittleEndian.PutUint64(in[0:], start)
		binary.LittleEndian.PutUint32(in[8:], 0xFFFFFFFF)
		binary.LittleEndian.PutUint64(in[32:], id)
		var n uint32
		if err := ioctl(vol, fsctlReadUsnJournal, &in[0], uint32(len(in)), &out[0], uint32(len(out)), &n, nil); err != nil {
			return nil, start, err
		}
		next := le64(out[0:])
		if n <= 8 || next == start {
			return recs, next, nil
		}
		for p := uint32(8); p+60 <= n; {
			r := out[p:]
			if le16(r[4:]) == 2 {
				recs = append(recs, record{frn: le64(r[8:]), parent: le64(r[16:]), reason: le32(r[40:]), attrs: le32(r[52:])})
			}
			p += le32(r[0:])
		}
		start = next
	}
}

var procOpenFileByID = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenFileById")

// openByID is replaceable in tests.
var openByID = openFileByID

// openFileByID opens a file by its file reference number; hint is any handle
// on the volume.
func openFileByID(hint windows.Handle, frn uint64) (windows.Handle, error) {
	var desc [24]byte
	binary.LittleEndian.PutUint32(desc[0:], 24)
	binary.LittleEndian.PutUint64(desc[8:], frn)
	r, _, err := procOpenFileByID.Call(uintptr(hint), uintptr(unsafe.Pointer(&desc[0])), windows.FILE_READ_ATTRIBUTES,
		share, 0, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if windows.Handle(r) == windows.InvalidHandle {
		return windows.InvalidHandle, err
	}
	return windows.Handle(r), nil
}

// sizes returns the allocated size and the end of file of an open file.
func sizes(h windows.Handle) (alloc, size int64, err error) {
	var info [24]byte
	if err := dirInfo(h, windows.FileStandardInfo, &info[0], uint32(len(info))); err != nil {
		return 0, 0, err
	}
	return int64(le64(info[0:])), int64(le64(info[8:])), nil
}

// fileID returns the file reference number of a path.
func fileID(path string) (uint64, error) {
	h, err := open(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var bi windows.ByHandleFileInformation
	if err := handleInfo(h, &bi); err != nil {
		return 0, err
	}
	return uint64(bi.FileIndexHigh)<<32 | uint64(bi.FileIndexLow), nil
}

type entry struct {
	frn   uint64
	name  string
	attrs uint32
	alloc int64
}

// list returns the entries of a directory with their file IDs and sizes, in
// one call per buffer instead of one open per file.
func list(dir string) ([]entry, error) {
	h, err := open(dir, windows.FILE_LIST_DIRECTORY, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 64<<10)
	class := uint32(fileIDBothDirectoryRestartInfo)
	var out []entry
	for {
		err := dirInfo(h, class, &buf[0], uint32(len(buf)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		class = fileIDBothDirectoryInfo
		for off := uint32(0); ; {
			e := buf[off:]
			nlen := le32(e[60:])
			name := utf16le(e[104 : 104+nlen])
			if name != "." && name != ".." {
				out = append(out, entry{frn: le64(e[96:]), name: name, attrs: le32(e[56:]), alloc: int64(le64(e[48:]))})
			}
			next := le32(e[0:])
			if next == 0 {
				break
			}
			off += next
		}
	}
}

// volumeOf returns the volume root of a path ("C:\") and its file system.
func volumeOf(path string) (root, fstype string, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", "", err
	}
	vol := make([]uint16, windows.MAX_PATH)
	if err := volPathName(p, &vol[0], uint32(len(vol))); err != nil {
		return "", "", err
	}
	fsName := make([]uint16, windows.MAX_PATH)
	if err := volInfo(&vol[0], nil, 0, nil, nil, nil, &fsName[0], uint32(len(fsName))); err != nil {
		return "", "", err
	}
	return windows.UTF16ToString(vol), windows.UTF16ToString(fsName), nil
}

// enableBackup lets the service read trees whose ACLs exclude SYSTEM.
func enableBackup() error {
	var tok windows.Token
	if err := processToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return err
	}
	defer tok.Close()
	var luid windows.LUID
	if err := privilege(nil, windows.StringToUTF16Ptr("SeBackupPrivilege"), &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	return windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil)
}

// volumeName returns the `\\?\Volume{GUID}` device of a mount point, a drive
// root or a folder a volume is mounted at, to open for the USN journal.
func volumeName(root string) (string, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, 64)
	if err := volMountName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return strings.TrimSuffix(windows.UTF16ToString(buf), `\`), nil
}

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
