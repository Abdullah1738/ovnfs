//go:build windows

package main

import (
	"cmp"
	"os"
	"os/exec"
	"syscall"
)

// defaultEditor is what config edit runs when neither %VISUAL% nor %EDITOR% is set.
const defaultEditor = "notepad"

// editorCommand runs editor, a command line such as `code --wait`, on file
// through cmd.exe. The line is passed raw: Go's argument quoting would turn
// the editor's own arguments into part of its name. /s makes cmd strip only
// the outermost quotes, so a quoted editor path survives.
func editorCommand(editor, file string) *exec.Cmd {
	cmd := exec.Command(cmp.Or(os.Getenv("ComSpec"), "cmd.exe"))
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd /s /c "` + editor + ` "` + file + `""`}
	return cmd
}
