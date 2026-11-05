// Package watch reports changes below host directories: FSEvents on macOS,
// inotify on Linux, ReadDirectoryChangesW on Windows.
//
// A Watcher calls its fn with changed paths, each spelled under the directory
// passed to Add it lies below. A reported added directory itself stands for
// unknown changes anywhere below it: the backend lost individual events.
// Covers tells whether the backend currently sees every change below an added
// directory, so callers may cache what lies there until a report recalls it.
package watch
