//go:build !windows

package filesystem

import "os"

func replaceFile(source, destination string) error { return os.Rename(source, destination) }
