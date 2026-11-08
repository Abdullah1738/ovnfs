//go:build !windows

package main

import "os/exec"

// defaultEditor is what config edit runs when neither $VISUAL nor $EDITOR is set.
const defaultEditor = "vi"

// editorCommand runs editor, a shell command line such as `code --wait`, on
// file, the way git runs $EDITOR.
func editorCommand(editor, file string) *exec.Cmd {
	return exec.Command("sh", "-c", editor+` "$@"`, editor, file)
}
