package remotefs

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// frameBytes builds a frame by hand: payload length u32 LE, type, id u32 LE, payload.
func frameBytes(ty byte, id uint32, payload string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))
	b = append(b, ty)
	b = binary.LittleEndian.AppendUint32(b, id)
	return append(b, payload...)
}

// dataPayload builds a REQ_DATA payload: JSON length u32 LE, JSON, data.
func dataPayload(json, data string) string {
	return string(binary.LittleEndian.AppendUint32(nil, uint32(len(json)))) + json + data
}

// TestHeaderLayoutGolden pins the 9-byte header to the Rust codecs.
func TestHeaderLayoutGolden(t *testing.T) {
	// o2 crates/vfs/remotefs/src/frame.rs:382: header(Unix, Req, id 7, len 3) == [3,0,0,0,1,7,0,0,0].
	got, err := appendFrame(nil, FrameReq, 7, []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{3, 0, 0, 0, 1, 7, 0, 0, 0, 'a', 'b', 'c'}; !bytes.Equal(got, want) {
		t.Fatalf("header = %v, want %v", got, want)
	}
	// vibevmm remotefs.rs:16-25 frame types, :27 HEADER_LEN, :29 MAX_FRAME.
	if FrameReq != 1 || FrameOKJSON != 2 || FrameOKData != 3 || FrameErr != 4 || FrameReqData != 5 || HeaderLen != 9 || MaxFrame != 2*1024*1024 {
		t.Fatal("frame constants drifted from vibevmm proto")
	}
}

// TestClientRequestFramesGolden checks the client emits the VMM's exact request frames.
// JSON shapes: o2 crates/vfs/remotefs/src/v1.rs:297-304 (vibevmm proto::Request,
// remotefs.rs:33 `#[serde(tag = "op", rename_all = "snake_case")]`); id 1 is the
// VMM's PROXY_REQUEST_ID (remotefs.rs:592); REQ_DATA layout is encode_data_request
// (remotefs.rs:195).
func TestClientRequestFramesGolden(t *testing.T) {
	mtime := uint64(12)
	tests := []struct {
		name  string
		call  func(*Client) error
		want  []byte
		reply []byte
	}{
		{"stat", func(c *Client) error { _, err := c.Stat("a"); return err },
			frameBytes(FrameReq, 1, `{"op":"stat","tag":"home","path":"a"}`),
			frameBytes(FrameOKJSON, 1, `{"kind":"file","size":1,"mtime":2,"etag":null}`)},
		{"list", func(c *Client) error { _, err := c.List(""); return err },
			frameBytes(FrameReq, 1, `{"op":"list","tag":"home","path":""}`),
			frameBytes(FrameOKJSON, 1, `{"entries":[]}`)},
		{"read", func(c *Client) error { _, err := c.Read("a", 1, 3); return err },
			frameBytes(FrameReq, 1, `{"op":"read","tag":"home","path":"a","offset":1,"len":3}`),
			frameBytes(FrameOKData, 1, "abc")},
		{"create", func(c *Client) error { _, err := c.Create("a", 420, 0, 0, true, false); return err },
			frameBytes(FrameReq, 1, `{"op":"create","tag":"home","path":"a","mode":420,"uid":0,"gid":0,"excl":true,"trunc":false}`),
			frameBytes(FrameOKJSON, 1, `{"kind":"file","size":0,"mtime":2,"etag":null}`)},
		{"write", func(c *Client) error { _, err := c.Write("a", 5, []byte("bytes")); return err },
			frameBytes(FrameReqData, 1, dataPayload(`{"op":"write","tag":"home","path":"a","offset":5}`, "bytes")),
			frameBytes(FrameOKJSON, 1, `{"written":5}`)},
		{"setattr", func(c *Client) error { _, err := c.Setattr("a", SetAttr{Mtime: &mtime}); return err },
			frameBytes(FrameReq, 1, `{"op":"setattr","tag":"home","path":"a","mtime":12}`),
			frameBytes(FrameOKJSON, 1, `{"kind":"file","size":0,"mtime":12,"etag":null}`)},
		{"sync", func(c *Client) error { return c.Sync("a", true) },
			frameBytes(FrameReq, 1, `{"op":"sync","tag":"home","path":"a","release":true}`),
			frameBytes(FrameOKJSON, 1, `{}`)},
		{"rename", func(c *Client) error { return c.Rename("a", "b", true) },
			frameBytes(FrameReq, 1, `{"op":"rename","tag":"home","from":"a","to":"b","noreplace":true}`),
			frameBytes(FrameOKJSON, 1, `{}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			c := NewClient(client, "home")
			defer c.Close()
			done := make(chan error, 1)
			go func() { done <- tt.call(c) }()
			got := make([]byte, len(tt.want))
			if _, err := io.ReadFull(server, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("frame\n got %q\nwant %q", got, tt.want)
			}
			if _, err := server.Write(tt.reply); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("call: %v", err)
			}
		})
	}
}

// fakeInfo is a FileInfo with chosen attributes.
type fakeInfo struct {
	size  int64
	mode  os.FileMode
	mtime time.Time
	sys   any
}

// Name returns a fixed name.
func (i fakeInfo) Name() string { return "x" }

// Size returns the chosen size.
func (i fakeInfo) Size() int64 { return i.size }

// Mode returns the chosen mode.
func (i fakeInfo) Mode() os.FileMode { return i.mode }

// ModTime returns the chosen mtime.
func (i fakeInfo) ModTime() time.Time { return i.mtime }

// IsDir reports whether the mode is a directory.
func (i fakeInfo) IsDir() bool { return i.mode.IsDir() }

// Sys returns the chosen system metadata.
func (i fakeInfo) Sys() any { return i.sys }

// TestStatReplyGolden checks stat replies keep the legacy wire shape plus attributes.
func TestStatReplyGolden(t *testing.T) {
	c := &conn{server: &server{}, enc: newEncoder()}
	tests := []struct {
		name string
		info fakeInfo
		want string
	}{
		// o2 crates/vfs/remotefs/src/v1.rs:333 (a 0777 node, size 3, mtime 2.999 s, uid 1, gid 2).
		{"file", fakeInfo{3, 0o777, time.Unix(2, 999_999_999), unixstat.Info{UID: 1, GID: 2}},
			`{"kind":"file","size":3,"mtime":2,"etag":null,"mode":511,"uid":1,"gid":2}`},
		// o2 v1.rs:335-336: directories report size 0 and pre-epoch mtimes clamp to 0.
		{"dir", fakeInfo{4096, os.ModeDir | 0o755, time.Unix(-1, 0), unixstat.Info{UID: 1, GID: 2}},
			`{"kind":"dir","size":0,"mtime":0,"etag":null,"mode":493,"uid":1,"gid":2}`},
		{"special bits", fakeInfo{0, os.ModeDir | os.ModeSticky | os.ModeSetgid | 0o777, time.Unix(5, 0), unixstat.Info{}},
			`{"kind":"dir","size":0,"mtime":5,"etag":null,"mode":2047,"uid":0,"gid":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := c.ok(c.stat(tt.info))
			if err != nil {
				t.Fatal(err)
			}
			if string(r.body) != tt.want {
				t.Fatalf("stat reply\n got %s\nwant %s", r.body, tt.want)
			}
		})
	}
}

// TestServerReplyFramesGolden drives the server with raw VMM frames and pins the reply bytes.
func TestServerReplyFramesGolden(t *testing.T) {
	share := testShare(t, true)
	writeCheckout(t, share, "a", "hello")
	tests := []struct {
		name string
		req  []byte
		want []byte
	}{
		// A v2 hello as o2 sends it: wire.rs:104,107 (Request::Hello, tag "op"), hello.rs:134
		// (Hello::v2 offers [2,1]), hello.rs:194 (features ["bin_attrs"]). Answered with an
		// ERR whose shape is vibevmm proto::ErrReply (remotefs.rs:156-163).
		{"hello", frameBytes(FrameReq, 1, `{"op":"hello","versions":[2,1],"features":["bin_attrs"]}`),
			frameBytes(FrameErr, 1, `{"code":"unsupported","msg":"remote-fs v2 is not served; v1 clients send no hello"}`)},
		{"read", frameBytes(FrameReq, 1, `{"op":"read","tag":"home","path":"a","offset":1,"len":3}`),
			frameBytes(FrameOKData, 1, "ell")},
		{"write", frameBytes(FrameReqData, 1, dataPayload(`{"op":"write","tag":"home","path":"a","offset":5}`, "!!")),
			frameBytes(FrameOKJSON, 1, `{"written":2}`)},
		{"sync", frameBytes(FrameReq, 1, `{"op":"sync","tag":"home","path":"a","release":true}`),
			frameBytes(FrameOKJSON, 1, `{}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "sync" && runtime.GOOS == "windows" {
				t.Skip("remotefs is not served on Windows, whose FlushFileBuffers needs the writable handle sync does not open")
			}
			client, srv := net.Pipe()
			defer client.Close()
			s := &server{share: share}
			go s.serveConn(srv)
			if _, err := client.Write(tt.req); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(tt.want))
			if _, err := io.ReadFull(client, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("reply\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}
