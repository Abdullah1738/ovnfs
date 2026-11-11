// Package watch reports changes below host directories: FSEvents on macOS,
// inotify on Linux, ReadDirectoryChangesW on Windows.
//
// A Watcher calls its fn with Changes, each path spelled under the directory
// passed to Add it lies below. A Change marked Tree stands for unknown changes
// anywhere at or below its path: the backend lost individual events.
// Covers tells whether the backend currently sees every change below an added
// directory, so callers may cache what lies there until a report recalls it.
// Sync returns once every change made before the call has reached fn.
package watch

// Change is a changed path, spelled under the directory given to Add.
type Change struct {
	Path string
	// Tree marks a change anywhere at or below Path whose individual paths are
	// unknown (lost or coalesced events); a watched directory itself as Path
	// means everything in it may have changed.
	Tree bool
}
