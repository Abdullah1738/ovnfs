package smb

import (
	"encoding/binary"
	"strings"
	"time"
)

// oplockGrant is shared by every open belonging to the same client file lease.
// A returned break snapshot retains source and done so a subsequent break cannot
// redirect an earlier waiter.
type oplockGrant struct {
	owner     *open
	c         *conn
	lease     bool
	key       [16]byte
	parentKey [16]byte
	state     uint32
	level     uint8
	version   int
	epoch     uint16
	done      chan struct{}
	timer     *time.Timer
	source    *oplockGrant
}

// init registers both classic oplock and lease break acknowledgements.
func init() { handlers[cmdOplockBreak] = handleOplockBreak }

// grantCaching grants caching only to an isolated open or handles sharing a client lease key.
func (g *openRegistry) grantCaching(c *conn, o *open, a createArgs) (uint16, uint32) {
	o.mu.Lock()
	p := o.path
	o.mu.Unlock()
	cacheable := c.srv.FS.Cacheable(p)
	g.mu.Lock()
	defer g.mu.Unlock()
	if previous := g.grants[o]; previous != nil {
		return previous.epoch, previous.leaseFlags()
	}
	lease := a.oplock == 255 && a.leaseVersion != 0
	isolated := true
	var grant *oplockGrant
	for other := range g.byPath[p] {
		if other == o {
			continue
		}
		previous := g.grants[other]
		if lease && previous != nil && previous.lease && previous.key == a.leaseKey && previous.c.clientGUID == c.clientGUID {
			grant = previous
			continue
		}
		isolated = false
	}
	if grant == nil {
		grant = &oplockGrant{owner: o, c: c, lease: lease, key: a.leaseKey, version: a.leaseVersion}
		if lease && a.leaseVersion == 52 {
			grant.epoch = a.leaseEpoch
			if a.leaseFlags&4 != 0 {
				grant.parentKey = a.leaseParentKey
			}
		}
	}
	if g.grants == nil {
		g.grants = make(map[*open]*oplockGrant)
	}
	g.grants[o] = grant
	if !o.isDir {
		if grant.lease {
			o.mu.Lock()
			o.oplock = 255
			o.leaseKey = grant.key
			o.mu.Unlock()
			// Daemon-side filesystem readers have no client-flush barrier, so
			// WRITE caching is not granted even when the client requests it.
			requested := a.leaseState & 3
			// A reopen cannot cancel a pending break, combine incomparable states,
			// or replace the server's epoch with a client-supplied epoch.
			if cacheable && isolated && c.dialect >= 0x210 && requested&1 != 0 && grant.done == nil && requested != grant.state && requested&grant.state == grant.state {
				grant.state = requested
				if grant.version == 52 {
					grant.epoch++
				}
			}
			// A lease key names one path, so its opens are all at p, besides
			// o, which is not published yet.
			o.mu.Lock()
			o.leaseState = grant.state
			o.mu.Unlock()
			for other := range g.byPath[p] {
				if other != o && g.grants[other] == grant {
					other.mu.Lock()
					other.leaseState = grant.state
					other.mu.Unlock()
				}
			}
		} else if cacheable && isolated && (a.oplock == 1 || a.oplock == 8 || a.oplock == 9) {
			// Exclusive and batch oplocks also allow unsafe client write caching.
			grant.level = 1
			o.mu.Lock()
			o.oplock = 1
			o.mu.Unlock()
		}
	}
	return grant.epoch, grant.leaseFlags()
}

// leaseFlags describes the current lease, not a later reopen's request.
func (grant *oplockGrant) leaseFlags() uint32 {
	var flags uint32
	if grant.done != nil {
		flags |= 2
	}
	if grant.parentKey != [16]byte{} {
		flags |= 4
	}
	return flags
}

// breakConflicting revokes other lease keys before a newly opened handle can access their data.
func (g *openRegistry) breakConflicting(c *conn, p string, key [16]byte, lease bool) []*oplockGrant {
	return g.breakCaching(p, false, func(grant *oplockGrant) bool {
		return !(lease && grant.lease && key == grant.key && c.clientGUID == grant.c.clientGUID)
	})
}

func withinPath(p, root string) bool {
	return root == "" || root == "." || p == root ||
		len(p) > len(root) && strings.HasPrefix(p, root) && p[len(root)] == '/'
}

// breakDescendants revokes all caches on a path and its children, including the caller's lease key.
func (g *openRegistry) breakDescendants(p string) []*oplockGrant {
	return g.breakCaching(strings.TrimSuffix(p, "/"), true, func(*oplockGrant) bool { return true })
}

// breakCaching selects each matching lease of the opens at p (and, with
// subtree, below p) once, snapshots its wait channel, and sends outside registry locks.
func (g *openRegistry) breakCaching(p string, subtree bool, matches func(*oplockGrant) bool) []*oplockGrant {
	g.mu.Lock()
	var breaks []*oplockGrant
	var notifications []struct {
		c    *conn
		body []byte
	}
	paths := []string{p}
	if subtree {
		paths = g.under(p)
	}
	var candidates []*open
	for _, q := range paths {
		for o := range g.byPath[q] {
			candidates = append(candidates, o)
		}
	}
	for _, o := range candidates {
		grant := g.grants[o]
		if grant == nil || grant.state == 0 && grant.level == 0 {
			continue
		}
		if !matches(grant) {
			continue
		}
		selected := false
		for _, pending := range breaks {
			if pending.source == grant {
				selected = true
				break
			}
		}
		if selected {
			continue
		}
		if grant.done == nil {
			grant.done = make(chan struct{})
			if (grant.lease && grant.state&6 != 0) || (!grant.lease && grant.level != 1) {
				done := grant.done
				grant.timer = time.AfterFunc(35*time.Second, func() {
					g.mu.Lock()
					if grant.done == done {
						g.finishBreakLocked(grant)
					}
					g.mu.Unlock()
				})
			}
			if grant.lease {
				if grant.version == 52 {
					grant.epoch++
				} else {
					grant.epoch = 0
				}
			}
			notifications = append(notifications, struct {
				c    *conn
				body []byte
			}{grant.c, grant.breakBody()})
		}
		snapshot := *grant
		snapshot.source = grant
		breaks = append(breaks, &snapshot)
		if (grant.lease && grant.state&6 == 0) || (!grant.lease && grant.level == 1) {
			g.finishBreakLocked(grant)
		}
	}
	g.mu.Unlock()
	for _, notification := range notifications {
		notification.c.sendBreak(notification.body)
	}
	return breaks
}

// breakBody always revokes to NONE; only READ-only leases and level II oplocks need no ACK.
func (grant *oplockGrant) breakBody() []byte {
	if grant.lease {
		b := make([]byte, 44)
		binary.LittleEndian.PutUint16(b, 44)
		binary.LittleEndian.PutUint16(b[2:], grant.epoch)
		if grant.state&6 != 0 {
			binary.LittleEndian.PutUint32(b[4:], 1)
		}
		copy(b[8:], grant.key[:])
		binary.LittleEndian.PutUint32(b[24:], grant.state)
		return b
	}
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b, 24)
	putOpenID(b[8:], grant.owner.id)
	return b
}

// finishBreakLocked revokes a cache group and wakes every waiter for this break.
func (g *openRegistry) finishBreakLocked(grant *oplockGrant) {
	grant.state, grant.level = 0, 0
	for o, previous := range g.grants {
		if previous != grant {
			continue
		}
		o.mu.Lock()
		o.leaseState = 0
		if !grant.lease {
			o.oplock = 0
		}
		o.mu.Unlock()
	}
	if grant.timer != nil {
		grant.timer.Stop()
		grant.timer = nil
	}
	if grant.done != nil {
		close(grant.done)
		grant.done = nil
	}
}

// waitBreaks waits for acknowledgements or the grant-owned timeout. Cancelling an
// operation does not cancel or extend the outstanding cache break's deadline.
func (g *openRegistry) waitBreaks(breaks []*oplockGrant, cancelled <-chan struct{}) bool {
	for _, grant := range breaks {
		select {
		case <-grant.done:
		case <-cancelled:
			return false
		}
	}
	return true
}

// dropCaching keeps a shared lease until its last open closes. If its notification
// transport closes during a break, another surviving transport receives the break.
func (g *openRegistry) dropCaching(o *open) {
	g.mu.Lock()
	grant := g.grants[o]
	var previous, resend *conn
	var body []byte
	if grant != nil {
		var replacement *open
		o.mu.Lock()
		p := o.path
		o.mu.Unlock()
		// A lease key names one path, so the opens sharing grant are all at p.
		for other := range g.byPath[p] {
			if previous := g.grants[other]; other != o && previous == grant {
				replacement = other
				if other.tree.sess.conn != grant.c {
					break
				}
			}
		}
		if replacement == nil {
			g.finishBreakLocked(grant)
		} else if grant.owner == o {
			previous = grant.c
			grant.owner = replacement
			grant.c = replacement.tree.sess.conn
			if grant.done != nil && grant.c != previous {
				resend, body = grant.c, grant.breakBody()
			}
		}
		delete(g.grants, o)
	}
	g.mu.Unlock()
	if resend == nil {
		return
	}
	// A connection's lock is never taken under g.mu: request.async holds one
	// while it waits for a tree's lock, which publishOpen holds while it waits
	// for g.mu. A connection never reopens, so checking after the fact is safe.
	previous.mu.Lock()
	disconnected := previous.closed
	previous.mu.Unlock()
	if disconnected {
		resend.sendBreak(body)
	}
}

// handleOplockBreak accepts only acknowledgements for breaks issued to the authenticated client.
func handleOplockBreak(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 2 {
		return nil, statusInvalidParameter
	}
	size := binary.LittleEndian.Uint16(b)
	g := &r.c.srv.st.opens
	if size == 24 {
		if len(b) < 24 || b[2] > 1 {
			return nil, statusInvalidParameter
		}
		o, st := r.openFile(getOpenID(b[8:]))
		if st != statusSuccess {
			return nil, st
		}
		g.mu.Lock()
		grant := g.grants[o]
		if grant == nil || grant.lease || grant.done == nil || b[2] != 0 {
			g.mu.Unlock()
			return nil, statusInvalidOplockProtocol
		}
		g.finishBreakLocked(grant)
		g.mu.Unlock()
		out := make([]byte, 24)
		binary.LittleEndian.PutUint16(out, 24)
		putOpenID(out[8:], o.id)
		return out, statusSuccess
	}
	if size != 36 || len(b) < 36 {
		return nil, statusInvalidParameter
	}
	var key [16]byte
	copy(key[:], b[8:24])
	g.mu.Lock()
	var found *oplockGrant
	for _, grant := range g.grants {
		if grant.lease && grant.key == key && grant.c.clientGUID == r.c.clientGUID {
			found = grant
			break
		}
	}
	if found == nil {
		g.mu.Unlock()
		return nil, statusObjectNameNotFound
	}
	if found.done == nil {
		g.mu.Unlock()
		return nil, statusUnsuccessful
	}
	// Every break issued here targets NONE, so no caching bit is acceptable.
	if binary.LittleEndian.Uint32(b[24:]) != 0 {
		g.mu.Unlock()
		return nil, statusRequestNotAccepted
	}
	g.finishBreakLocked(found)
	g.mu.Unlock()
	out := make([]byte, 36)
	binary.LittleEndian.PutUint16(out, 36)
	copy(out[8:], key[:])
	return out, statusSuccess
}

const (
	statusUnsuccessful          uint32 = 0xc0000001
	statusInvalidOplockProtocol uint32 = 0xc00000e3
)
