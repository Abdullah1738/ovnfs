package smb

import "testing"

// TestSMBRetirementInvalidatesOpenHandles prevents native descriptors from
// retaining access to retired checkout data after its namespace disappears.
func TestSMBRetirementInvalidatesOpenHandles(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) {
		putDiskFile(t, lower, "retired/file", "retired data")
		putDiskFile(t, lower, "retired-other/file", "live data")
	})
	c := guestClient(t, fixture)
	retired := mustOpen(t, c, "retired/file", 1, 0x40)
	live := mustOpen(t, c, "retired-other/file", 1, 0x40)
	if err := fixture.server.FS.Remove("retired/file"); err != nil {
		t.Fatal(err)
	}
	fixture.server.Retire("retired")
	_, r := c.read(retired, 0, 64)
	c.expectStatus(r, statusFileClosed)
	_, r = c.queryInfo(retired, 5)
	c.expectStatus(r, statusFileClosed)
	readEquals(t, c, live, "live data")
	c.expectStatus(c.close(live), wireSuccess)
}
