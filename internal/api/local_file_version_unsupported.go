//go:build !windows && !linux

package api

import "os"

func localFileVersionForFile(*os.File) (localFileVersion, bool, error) {
	return localFileVersion{}, false, nil
}
