package fetch

import (
	"fmt"
	"io"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// agent is what this client calls itself in the agent capability and the
// HTTP User-Agent; servers such as GitHub expect the "git/" prefix.
const agent = "git/2.56.0-ovnfs"

// advertisement is the server's first message: a v2 capability list or a
// v0/v1 ref advertisement, of which only capabilities and shallow lines matter
// when fetching by object id.
type advertisement struct {
	v2 bool
	// caps maps capability names to values ("" when valueless).
	caps map[string]string
	// shallow lists the commits a shallow v0 server advertised.
	shallow []object.ID
}

func (a *advertisement) has(name string) bool {
	_, ok := a.caps[name]
	return ok
}

// fetchFeature reports a v2 fetch feature such as "shallow".
func (a *advertisement) fetchFeature(name string) bool {
	return a.v2 && strings.Contains(" "+a.caps["fetch"]+" ", " "+name+" ")
}

// objectFormat is the server's hash format; servers that do not say are SHA1.
func (a *advertisement) objectFormat() (object.Format, error) {
	name, ok := a.caps["object-format"]
	if !ok {
		return object.SHA1, nil
	}
	return object.ParseFormat(name)
}

// readAdvertisement reads the capability advertisement (v2) or the ref
// advertisement (v0, optionally preceded by "version 1").
func readAdvertisement(p *pktReader) (*advertisement, error) {
	adv := &advertisement{caps: map[string]string{}}
	kind, line, err := p.readLine()
	if err != nil {
		return nil, advertisementError(err)
	}
	if kind == pktData && line == "version 2" {
		adv.v2 = true
		for {
			kind, line, err := p.readLine()
			if err != nil {
				return nil, advertisementError(err)
			}
			if kind == pktFlush {
				return adv, nil
			}
			if kind != pktData {
				return nil, fmt.Errorf("%w: unexpected %v in capability advertisement", errProtocol, kind)
			}
			k, v, _ := strings.Cut(line, "=")
			adv.caps[k] = v
		}
	}
	if kind == pktData && line == "version 1" {
		kind, line, err = p.readLine()
		if err != nil {
			return nil, advertisementError(err)
		}
	}
	first := true
	for ; ; kind, line, err = p.readLine() {
		if err != nil {
			return nil, advertisementError(err)
		}
		if kind == pktFlush {
			return adv, nil
		}
		if kind != pktData {
			return nil, fmt.Errorf("%w: unexpected %v in ref advertisement", errProtocol, kind)
		}
		if first {
			first = false
			ref, caps, _ := strings.Cut(line, "\x00")
			for c := range strings.FieldsSeq(caps) {
				k, v, _ := strings.Cut(c, "=")
				adv.caps[k] = v
			}
			line = ref
		}
		if hex, ok := strings.CutPrefix(line, "shallow "); ok {
			id, err := object.ParseID(hex)
			if err != nil {
				return nil, fmt.Errorf("%w: bad shallow line %q", errProtocol, line)
			}
			adv.shallow = append(adv.shallow, id)
		}
	}
}

func advertisementError(err error) error {
	if err == io.EOF {
		return fmt.Errorf("fetch: the remote end hung up before advertising capabilities")
	}
	return err
}

// request is one fetch's negotiation input.
type request struct {
	format  object.Format
	wants   []object.ID
	haves   []object.ID
	shallow []object.ID
	depth   int
	// thin asks for a thin pack, which the receiver completes from haves.
	thin bool
}

// check refuses a server that cannot serve the request.
func (r *request) check(adv *advertisement) error {
	f, err := adv.objectFormat()
	if err != nil {
		return fmt.Errorf("fetch: server object format: %w", err)
	}
	if f != r.format {
		return fmt.Errorf("fetch: the remote repository uses %s object names, the target uses %s", f, r.format)
	}
	if adv.v2 && !adv.has("fetch") {
		return fmt.Errorf("fetch: server does not offer the fetch command")
	}
	shallowOK := adv.fetchFeature("shallow") || !adv.v2 && adv.has("shallow")
	if (r.depth > 0 || len(r.shallow) > 0) && !shallowOK {
		return fmt.Errorf("fetch: server does not support shallow clients")
	}
	return nil
}

// encodeV2 builds a v2 "command=fetch" request that ends negotiation at once
// ("done"): the haves are the target's ref tips.
func (r *request) encodeV2(adv *advertisement) ([]byte, error) {
	var w pktWriter
	w.line("command=fetch")
	if adv.has("agent") {
		w.line("agent=%s", agent)
	}
	if adv.has("object-format") {
		w.line("object-format=%s", r.format)
	}
	w.delim()
	if r.thin {
		w.line("thin-pack")
	}
	w.line("no-progress")
	w.line("ofs-delta")
	if r.depth > 0 {
		w.line("deepen %d", r.depth)
	}
	for _, id := range r.shallow {
		w.line("shallow %s", id)
	}
	for _, id := range r.wants {
		w.line("want %s", id)
	}
	for _, id := range r.haves {
		w.line("have %s", id)
	}
	w.line("done")
	w.flush()
	return w.buf, w.err
}

// bandMode is how a v0 server frames the pack.
type bandMode uint8

const (
	bandNone  bandMode = iota // raw pack bytes after the negotiation
	bandSmall                 // side-band: 1000-byte packets
	band64k                   // side-band-64k
)

// encodeV0 builds a v0 upload-request followed by the haves and "done",
// usable both on a stateful connection and as one stateless HTTP POST.
// Multi-ack is not requested, so the server answers with exactly one ACK or
// NAK after the shallow update.
func (r *request) encodeV0(adv *advertisement) ([]byte, bandMode, error) {
	caps := []string{}
	mode := bandNone
	switch {
	case adv.has("side-band-64k"):
		caps, mode = append(caps, "side-band-64k"), band64k
	case adv.has("side-band"):
		caps, mode = append(caps, "side-band"), bandSmall
	}
	for _, c := range []string{"ofs-delta", "no-progress"} {
		if adv.has(c) {
			caps = append(caps, c)
		}
	}
	if r.thin && adv.has("thin-pack") {
		caps = append(caps, "thin-pack")
	}
	if r.depth > 0 || len(r.shallow) > 0 {
		caps = append(caps, "shallow")
	}
	if adv.has("agent") {
		caps = append(caps, "agent="+agent)
	}
	if adv.has("object-format") {
		caps = append(caps, "object-format="+r.format.String())
	}
	var w pktWriter
	for i, id := range r.wants {
		if i == 0 {
			w.line("want %s %s", id, strings.Join(caps, " "))
		} else {
			w.line("want %s", id)
		}
	}
	for _, id := range r.shallow {
		w.line("shallow %s", id)
	}
	if r.depth > 0 {
		w.line("deepen %d", r.depth)
	}
	w.flush()
	for _, id := range r.haves {
		w.line("have %s", id)
	}
	w.line("done")
	return w.buf, mode, w.err
}

// maxShallowLines bounds the shallow lines a server may send beyond one
// unshallow per commit the client called shallow: a real boundary is one
// line per cut commit, far fewer than this.
const maxShallowLines = 1 << 20

// shallowLimit is the most shallow-info lines the request can justify.
func (r *request) shallowLimit() int { return maxShallowLines + len(r.shallow) }

// shallowUpdate is the server's verdict on the shallow boundary.
type shallowUpdate struct {
	shallow, unshallow []object.ID
	limit              int // lines accepted before the server is refused
}

// parseShallowLine records a "shallow <id>" or "unshallow <id>" line.
func (u *shallowUpdate) parse(line string, f object.Format) error {
	if len(u.shallow)+len(u.unshallow) >= u.limit {
		return fmt.Errorf("%w: more than %d shallow-info lines", errProtocol, u.limit)
	}
	verb, hex, _ := strings.Cut(line, " ")
	id, err := object.ParseIDFormat(f, hex)
	if err != nil {
		return fmt.Errorf("%w: bad shallow-info line %q", errProtocol, line)
	}
	switch verb {
	case "shallow":
		u.shallow = append(u.shallow, id)
	case "unshallow":
		u.unshallow = append(u.unshallow, id)
	default:
		return fmt.Errorf("%w: unexpected line %q in shallow-info", errProtocol, line)
	}
	return nil
}

// receiveFunc stores the pack read from r.
type receiveFunc func(r io.Reader) error

// readV2Response reads a v2 fetch response: optional sections, then the
// side-band multiplexed packfile, passed to recv.
func readV2Response(p *pktReader, f object.Format, shallowLimit int, recv receiveFunc) (shallowUpdate, error) {
	su := shallowUpdate{limit: shallowLimit}
	for {
		kind, header, err := p.readLine()
		if err != nil {
			return su, responseError(err)
		}
		if kind != pktData {
			return su, fmt.Errorf("%w: expected a section header, got %v", errProtocol, kind)
		}
		if header == "packfile" {
			if err := receiveBanded(p, recv); err != nil {
				return su, err
			}
			return su, nil
		}
		for {
			kind, line, err := p.readLine()
			if err != nil {
				return su, responseError(err)
			}
			if kind == pktDelim {
				break
			}
			if kind != pktData {
				// An acknowledgments section ending in a flush means no pack
				// follows, which "done" should have prevented.
				return su, fmt.Errorf("%w: response ended without a packfile", errProtocol)
			}
			if header == "shallow-info" {
				if err := su.parse(line, f); err != nil {
					return su, err
				}
			}
			// acknowledgments, wanted-refs, packfile-uris: nothing to use.
		}
	}
}

// readV0Response reads a v0 response to encodeV0's request: the shallow
// update when a depth was requested, one ACK or NAK, then the pack.
func readV0Response(p *pktReader, f object.Format, deepen bool, shallowLimit int, mode bandMode, recv receiveFunc) (shallowUpdate, error) {
	su := shallowUpdate{limit: shallowLimit}
	if deepen {
		for {
			kind, line, err := p.readLine()
			if err != nil {
				return su, responseError(err)
			}
			if kind == pktFlush {
				break
			}
			if err := su.parse(line, f); err != nil {
				return su, err
			}
		}
	}
	for {
		kind, line, err := p.readLine()
		if err != nil {
			return su, responseError(err)
		}
		if kind == pktData && (line == "NAK" || strings.HasPrefix(line, "ACK ")) {
			if strings.HasSuffix(line, " continue") || strings.HasSuffix(line, " common") || strings.HasSuffix(line, " ready") {
				continue // multi-ack lines from a server ignoring our caps
			}
			break
		}
		if kind == pktData && strings.HasPrefix(line, "shallow ") {
			// A shallow server may send its boundary unasked.
			if err := su.parse(line, f); err != nil {
				return su, err
			}
			continue
		}
		return su, fmt.Errorf("%w: expected ACK or NAK, got %q", errProtocol, line)
	}
	if mode == bandNone {
		return su, recv(p.raw())
	}
	return su, receiveBanded(p, recv)
}

// receiveBanded passes the side-band stream to recv and reads it to its
// flush, so an error the server reports after the pack is not missed.
func receiveBanded(p *pktReader, recv receiveFunc) error {
	sb := &sidebandReader{p: p}
	if err := recv(sb); err != nil {
		// A server error explains a truncated pack better than the indexer.
		if sb.err != nil && sb.err != io.EOF {
			if _, ok := sb.err.(*remoteError); ok {
				return sb.err
			}
		}
		return err
	}
	if _, err := io.Copy(io.Discard, sb); err != nil {
		return err
	}
	return nil
}

func responseError(err error) error {
	if err == io.EOF {
		return fmt.Errorf("fetch: the remote end hung up unexpectedly")
	}
	return err
}

// raw returns the stream after the last packet, for a pack sent unframed.
func (p *pktReader) raw() io.Reader { return p.r }
