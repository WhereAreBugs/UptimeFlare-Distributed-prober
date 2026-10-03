package agent

// Windows does not expose a supported directory-fsync equivalent through
// os.File.Sync. The cache file is synced before replacement, but directory
// metadata durability after a sudden power loss depends on the filesystem.
// Startup still fails closed if a durable queue loses its identity cache.
func syncConfigDirectory(string) error { return nil }
