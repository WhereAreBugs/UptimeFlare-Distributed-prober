//go:build !windows

package agent

import (
	"fmt"
	"os"
)

// File.Sync persists the cache contents; syncing its parent also persists the
// rename before results can be committed under the new cached identity.
func syncConfigDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open configuration directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync configuration directory: %w", err)
	}
	return nil
}
