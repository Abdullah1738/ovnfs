package main

import (
	"testing"

	"github.com/alecthomas/kong"
)

func TestServeOptions(t *testing.T) {
	for _, name := range []string{"OVNFS_MEMORY", "OVNFS_BLOB_CACHE", "OVNFS_ATIME", "OVNFS_FSYNC"} {
		t.Setenv(name, "")
	}
	parse := func(args ...string) (serveCmd, error) {
		var cli struct {
			Serve serveCmd `cmd:""`
		}
		parser, err := kong.New(&cli, serveDefaults())
		if err != nil {
			return cli.Serve, err
		}
		_, err = parser.Parse(append([]string{"serve"}, args...))
		return cli.Serve, err
	}

	cmd, err := parse()
	if err != nil || cmd.Memory != 1<<30 || cmd.BlobCache != 512<<20 || cmd.Atime || cmd.Fsync {
		t.Fatalf("defaults: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_MEMORY", "2G")
	t.Setenv("OVNFS_BLOB_CACHE", "64MiB")
	t.Setenv("OVNFS_ATIME", "1")
	t.Setenv("OVNFS_FSYNC", "1")
	cmd, err = parse()
	if err != nil || cmd.Memory != 2<<30 || cmd.BlobCache != 64<<20 || !cmd.Atime || !cmd.Fsync {
		t.Fatalf("environment: %+v, %v", cmd, err)
	}
	cmd, err = parse("--memory", "0", "--blob-cache", "1K", "--proto", "smb", "--listen", "localhost:1234")
	if err != nil || cmd.Memory != 0 || cmd.BlobCache != 1024 || cmd.Proto != "smb" || cmd.Listen != "localhost:1234" {
		t.Fatalf("flags: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_FSYNC", "false")
	cmd, err = parse("--fsync")
	if err != nil || !cmd.Fsync {
		t.Fatalf("fsync flag: %+v, %v", cmd, err)
	}
	if _, err := parse("--memory", "-1"); err == nil {
		t.Fatal("negative memory accepted")
	}
	if _, err := parse("--proto", "unknown"); err == nil {
		t.Fatal("unknown protocol accepted")
	}
}
