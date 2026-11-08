package fetch

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadPacketKinds(t *testing.T) {
	p := newPktReader(strings.NewReader("0006a\n0005a0000000100020004000aERR x\n0009FFFF\n"))
	want := []struct {
		kind packetKind
		data string
	}{
		{pktData, "a\n"}, {pktData, "a"}, {pktFlush, ""}, {pktDelim, ""},
		{pktResponseEnd, ""}, {pktData, ""}, {pktData, "ERR x\n"}, {pktData, "FFFF\n"},
	}
	for i, w := range want {
		kind, data, err := p.readPacket()
		if err != nil || kind != w.kind || string(data) != w.data {
			t.Fatalf("packet %d = %v %q %v, want %v %q", i, kind, data, err, w.kind, w.data)
		}
	}
	if _, _, err := p.readPacket(); err != io.EOF {
		t.Fatalf("at end: %v, want io.EOF", err)
	}
}

func TestReadPacketMalformed(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0003", "bad pkt-line length 3"},
		{"fff1" + strings.Repeat("x", 0xfff1-4), "bad pkt-line length 65521"},
		{"00zz", "bad pkt-line length"},
		{"000ashort", "unexpected EOF"},
		{"00", "unexpected EOF"},
	} {
		_, _, err := newPktReader(strings.NewReader(tc.in)).readPacket()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%.8q: err %v, want %q", tc.in, err, tc.want)
		}
	}
	// The largest legal packet.
	big := "fff0" + strings.Repeat("y", maxPacketPayload)
	if _, data, err := newPktReader(strings.NewReader(big)).readPacket(); err != nil || len(data) != maxPacketPayload {
		t.Fatalf("max packet: %d bytes, %v", len(data), err)
	}
}

func TestReadLineErrAndLF(t *testing.T) {
	p := newPktReader(strings.NewReader("000cversion\n000bversion0011ERR not ours\n"))
	for range 2 {
		if _, line, err := p.readLine(); err != nil || line != "version" {
			t.Fatalf("line %q %v", line, err)
		}
	}
	_, _, err := p.readLine()
	var re *remoteError
	if !errors.As(err, &re) || re.msg != "not ours" {
		t.Fatalf("ERR packet: %v", err)
	}
}

func TestWriterFraming(t *testing.T) {
	var w pktWriter
	w.line("want %s", "x")
	w.data([]byte{})
	w.delim()
	w.flush()
	if got := string(w.buf); got != "000bwant x\n000400010000" || w.err != nil {
		t.Fatalf("got %q, %v", got, w.err)
	}
	// An oversized payload fails the request instead of panicking, and
	// nothing more is appended.
	before := len(w.buf)
	w.data(make([]byte, maxPacketPayload+1))
	w.line("more")
	if w.err == nil || !strings.Contains(w.err.Error(), "exceeds") || len(w.buf) != before {
		t.Fatalf("oversized payload: err %v, %d bytes buffered", w.err, len(w.buf))
	}
}

func TestSideband(t *testing.T) {
	var w pktWriter
	w.data([]byte("\x01PA"))
	w.data([]byte("\x02Counting objects: 1\r"))
	w.data([]byte("\x02")) // v2 keepalive
	w.data([]byte("\x01CK"))
	w.flush()
	w.line("after")
	p := newPktReader(strings.NewReader(string(w.buf)))
	got, err := io.ReadAll(&sidebandReader{p: p})
	if err != nil || string(got) != "PACK" {
		t.Fatalf("got %q, %v", got, err)
	}
	// The stream stops at the flush; what follows is still readable.
	if _, line, _ := p.readLine(); line != "after" {
		t.Fatalf("after flush: %q", line)
	}
}

func TestSidebandErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		pkts []string
		want string
	}{
		{"band3", []string{"\x01PA", "\x03pack-objects died\n"}, "remote error: pack-objects died"},
		{"no flush", []string{"\x01PA"}, "unexpected EOF"},
		{"bad band", []string{"\x05x"}, "bad side-band 5"},
		{"empty", []string{""}, "empty side-band packet"},
	} {
		var w pktWriter
		for _, p := range tc.pkts {
			w.data([]byte(p))
		}
		_, err := io.ReadAll(&sidebandReader{p: newPktReader(strings.NewReader(string(w.buf)))})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	var w pktWriter
	w.data([]byte("\x01x"))
	w.delim()
	if _, err := io.ReadAll(&sidebandReader{p: newPktReader(strings.NewReader(string(w.buf)))}); !errors.Is(err, errProtocol) {
		t.Errorf("delim in pack stream: %v", err)
	}
}
