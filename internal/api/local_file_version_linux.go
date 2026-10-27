//go:build linux

package api

import (
	"os"

	"golang.org/x/sys/unix"
)

func localFileVersionForFile(f *os.File) (localFileVersion, bool, error) {
	if f == nil {
		return localFileVersion{}, false, nil
	}

	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return localFileVersion{}, false, err
	}

	version := localFileVersion{
		volume: uint64(stat.Dev),
		fileID: uint64(stat.Ino),
		changeTime: uint64(stat.Ctim.Sec)<<32 |
			uint64(uint32(stat.Ctim.Nsec)),
	}
	if version.fileID == 0 || version.changeTime == 0 {
		return localFileVersion{}, false, nil
	}
	return version, true, nil
}
