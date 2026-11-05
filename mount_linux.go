//go:build linux

package main

import (
	"os"
	"strings"
)

// mounted matches procfs mount records without querying the mounted filesystem.
func mounted(dir string) bool {
	target := mountPath(dir)
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return true
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && unescape.Replace(fields[4]) == target {
			return true
		}
	}
	return false
}
