package relay

import "time"

// outQueue holds encoded frames for the link's writer: control frames are
// never dropped; datagrams are dropped once they waited longer than
// sojourn, as a router's active queue management would, so QUIC sees loss
// rather than a hidden, growing queue.
type outQueue struct {
	buf    []byte   // encoded frames, oldest first
	frames []queued // each frame in buf
	live   bool     // whether a link is up (frames for no link are refused)
	v2     bool     // whether the link speaks v2 (send-to)
}

// queued is one frame in outQueue.buf.
type queued struct {
	end      int       // where it ends in buf
	at       time.Time // when it was queued
	datagram bool      // a datagram (droppable) rather than a control frame
}

// push appends f, queued at now, unless no link is up or that would exceed
// outCap once stale datagrams are shed; whether it did.
func (o *outQueue) push(f *frame, now time.Time) bool {
	if !o.live {
		return false
	}
	n := frameLen(f)
	if len(o.buf)+n > outCap {
		o.shed(now)
		if len(o.buf)+n > outCap {
			return false
		}
	}
	o.buf = appendFrame(o.buf, f)
	o.frames = append(o.frames, queued{end: len(o.buf), at: now, datagram: f.kind == kindSend || f.kind == kindSendTo})
	return true
}

// shed drops the datagrams queued longer than sojourn before now, keeping
// every other frame in order; how many it dropped.
func (o *outQueue) shed(now time.Time) int {
	start, kept, w := 0, 0, 0
	for _, q := range o.frames {
		from, to := start, q.end
		start = to
		if q.datagram && now.Sub(q.at) > sojourn {
			continue
		}
		if from != kept {
			copy(o.buf[kept:], o.buf[from:to])
		}
		kept += to - from
		q.end = kept
		o.frames[w] = q
		w++
	}
	dropped := len(o.frames) - w
	o.frames = o.frames[:w]
	o.buf = o.buf[:kept]
	return dropped
}

// take hands the writer every frame queued, less stale datagrams: the
// encoded bytes and where each frame ends in them. The queue continues in
// spare (the writer's previous buffer); ends is reused for the frame ends.
func (o *outQueue) take(now time.Time, spare []byte, ends []int) ([]byte, []int) {
	o.shed(now)
	for _, q := range o.frames {
		ends = append(ends, q.end)
	}
	buf := o.buf
	o.buf = spare[:0]
	o.frames = o.frames[:0]
	return buf, ends
}

// clear empties the queue (the link came up or went down).
func (o *outQueue) clear() {
	o.buf = o.buf[:0]
	o.frames = o.frames[:0]
}
