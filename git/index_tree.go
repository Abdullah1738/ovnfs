package git

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// IndexTree reads stage-zero entries and blob sizes from a captured index without reading blob content.
func (r *Repo) IndexTree(ctx context.Context, index string) ([]TreeEntry, error) {
	cmd := r.command(ctx, "ls-files", "--stage", "-z")
	cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	out, err := run(cmd)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	var objects strings.Builder
	for raw := range bytes.SplitSeq(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		header, name, ok := strings.Cut(string(raw), "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("invalid ls-files record %q", raw)
		}
		if fields[2] != "0" {
			continue
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil {
			return nil, err
		}
		entry := TreeEntry{Mode: uint32(mode), OID: fields[1], Path: name, Type: "blob"}
		if mode == 0o160000 {
			entry.Type = "commit"
		} else {
			objects.WriteString(entry.OID)
			objects.WriteByte('\n')
		}
		entries = append(entries, entry)
	}
	if objects.Len() == 0 {
		return entries, nil
	}
	cmd = r.command(ctx, "cat-file", "--batch-check=%(objectname) %(objectsize)")
	cmd.Stdin = strings.NewReader(objects.String())
	out, err = run(cmd)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(bytes.TrimSuffix(out, []byte{'\n'}), []byte{'\n'})
	n := 0
	for i := range entries {
		if entries[i].Type == "commit" {
			continue
		}
		if n >= len(lines) {
			return nil, fmt.Errorf("missing index object size for %s", entries[i].OID)
		}
		fields := strings.Fields(string(lines[n]))
		n++
		if len(fields) != 2 || fields[0] != entries[i].OID {
			return nil, fmt.Errorf("invalid index object size %q", lines[n-1])
		}
		entries[i].Size, err = strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("index object %s: %w", entries[i].OID, err)
		}
	}
	return entries, nil
}
