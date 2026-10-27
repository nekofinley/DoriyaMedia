//go:build windows

package api

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type localWindowsFileBasicInfo struct {
	CreationTime   windows.Filetime
	LastAccessTime windows.Filetime
	LastWriteTime  windows.Filetime
	ChangeTime     windows.Filetime
	FileAttributes uint32
	_              uint32
}

func localFileVersionForFile(f *os.File) (localFileVersion, bool, error) {
	if f == nil {
		return localFileVersion{}, false, nil
	}
	handle := windows.Handle(f.Fd())

	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &identity); err != nil {
		return localFileVersion{}, false, err
	}

	var basic localWindowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)),
		uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return localFileVersion{}, false, err
	}

	version := localFileVersion{
		volume: uint64(identity.VolumeSerialNumber),
		fileID: uint64(identity.FileIndexHigh)<<32 | uint64(identity.FileIndexLow),
		changeTime: uint64(basic.ChangeTime.HighDateTime)<<32 |
			uint64(basic.ChangeTime.LowDateTime),
	}
	if version.fileID == 0 || version.changeTime == 0 {
		return localFileVersion{}, false, nil
	}
	return version, true, nil
}
