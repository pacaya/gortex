//go:build !linux

package indexer

// slowWatchMount is a Linux/WSL2 concern (9p/drvfs/SMB mounts where native
// fsnotify is unreliable). On other platforms native fsnotify is used as-is.
func slowWatchMount(string) bool { return false }
