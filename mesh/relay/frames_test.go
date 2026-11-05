package relay

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/mesh"
)

// vector is one line of testdata/node-frames.txt (shared with stencil's
// relay service and stencil-net).
type vector struct {
	name, use, message, reads string
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	f, err := os.Open("testdata/node-frames.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var vectors []vector
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, " | ")
		if len(fields) != 5 {
			t.Fatalf("vector line has %d fields, want 5: %q", len(fields), line)
		}
		vectors = append(vectors, vector{fields[0], fields[1], fields[2], fields[3]})
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return vectors
}

var kindNames = map[byte]string{
	kindAuth: "auth", kindConnect: "connect", kindSend: "send", kindPing: "ping",
	kindRelease: "release", kindSendTo: "send-to", kindChallenge: "challenge", kindWelcome: "welcome",
	kindRoute: "route", kindNoPeer: "no-peer", kindRecv: "recv", kindPong: "pong",
	kindGone: "gone", kindClosing: "closing",
}

func hexOrNone(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	return hex.EncodeToString(b)
}

// frameText writes f as the vectors' "what it reads as" field does.
func frameText(f *frame) string {
	var fields []string
	switch f.kind {
	case kindAuth:
		fields = []string{"node=" + hex.EncodeToString(f.peer[:]), "sig=" + hex.EncodeToString(f.sig[:])}
	case kindConnect, kindNoPeer:
		fields = []string{"peer=" + hex.EncodeToString(f.peer[:])}
	case kindChallenge:
		fields = []string{"nonce=" + hex.EncodeToString(f.nonce[:])}
	case kindSend, kindRecv:
		fields = []string{fmt.Sprintf("route=%d", f.route), "datagram=" + hexOrNone(f.datagram)}
	case kindSendTo:
		fields = []string{"peer=" + hex.EncodeToString(f.peer[:]), "datagram=" + hexOrNone(f.datagram)}
	case kindPing, kindPong:
		fields = []string{fmt.Sprintf("n=%d", f.n)}
	case kindRelease, kindGone:
		fields = []string{fmt.Sprintf("route=%d", f.route)}
	case kindWelcome:
		fields = []string{"observed=" + f.observed.String()}
	case kindRoute:
		fields = []string{"peer=" + hex.EncodeToString(f.peer[:]), fmt.Sprintf("route=%d", f.route), "observed=" + f.observed.String()}
	case kindClosing:
		fields = []string{fmt.Sprintf("reason=%d", f.reason), fmt.Sprintf("retry_ms=%d", f.retry.Milliseconds())}
	}
	return strings.Join(append([]string{kindNames[f.kind]}, fields...), " ")
}

// readText reads msg and writes what it reads as, the error included.
func readText(msg []byte) string {
	var parts []string
	rd := frameReader{rest: msg}
	for {
		f, ok := rd.next()
		if !ok {
			break
		}
		parts = append(parts, frameText(&f))
	}
	text := strings.Join(parts, "; ")
	var body bodyError
	switch {
	case errors.Is(rd.err, errTruncated):
		text = strings.TrimPrefix(text+" !truncated", " ")
	case errors.As(rd.err, &body):
		text = strings.TrimPrefix(fmt.Sprintf("%s !body=%02x", text, byte(body)), " ")
	case rd.err != nil:
		text += " !" + rd.err.Error()
	}
	return text
}

// parseFrame reads one frame of the "what it reads as" field.
func parseFrame(t *testing.T, text string) frame {
	t.Helper()
	words := strings.Fields(text)
	var f frame
	found := false
	for kind, name := range kindNames {
		if name == words[0] {
			f.kind, found = kind, true
		}
	}
	if !found {
		t.Fatalf("unknown frame kind %q", words[0])
	}
	bytesOf := func(v string) []byte {
		if v == "-" {
			return nil
		}
		b, err := hex.DecodeString(v)
		if err != nil {
			t.Fatalf("hex %q: %v", v, err)
		}
		return b
	}
	number := func(v string) uint64 {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("number %q: %v", v, err)
		}
		return n
	}
	for _, word := range words[1:] {
		key, value, _ := strings.Cut(word, "=")
		switch key {
		case "node", "peer":
			f.peer = mesh.NodeID(bytesOf(value))
		case "sig":
			f.sig = [64]byte(bytesOf(value))
		case "nonce":
			f.nonce = [32]byte(bytesOf(value))
		case "route":
			f.route = uint32(number(value))
		case "datagram":
			f.datagram = bytesOf(value)
		case "n":
			f.n = number(value)
		case "observed":
			f.observed = netip.MustParseAddrPort(value)
		case "reason":
			f.reason = closeReason(number(value))
		case "retry_ms":
			f.retry = time.Duration(number(value)) * time.Millisecond
		default:
			t.Fatalf("unknown field %q", key)
		}
	}
	return f
}

func TestFrameVectors(t *testing.T) {
	vectors := loadVectors(t)
	seen := map[string]int{}
	kinds := map[byte]bool{}
	for _, v := range vectors {
		seen[v.use]++
		if v.use == "proof" {
			continue
		}
		msg, err := hex.DecodeString(v.message)
		if err != nil {
			t.Fatalf("%s: message hex: %v", v.name, err)
		}
		if got := readText(msg); got != v.reads {
			t.Errorf("%s: reads as\n  %s\nwant\n  %s", v.name, got, v.reads)
		}
		if v.use != "both" {
			continue
		}
		var encoded []byte
		for text := range strings.SplitSeq(v.reads, "; ") {
			f := parseFrame(t, text)
			kinds[f.kind] = true
			encoded = appendFrame(encoded, &f)
		}
		if got := hex.EncodeToString(encoded); got != v.message {
			t.Errorf("%s: encodes as\n  %s\nwant\n  %s", v.name, got, v.message)
		}
	}
	if seen["both"] == 0 || seen["read"] == 0 || seen["proof"] == 0 {
		t.Fatalf("vector uses seen: %v", seen)
	}
	for kind, name := range kindNames {
		if !kinds[kind] {
			t.Errorf("no both-ways vector for %s", name)
		}
	}
}

// TestProofVector: the challenge answer signs exactly what the relay
// verifies.
func TestProofVector(t *testing.T) {
	var proof vector
	for _, v := range loadVectors(t) {
		if v.use == "proof" {
			proof = v
		}
	}
	values := map[string][]byte{}
	for _, word := range strings.Fields(proof.reads)[1:] {
		key, value, _ := strings.Cut(word, "=")
		b, err := hex.DecodeString(value)
		if err != nil {
			t.Fatalf("proof %s: %v", key, err)
		}
		values[key] = b
	}
	key := ed25519.NewKeyFromSeed(values["seed"])
	if got := key.Public().(ed25519.PublicKey); !bytes.Equal(got, values["node"]) {
		t.Fatalf("node of seed = %x, want %x", got, values["node"])
	}
	if got := ed25519.Sign(key, challengeMessage([32]byte(values["nonce"]))); !bytes.Equal(got, values["sig"]) {
		t.Fatalf("signature = %x, want %x", got, values["sig"])
	}
}
