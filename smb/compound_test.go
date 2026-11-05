package smb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func wirePostqueryClose(id wireID) []byte {
	body := make([]byte, 24)
	binary.LittleEndian.PutUint16(body, 24)
	binary.LittleEndian.PutUint16(body[2:], 1)
	copy(body[8:], id[:])
	return body
}

func wireRelatedID() wireID {
	var id wireID
	for i := range id {
		id[i] = 0xff
	}
	return id
}

// TestSMBRelatedCloseAfterOptionalFailure reproduces macOS's metadata compounds:
// an unsuccessful optional probe must not leak the successfully opened handle.
func TestSMBRelatedCloseAfterOptionalFailure(t *testing.T) {
	for _, tc := range []struct {
		name, object string
		options      uint32
		command      uint16
		status       uint32
	}{
		{"empty-directory", "empty", 1, 14, 0xc000000f},
		{"unsupported-ioctl", "file", 0x40, 11, 0xc0000010},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := startSMB(t, nil, func(lower, upper string) {
				putDiskFile(t, lower, "file", "kept")
				if err := os.Mkdir(filepath.Join(lower, "empty"), 0o755); err != nil {
					t.Fatal(err)
				}
			})
			c := guestClient(t, fixture)
			related := wireRelatedID()
			var probe []byte
			if tc.command == 14 {
				probe = make([]byte, 32)
				binary.LittleEndian.PutUint16(probe, 33)
				probe[2], probe[3] = 12, 1
				copy(probe[8:], related[:])
				pattern := wireUTF16("missing-entry")
				binary.LittleEndian.PutUint16(probe[24:], 96)
				binary.LittleEndian.PutUint16(probe[26:], uint16(len(pattern)))
				binary.LittleEndian.PutUint32(probe[28:], 4096)
				probe = append(probe, pattern...)
			} else {
				probe = make([]byte, 56)
				binary.LittleEndian.PutUint16(probe, 57)
				binary.LittleEndian.PutUint32(probe[4:], 0x000900c4)
				copy(probe[8:], related[:])
				binary.LittleEndian.PutUint32(probe[48:], 1)
			}
			replies := c.compoundRequests(
				c.packet(5, wireCreateBody(tc.object, 1, tc.options), 0, c.tree, c.session),
				c.packet(tc.command, probe, wireRelated, ^uint32(0), ^uint64(0)),
				c.packet(6, wirePostqueryClose(related), wireRelated, ^uint32(0), ^uint64(0)),
			)
			c.expectStatus(replies[0], wireSuccess)
			c.expectStatus(replies[1], tc.status)
			c.expectStatus(replies[2], wireSuccess)
			if len(replies[0].body) < 88 || len(replies[2].body) < 60 || binary.LittleEndian.Uint16(replies[2].body[2:])&1 == 0 {
				t.Fatal("compound did not return CREATE and CLOSE postquery attributes")
			}
			var closed wireID
			copy(closed[:], replies[0].body[64:80])
			_, r := c.queryInfo(closed, 6)
			c.expectStatus(r, 0xc0000128)

			// A second exclusive open also proves the global share registry was
			// released, rather than merely hiding the handle from this tree.
			body := wireCreateBody(tc.object, 1, tc.options)
			binary.LittleEndian.PutUint32(body[32:], 0)
			r = c.exchange(5, body)
			c.expectStatus(r, wireSuccess)
			if len(r.body) < 88 {
				t.Fatal("short exclusive CREATE response")
			}
			var reopened wireID
			copy(reopened[:], r.body[64:80])
			c.expectStatus(c.close(reopened), wireSuccess)
		})
	}
}

// TestSMBRelatedExistingHandle inherits a FileId supplied by a prior operation,
// not just by CREATE, and permits cleanup without executing later mutations.
func TestSMBRelatedExistingHandle(t *testing.T) {
	for _, failedProbe := range []bool{false, true} {
		name := "successful-query"
		if failedProbe {
			name = "failed-query"
		}
		t.Run(name, func(t *testing.T) {
			fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "file", "kept") })
			c := guestClient(t, fixture)
			id := mustOpen(t, c, "file", 1, 0x40)
			related := wireRelatedID()
			class := byte(6)
			if failedProbe {
				class = 21
			}
			parts := [][]byte{c.packet(16, wireQueryBody(id, class), 0, c.tree, c.session)}
			if failedProbe {
				write := make([]byte, 48)
				binary.LittleEndian.PutUint16(write, 49)
				binary.LittleEndian.PutUint16(write[2:], 112)
				binary.LittleEndian.PutUint32(write[4:], 4)
				copy(write[16:], related[:])
				parts = append(parts, c.packet(9, append(write, []byte("BAD!")...), wireRelated, ^uint32(0), ^uint64(0)))
			}
			parts = append(parts, c.packet(6, wirePostqueryClose(related), wireRelated, ^uint32(0), ^uint64(0)))
			replies := c.compoundRequests(parts...)
			if failedProbe {
				c.expectStatus(replies[0], 0xc0000034)
				c.expectStatus(replies[1], 0xc000000d)
			} else {
				c.expectStatus(replies[0], wireSuccess)
			}
			c.expectStatus(replies[len(replies)-1], wireSuccess)
			_, r := c.queryInfo(id, 6)
			c.expectStatus(r, 0xc0000128)
			reopened := mustOpen(t, c, "file", 1, 0x40)
			readEquals(t, c, reopened, "kept")
			c.expectStatus(c.close(reopened), wireSuccess)
		})
	}
}

// TestSMBRelatedCloseRequiresValidContext preserves failed-CREATE and malformed
// first-related rejection instead of treating every failed-chain CLOSE as safe.
func TestSMBRelatedCloseRequiresValidContext(t *testing.T) {
	for _, failedCreate := range []bool{false, true} {
		name := "first-request-related"
		if failedCreate {
			name = "failed-create"
		}
		t.Run(name, func(t *testing.T) {
			fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "file", "kept") })
			c := guestClient(t, fixture)
			id := mustOpen(t, c, "file", 1, 0x40)
			var first []byte
			want := uint32(0xc000000d)
			if failedCreate {
				first = c.packet(5, wireCreateBody("missing", 1, 0x40), 0, c.tree, c.session)
				want = 0xc0000034
			} else {
				first = c.packet(16, wireQueryBody(id, 6), wireRelated, c.tree, c.session)
			}
			replies := c.compoundRequests(first, c.packet(6, wirePostqueryClose(id), wireRelated, ^uint32(0), ^uint64(0)))
			c.expectStatus(replies[0], want)
			c.expectStatus(replies[1], 0xc000000d)
			readEquals(t, c, id, "kept")
			c.expectStatus(c.close(id), wireSuccess)
		})
	}
}
