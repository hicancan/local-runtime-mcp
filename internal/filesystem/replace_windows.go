//go:build windows

package filesystem

import "golang.org/x/sys/windows"

func replaceFile(source, destination string) error {
	return moveFile(source, destination, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func createFile(source, destination string) error {
	return moveFile(source, destination, windows.MOVEFILE_WRITE_THROUGH)
}

func moveFile(source, destination string, flags uint32) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourcePointer, destinationPointer, flags)
}
