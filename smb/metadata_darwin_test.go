package smb

import (
	"encoding/binary"
	"testing"
	"time"
)

// TestSMBTimestampsSurviveArchiveLifecycle exercises the metadata consumed when a
// client closes, reopens and renames a buffered archive, both before and after spill.
func TestSMBTimestampsSurviveArchiveLifecycle(t *testing.T) {
	for _, disk := range []bool{false, true} {
		name := "memory"
		if disk {
			name = "disk"
		}
		t.Run(name, func(t *testing.T) {
			fixture := startSMB(t, nil, nil)
			c := guestClient(t, fixture)
			id, reply := c.create("archive.tmp", 2, 0x40)
			c.expectStatus(reply, wireSuccess)
			created := binary.LittleEndian.Uint64(reply.body[8:])
			if created == 0 {
				t.Fatal("CREATE returned no creation time")
			}
			c.expectStatus(c.write(id, 0, []byte("archive data")), wireSuccess)
			if disk {
				fixture.flush(t)
			}

			// Use future, distinct timestamps: Darwin may move birth time backward
			// when utimes requests a modification time older than the birth time.
			accessed := uint64(time.Date(2037, 1, 2, 3, 4, 5, 0, time.UTC).Unix()+11644473600) * 10000000
			written := accessed + 600000000
			setTimes := func() {
				t.Helper()
				data := make([]byte, 40)
				binary.LittleEndian.PutUint64(data[8:], accessed)
				binary.LittleEndian.PutUint64(data[16:], written)
				c.expectStatus(c.setInfo(id, 4, data), wireSuccess)
			}
			checkTimes := func(data []byte, stage string) {
				t.Helper()
				if len(data) < 24 {
					t.Fatalf("%s: short timestamp information", stage)
				}
				if got := binary.LittleEndian.Uint64(data); got != created {
					t.Fatalf("%s: creation time changed after write/rename: %d, want %d", stage, got, created)
				}
				if got := binary.LittleEndian.Uint64(data[8:]); got != accessed {
					t.Fatalf("%s: access time = %d, want %d", stage, got, accessed)
				}
				if got := binary.LittleEndian.Uint64(data[16:]); got != written {
					t.Fatalf("%s: write time = %d, want %d", stage, got, written)
				}
			}
			closePostquery := func(stage string) {
				t.Helper()
				body := make([]byte, 24)
				binary.LittleEndian.PutUint16(body, 24)
				binary.LittleEndian.PutUint16(body[2:], 1)
				copy(body[8:], id[:])
				r := c.exchange(6, body)
				c.expectStatus(r, wireSuccess)
				if len(r.body) < 60 || binary.LittleEndian.Uint16(r.body[2:])&1 == 0 {
					t.Fatalf("%s: CLOSE did not return postquery attributes", stage)
				}
				checkTimes(r.body[8:], stage)
				if size := binary.LittleEndian.Uint64(r.body[48:]); size != 12 {
					t.Fatalf("%s: CLOSE EOF = %d, want 12", stage, size)
				}
			}
			reopen := func(path, stage string) {
				t.Helper()
				id, reply = c.create(path, 1, 0x40)
				c.expectStatus(reply, wireSuccess)
				checkTimes(reply.body[8:], stage)
				if size := binary.LittleEndian.Uint64(reply.body[48:]); size != 12 {
					t.Fatalf("%s: CREATE EOF = %d, want 12", stage, size)
				}
			}

			setTimes()
			closePostquery("written archive close")
			reopen("archive.tmp", "written archive reopen")
			c.expectStatus(c.rename(id, "output.rlib", false), wireSuccess)
			closePostquery("renamed archive close")
			reopen("output.rlib", "renamed archive reopen")
			c.expectStatus(c.write(id, 8, []byte("DATA")), wireSuccess)
			setTimes()
			closePostquery("rewritten archive close")
			reopen("output.rlib", "rewritten archive reopen")
			basic, r := c.queryInfo(id, 4)
			c.expectStatus(r, wireSuccess)
			checkTimes(basic, "QUERY_INFO")

			dir := mustOpen(t, c, "", 1, 1)
			body := make([]byte, 32)
			binary.LittleEndian.PutUint16(body, 33)
			body[2], body[3] = 1, 1
			copy(body[8:], dir[:])
			pattern := wireUTF16("output.rlib")
			binary.LittleEndian.PutUint16(body[24:], 96)
			binary.LittleEndian.PutUint16(body[26:], uint16(len(pattern)))
			binary.LittleEndian.PutUint32(body[28:], 4096)
			r = c.exchange(14, append(body, pattern...))
			c.expectStatus(r, wireSuccess)
			if len(r.body) < 8 {
				t.Fatal("short QUERY_DIRECTORY response")
			}
			entry := c.wireSecurity(r, int(binary.LittleEndian.Uint16(r.body[2:])), int(binary.LittleEndian.Uint32(r.body[4:])))
			if len(entry) < 64 {
				t.Fatal("short directory entry")
			}
			checkTimes(entry[8:], "QUERY_DIRECTORY")
			c.expectStatus(c.close(dir), wireSuccess)
			readEquals(t, c, id, "archive DATA")
			c.expectStatus(c.close(id), wireSuccess)
		})
	}
}
