//go:build !windows

package filesystem

import "os"

func replaceFile(source, destination string) error { return os.Rename(source, destination) }

func createFile(source, destination string) error {
	// Linking a complete temporary file is an atomic no-overwrite publish on the
	// same filesystem. Readers never see a partially written destination.
	if err := os.Link(source, destination); err != nil {
		return err
	}
	_ = os.Remove(source)
	return nil
}
