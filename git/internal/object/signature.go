package object

import (
	"bytes"
	"fmt"
	"strconv"
	"time"
)

// Signature is an identity with a time, as in a commit's author and committer
// lines, a tag's tagger line and a reflog entry: "Name <email> 1700000000 +0100".
// When keeps the recorded UTC offset as a fixed zone.
type Signature struct {
	Name, Email string
	When        time.Time
}

// String returns the signature as git stores it.
func (s Signature) String() string { return string(s.Append(nil)) }

// Append appends the stored form of s to b.
func (s Signature) Append(b []byte) []byte {
	b = append(b, s.Name...)
	b = append(b, " <"...)
	b = append(b, s.Email...)
	b = append(b, "> "...)
	b = strconv.AppendInt(b, s.When.Unix(), 10)
	_, offset := s.When.Zone()
	sign := byte('+')
	if offset < 0 {
		sign, offset = '-', -offset
	}
	minutes := offset / 60
	return fmt.Appendf(append(b, ' ', sign), "%02d%02d", minutes/60, minutes%60)
}

// ParseSignature decodes a stored signature. The name is everything before the
// last " <" that precedes the email's closing '>'; a missing or malformed
// timestamp is an error.
func ParseSignature(line []byte) (Signature, error) {
	end := bytes.LastIndexByte(line, '>')
	start := bytes.LastIndex(line[:max(end, 0)], []byte("<"))
	if end < 0 || start < 0 {
		return Signature{}, fmt.Errorf("invalid signature %q", line)
	}
	s := Signature{
		Name:  string(bytes.TrimRight(line[:start], " ")),
		Email: string(line[start+1 : end]),
	}
	fields := bytes.Fields(line[end+1:])
	if len(fields) != 2 {
		return Signature{}, fmt.Errorf("invalid signature time %q", line)
	}
	seconds, err := strconv.ParseInt(string(fields[0]), 10, 64)
	if err != nil {
		return Signature{}, fmt.Errorf("invalid signature time %q", line)
	}
	tz := fields[1]
	if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return Signature{}, fmt.Errorf("invalid signature zone %q", line)
	}
	hhmm, err := strconv.Atoi(string(tz[1:]))
	if err != nil {
		return Signature{}, fmt.Errorf("invalid signature zone %q", line)
	}
	offset := (hhmm/100*60 + hhmm%100) * 60
	if tz[0] == '-' {
		offset = -offset
	}
	s.When = time.Unix(seconds, 0).In(time.FixedZone("", offset))
	return s, nil
}
