package object

import (
	"bytes"
	"fmt"
)

// CommitObject is a decoded commit object.
type CommitObject struct {
	Tree      ID
	Parents   []ID
	Author    Signature
	Committer Signature
	// Encoding is the "encoding" header naming the message's charset; "" means UTF-8.
	Encoding string
	// Extra holds every other header (gpgsig, mergetag, ...) in stored order.
	Extra []ExtraHeader
	// Message is everything after the blank line ending the headers.
	Message string
}

// TagObject is a decoded annotated tag object.
type TagObject struct {
	Object ID
	Type   Kind
	// Name is the "tag" header, the tag's short name.
	Name string
	// Tagger is the zero Signature for old tags that lack the header.
	Tagger Signature
	// Extra holds every other header in stored order.
	Extra []ExtraHeader
	// Message is everything after the blank line ending the headers,
	// including any trailing signature block.
	Message string
}

// ExtraHeader is a commit or tag header the decoder does not interpret.
// Continuation lines are joined to Value with "\n", their leading space removed.
type ExtraHeader struct {
	Key, Value string
}

// ParseCommit decodes a commit object of format f. A missing or malformed tree
// or parent is an error; a signature whose time cannot be parsed keeps its
// name and email with a zero When, since such commits exist in old histories.
func ParseCommit(f Format, data []byte) (*CommitObject, error) {
	headers, msg := splitHeaders(data)
	c := &CommitObject{Message: msg}
	haveTree := false
	for _, h := range headers {
		var err error
		switch h.key {
		case "tree":
			if haveTree {
				c.Extra = append(c.Extra, h.extra())
				continue
			}
			c.Tree, err = ParseIDFormat(f, string(h.value))
			haveTree = true
		case "parent":
			var p ID
			p, err = ParseIDFormat(f, string(h.value))
			c.Parents = append(c.Parents, p)
		case "author":
			c.Author = parseSignatureLenient(h.value)
		case "committer":
			c.Committer = parseSignatureLenient(h.value)
		case "encoding":
			c.Encoding = string(h.value)
		default:
			c.Extra = append(c.Extra, h.extra())
		}
		if err != nil {
			return nil, fmt.Errorf("malformed commit: %w", err)
		}
	}
	if !haveTree {
		return nil, fmt.Errorf("malformed commit: no tree header")
	}
	return c, nil
}

// ParseTag decodes an annotated tag object of format f. The object and type
// headers are required.
func ParseTag(f Format, data []byte) (*TagObject, error) {
	headers, msg := splitHeaders(data)
	t := &TagObject{Message: msg}
	haveObject := false
	for _, h := range headers {
		var err error
		switch h.key {
		case "object":
			t.Object, err = ParseIDFormat(f, string(h.value))
			haveObject = true
		case "type":
			t.Type, err = ParseKind(string(h.value))
		case "tag":
			t.Name = string(h.value)
		case "tagger":
			t.Tagger = parseSignatureLenient(h.value)
		default:
			t.Extra = append(t.Extra, h.extra())
		}
		if err != nil {
			return nil, fmt.Errorf("malformed tag: %w", err)
		}
	}
	if !haveObject || t.Type == 0 {
		return nil, fmt.Errorf("malformed tag: missing object or type header")
	}
	return t, nil
}

type rawHeader struct {
	key   string
	value []byte
}

func (h rawHeader) extra() ExtraHeader { return ExtraHeader{Key: h.key, Value: string(h.value)} }

// splitHeaders splits a commit or tag into its "key value" headers and the
// message following the first empty line.
func splitHeaders(data []byte) ([]rawHeader, string) {
	var headers []rawHeader
	for len(data) > 0 {
		line, rest, found := bytes.Cut(data, []byte{'\n'})
		if len(line) == 0 && found {
			return headers, string(rest)
		}
		data = rest
		if line[0] == ' ' && len(headers) > 0 {
			last := &headers[len(headers)-1]
			// Clone so appending never writes into the object's bytes.
			last.value = append(append(bytes.Clone(last.value), '\n'), line[1:]...)
			continue
		}
		key, value, _ := bytes.Cut(line, []byte{' '})
		headers = append(headers, rawHeader{key: string(key), value: value})
	}
	return headers, ""
}

// parseSignatureLenient decodes a signature, keeping name and email when only
// the timestamp is malformed, as git's own readers do.
func parseSignatureLenient(v []byte) Signature {
	if s, err := ParseSignature(v); err == nil {
		return s
	}
	end := bytes.LastIndexByte(v, '>')
	start := bytes.LastIndexByte(v[:max(end, 0)], '<')
	if end < 0 || start < 0 {
		return Signature{Name: string(bytes.TrimSpace(v))}
	}
	return Signature{
		Name:  string(bytes.TrimRight(v[:start], " ")),
		Email: string(v[start+1 : end]),
	}
}
