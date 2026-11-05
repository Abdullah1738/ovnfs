package smb

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
	"time"
)

type openLeaseReply struct {
	state, flags uint32
	parent       [16]byte
	epoch        uint16
}

func leaseCreateBody(name string, access, share, state uint32, key, parent [16]byte, epoch uint16) []byte {
	b := wireCreateBody(name, 1, 0x40)
	b[3] = 255
	binary.LittleEndian.PutUint32(b[24:], access)
	binary.LittleEndian.PutUint32(b[32:], share)
	for (64+len(b))%8 != 0 {
		b = append(b, 0)
	}
	data := make([]byte, 52)
	copy(data, key[:])
	binary.LittleEndian.PutUint32(data[16:], state)
	binary.LittleEndian.PutUint32(data[20:], 4)
	copy(data[32:], parent[:])
	binary.LittleEndian.PutUint16(data[48:], epoch)
	context := appendCreateContexts([]createContext{{"RqLs", data}})
	binary.LittleEndian.PutUint32(b[48:], uint32(64+len(b)))
	binary.LittleEndian.PutUint32(b[52:], uint32(len(context)))
	return append(b, context...)
}

func leaseResponse(t *testing.T, r wireReply) (wireID, openLeaseReply) {
	t.Helper()
	if r.status != wireSuccess || len(r.body) < 88 {
		t.Fatalf("lease CREATE: status %#x, body length %d", r.status, len(r.body))
	}
	var id wireID
	copy(id[:], r.body[64:80])
	off, size := uint64(binary.LittleEndian.Uint32(r.body[80:])), uint64(binary.LittleEndian.Uint32(r.body[84:]))
	if off < 152 || size < 16 || off+size > uint64(len(r.raw)) {
		t.Fatal("missing or malformed lease response contexts")
	}
	data := r.raw[off : off+size]
	for {
		if len(data) < 16 {
			t.Fatal("short lease response context")
		}
		next := int(binary.LittleEndian.Uint32(data))
		span := len(data)
		if next != 0 {
			if next < 16 || next > span {
				t.Fatal("invalid lease response context chain")
			}
			span = next
		}
		no, nl := int(binary.LittleEndian.Uint16(data[4:])), int(binary.LittleEndian.Uint16(data[6:]))
		do, dl := int(binary.LittleEndian.Uint16(data[10:])), int(binary.LittleEndian.Uint32(data[12:]))
		if no < 16 || no+nl > span || do < 16 || do+dl > span {
			t.Fatal("invalid lease response context bounds")
		}
		if string(data[no:no+nl]) == "RqLs" {
			if dl != 52 {
				t.Fatalf("v2 lease response length %d", dl)
			}
			d := data[do : do+dl]
			var lease openLeaseReply
			lease.state = binary.LittleEndian.Uint32(d[16:])
			lease.flags = binary.LittleEndian.Uint32(d[20:])
			copy(lease.parent[:], d[32:48])
			lease.epoch = binary.LittleEndian.Uint16(d[48:])
			return id, lease
		}
		if next == 0 {
			t.Fatal("CREATE omitted requested lease response")
		}
		data = data[next:]
	}
}

func leaseReceive(t *testing.T, c *wireClient) wireReply {
	t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	raw, err := c.readFrame()
	if err != nil {
		t.Fatal(err)
	}
	r, err := decodeWireReply(raw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func leaseBreak(t *testing.T, c *wireClient, key [16]byte) uint16 {
	t.Helper()
	r := leaseReceive(t, c)
	if r.command != 18 || len(r.body) != 44 || binary.LittleEndian.Uint16(r.body) != 44 || !bytes.Equal(r.body[8:24], key[:]) {
		t.Fatalf("expected lease break for %x: command %d, body %x", key, r.command, r.body)
	}
	if binary.LittleEndian.Uint32(r.body[4:])&1 == 0 || binary.LittleEndian.Uint32(r.body[28:]) != 0 {
		t.Fatalf("expected acknowledged break to NONE: %x", r.body)
	}
	return binary.LittleEndian.Uint16(r.body[2:])
}

func leaseAcknowledge(c *wireClient, key [16]byte) {
	b := make([]byte, 36)
	binary.LittleEndian.PutUint16(b, 36)
	copy(b[8:], key[:])
	c.expectStatus(c.exchange(18, b), wireSuccess)
}

func leaseOpenID(t *testing.T, c *wireClient, r wireReply) wireID {
	t.Helper()
	c.expectStatus(r, wireSuccess)
	if len(r.body) < 88 {
		t.Fatal("short completed CREATE")
	}
	var id wireID
	copy(id[:], r.body[64:80])
	return id
}

// A break may require a same-key reopen to write dirty cached data. It must not
// wait behind the conflicting CREATE, and the reopen must not promote the lease.
func TestSMBLeaseCreateAllowsReopenFlush(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "cached", "before") })
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key, parent := [16]byte{1}, [16]byte{2}
	id, initial := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, parent, 11)))
	if initial.state != 3 || initial.flags != 4 || initial.parent != parent {
		t.Fatalf("initial lease: %+v", initial)
	}
	_, pending := other.create("cached", 1, 0x40)
	other.expectStatus(pending, statusPending)
	epoch := leaseBreak(t, owner, key)
	if epoch != initial.epoch+1 {
		t.Fatalf("break epoch %d, initial %d", epoch, initial.epoch)
	}
	reopened, breaking := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{99}, 900)))
	if breaking.state != 3 || breaking.flags != 6 || breaking.parent != parent || breaking.epoch != epoch {
		t.Fatalf("reopen changed pending lease: %+v, break epoch %d", breaking, epoch)
	}
	owner.expectStatus(owner.write(reopened, 0, []byte("flushed")), wireSuccess)
	owner.expectStatus(owner.close(reopened), wireSuccess)
	leaseAcknowledge(owner, key)
	final := leaseReceive(t, other)
	if final.command != 5 || final.flags&2 == 0 || binary.LittleEndian.Uint64(final.raw[32:]) != binary.LittleEndian.Uint64(pending.raw[32:]) {
		t.Fatal("CREATE did not retain its asynchronous identity")
	}
	opened := leaseOpenID(t, other, final)
	readEquals(t, other, opened, "flushed")
	other.expectStatus(other.close(opened), wireSuccess)
	owner.expectStatus(owner.close(id), wireSuccess)
}

// A locally closed HANDLE-cached open can disappear in response to the break,
// making an otherwise incompatible sharing request valid.
func TestSMBLeaseCreateRechecksSharingAfterCachedClose(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "cached", "before") })
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key := [16]byte{3}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 1, 1, 3, key, [16]byte{}, 0)))
	_, pending := other.create("cached", 1, 0x40)
	other.expectStatus(pending, statusPending)
	leaseBreak(t, owner, key)
	owner.expectStatus(owner.close(id), wireSuccess)
	opened := leaseOpenID(t, other, leaseReceive(t, other))
	other.expectStatus(other.write(opened, 0, []byte("after!")), wireSuccess)
	readEquals(t, other, opened, "after!")
	other.expectStatus(other.close(opened), wireSuccess)
}

// Acknowledging a break does not itself close an incompatible live open.
func TestSMBLeaseCreatePreservesLiveSharingViolation(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "cached", "before") })
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key := [16]byte{8}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 1, 1, 3, key, [16]byte{}, 0)))
	_, pending := other.create("cached", 1, 0x40)
	other.expectStatus(pending, statusPending)
	leaseBreak(t, owner, key)
	leaseAcknowledge(owner, key)
	other.expectStatus(leaseReceive(t, other), statusSharingViolation)
	readEquals(t, owner, id, "before")
	owner.expectStatus(owner.close(id), wireSuccess)
	opened := mustOpen(t, other, "cached", 1, 0x40)
	other.expectStatus(other.close(opened), wireSuccess)
}

func TestSMBLeaseDeleteOnCloseWaitsForCacheAndLastHandle(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "cached", "before") })
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key := [16]byte{9}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	_, pending := other.create("cached", 1, 0x1040)
	other.expectStatus(pending, statusPending)
	leaseBreak(t, owner, key)
	reopened, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	owner.expectStatus(owner.write(reopened, 0, []byte("flushed")), wireSuccess)
	owner.expectStatus(owner.close(reopened), wireSuccess)
	leaseAcknowledge(owner, key)
	deleting := leaseOpenID(t, other, leaseReceive(t, other))
	_, denied := other.create("cached", 1, 0x40)
	other.expectStatus(denied, statusDeletePending)
	other.expectStatus(other.close(deleting), wireSuccess)
	readEquals(t, owner, id, "flushed")
	owner.expectStatus(owner.close(id), wireSuccess)
	_, absent := other.create("cached", 1, 0x40)
	other.expectStatus(absent, statusObjectNameNotFound)
}

func TestSMBLeaseKeyCannotAliasDifferentFiles(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) {
		putDiskFile(t, lower, "first", "one")
		putDiskFile(t, lower, "second", "two")
	})
	owner := guestClient(t, fixture)
	key := [16]byte{10}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("first", 3, 7, 7, key, [16]byte{}, 0)))
	owner.expectStatus(owner.exchange(5, leaseCreateBody("second", 3, 7, 7, key, [16]byte{}, 0)), statusInvalidParameter)
	readEquals(t, owner, id, "one")
	owner.expectStatus(owner.close(id), wireSuccess)
	second, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("second", 3, 7, 7, key, [16]byte{}, 0)))
	readEquals(t, owner, second, "two")
	owner.expectStatus(owner.close(second), wireSuccess)
}

func TestSMBLeaseCreateCancellationReleasesTransaction(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "cached", "before") })
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key := [16]byte{4}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	_, pending := other.create("cached", 1, 0x40)
	other.expectStatus(pending, statusPending)
	leaseBreak(t, owner, key)
	cancel := other.packet(12, []byte{4, 0, 0, 0}, 2, other.tree, other.session)
	binary.LittleEndian.PutUint64(cancel[32:], binary.LittleEndian.Uint64(pending.raw[32:]))
	if err := other.writeFrame(cancel); err != nil {
		t.Fatal(err)
	}
	other.expectStatus(leaseReceive(t, other), statusCancelled)
	unrelated := mustOpen(t, other, "unrelated", 2, 0x40)
	other.expectStatus(other.close(unrelated), wireSuccess)
	leaseAcknowledge(owner, key)
	owner.expectStatus(owner.close(id), wireSuccess)
	opened := mustOpen(t, other, "cached", 1, 0x40)
	other.expectStatus(other.close(opened), wireSuccess)
}

func TestSMBLeaseCompoundCreateAllowsReopenFlush(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "cached", "before") })
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key := [16]byte{5}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	first := other.packet(5, wireCreateBody("cached", 1, 0x40), 0, other.tree, other.session)
	for len(first)%8 != 0 {
		first = append(first, 0)
	}
	binary.LittleEndian.PutUint32(first[20:], uint32(len(first)))
	closeBody := make([]byte, 24)
	binary.LittleEndian.PutUint16(closeBody, 24)
	for i := 8; i < len(closeBody); i++ {
		closeBody[i] = 0xff
	}
	second := other.packet(6, closeBody, wireRelated, ^uint32(0), ^uint64(0))
	if err := other.writeFrame(append(first, second...)); err != nil {
		t.Fatal(err)
	}
	leaseBreak(t, owner, key)
	reopened, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	owner.expectStatus(owner.write(reopened, 0, []byte("flushed")), wireSuccess)
	owner.expectStatus(owner.close(reopened), wireSuccess)
	leaseAcknowledge(owner, key)
	response := leaseReceive(t, other)
	other.expectStatus(response, wireSuccess)
	next := int(binary.LittleEndian.Uint32(response.raw[20:]))
	if response.command != 5 || next < 152 || next+64 > len(response.raw) {
		t.Fatal("missing related compound CLOSE response")
	}
	closed, err := decodeWireReply(response.raw[next:])
	if err != nil {
		t.Fatal(err)
	}
	other.expectStatus(closed, wireSuccess)
	if closed.command != 6 || binary.LittleEndian.Uint32(closed.raw[20:]) != 0 {
		t.Fatal("incorrect compound completion")
	}
	opened := mustOpen(t, other, "cached", 1, 0x40)
	readEquals(t, other, opened, "flushed")
	other.expectStatus(other.close(opened), wireSuccess)
	owner.expectStatus(owner.close(id), wireSuccess)
}

func TestSMBLeaseParentRenameWaitsForDescendantFlush(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "parent/child", "before") })
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key := [16]byte{6}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("parent/child", 3, 7, 7, key, [16]byte{}, 0)))
	directory := mustOpen(t, other, "parent", 1, 1)
	other.expectStatus(other.rename(directory, "renamed", false), statusPending)
	leaseBreak(t, owner, key)
	// The renaming open must be unlocked while its descendant callback runs.
	_, query := other.queryInfo(directory, 4)
	other.expectStatus(query, wireSuccess)
	if _, err := fixture.fs.Stat("parent/child"); err != nil {
		t.Fatalf("rename mutated source before descendant ACK: %v", err)
	}
	if _, err := fixture.fs.Stat("renamed"); !os.IsNotExist(err) {
		t.Fatalf("rename published target before descendant ACK: %v", err)
	}
	reopened, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("parent/child", 3, 7, 7, key, [16]byte{}, 0)))
	owner.expectStatus(owner.write(reopened, 0, []byte("flushed")), wireSuccess)
	owner.expectStatus(owner.close(reopened), wireSuccess)
	leaseAcknowledge(owner, key)
	other.expectStatus(leaseReceive(t, other), wireSuccess)
	opened := mustOpen(t, other, "renamed/child", 1, 0x40)
	readEquals(t, other, opened, "flushed")
	readEquals(t, owner, id, "flushed")
	other.expectStatus(other.close(opened), wireSuccess)
	other.expectStatus(other.close(directory), wireSuccess)
	owner.expectStatus(owner.close(id), wireSuccess)
}

func TestSMBLeaseDirectoryDispositionBreaksChildrenBeforeRecheck(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "parent/child", "before") })
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key := [16]byte{7}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("parent/child", 3, 7, 7, key, [16]byte{}, 0)))
	directory := mustOpen(t, other, "parent", 1, 1)
	other.expectStatus(other.disposition(directory, true), statusPending)
	leaseBreak(t, owner, key)
	owner.expectStatus(owner.write(id, 0, []byte("flushed")), wireSuccess)
	leaseAcknowledge(owner, key)
	other.expectStatus(leaseReceive(t, other), wireNotEmpty)
	readEquals(t, owner, id, "flushed")
	opened := mustOpen(t, other, "parent/child", 1, 0x40)
	other.expectStatus(other.close(opened), wireSuccess)
	other.expectStatus(other.close(directory), wireSuccess)
	owner.expectStatus(owner.close(id), wireSuccess)
}
