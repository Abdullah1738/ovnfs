// Package release finds ovnfs release binaries on Stencil's build service,
// where release.yml publishes them (see the upload command beside this
// package). The installers, `ovnfs update` and `ovnfs send` download from there.
//
//	bin, err := release.Find(ctx, "", runtime.GOOS, runtime.GOARCH) // the latest
//	exe, err := bin.Fetch(ctx)                                      // checked against bin.SHA256
package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Product is the build service product ovnfs is published as.
const Product = "ovnfs"

// URL is the build service origin: OVNFS_BUILD_URL, else https://build.stencil.so.
func URL() string {
	if u := strings.TrimRight(os.Getenv("OVNFS_BUILD_URL"), "/"); u != "" {
		return u
	}
	return "https://build.stencil.so"
}

// tagPattern matches a release tag, as opposed to a development build's
// pseudo-version or "(devel)".
var tagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// IsTag reports whether version names a release (vX.Y.Z).
func IsTag(version string) bool { return tagPattern.MatchString(version) }

// Platform is a GOOS/GOARCH pair.
type Platform struct{ OS, Arch string }

// Target is where a platform's binary lives on the build service.
type Target struct {
	Platform string // build service platform: macos, linux or windows
	Arch     string // build service architecture: x86_64 or arm64
	Name     string // published file name, e.g. ovnfs-darwin-arm64
}

// Targets are every platform a release ships a binary for (.goreleaser.yaml
// builds the same set).
var Targets = map[Platform]Target{
	{"darwin", "amd64"}:  {"macos", "x86_64", "ovnfs-darwin-amd64"},
	{"darwin", "arm64"}:  {"macos", "arm64", "ovnfs-darwin-arm64"},
	{"linux", "amd64"}:   {"linux", "x86_64", "ovnfs-linux-amd64"},
	{"linux", "arm64"}:   {"linux", "arm64", "ovnfs-linux-arm64"},
	{"windows", "amd64"}: {"windows", "x86_64", "ovnfs-windows-amd64.exe"},
	{"windows", "arm64"}: {"windows", "arm64", "ovnfs-windows-arm64.exe"},
}

// Binary is one published ovnfs binary.
type Binary struct {
	Tag    string // release tag, vX.Y.Z
	Size   int64
	SHA256 string // lowercase hex
	URL    string // presigned download, valid for minutes
}

// Find asks the build service for the binary of release tag ("" for the
// latest stable release) for goos/goarch. It fails for a platform no release
// ships, a tag or platform the service has no build of (404), and an answer
// that does not name a release binary for the platform with its size and SHA-256.
func Find(ctx context.Context, tag, goos, goarch string) (Binary, error) {
	t, ok := Targets[Platform{goos, goarch}]
	if !ok {
		return Binary{}, fmt.Errorf("no ovnfs release for %s/%s", goos, goarch)
	}
	route := "latest/" + t.Platform + "-" + t.Arch + "?channel=stable"
	if tag != "" {
		route = "versions/" + url.PathEscape(strings.TrimPrefix(tag, "v")) + "/" + t.Platform + "-" + t.Arch
	}
	u := URL() + "/api/products/" + Product + "/" + route
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Binary{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Binary{}, fmt.Errorf("finding the ovnfs release: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Binary{}, fmt.Errorf("finding the ovnfs release: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Binary{}, fmt.Errorf("finding the ovnfs release: %s answered %s %s", u, resp.Status, bytes.TrimSpace(body))
	}
	var answer struct {
		Build struct {
			Version string `json:"version"`
		} `json:"build"`
		File struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"file"`
		Download string `json:"download"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return Binary{}, fmt.Errorf("finding the ovnfs release: %s: %w", u, err)
	}
	bin := Binary{Tag: "v" + answer.Build.Version, Size: answer.File.Size, SHA256: answer.File.SHA256, URL: answer.Download}
	sum, err := hex.DecodeString(bin.SHA256)
	if !IsTag(bin.Tag) || answer.File.Name != t.Name || bin.Size <= 0 || err != nil || len(sum) != sha256.Size || bin.URL == "" {
		return Binary{}, fmt.Errorf("finding the ovnfs release: unexpected answer from %s: %s", u, body)
	}
	return bin, nil
}

// Fetch downloads b, failing unless it has b's size and SHA-256.
func (b Binary) Fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Drop the *url.Error wrapper, which quotes the long presigned URL.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, fmt.Errorf("downloading ovnfs %s: %w", b.Tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading ovnfs %s: %s", b.Tag, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, b.Size+1))
	if err != nil {
		return nil, fmt.Errorf("downloading ovnfs %s: %w", b.Tag, err)
	}
	if int64(len(data)) != b.Size {
		return nil, fmt.Errorf("downloading ovnfs %s: got %d bytes, expected %d", b.Tag, len(data), b.Size)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != b.SHA256 {
		return nil, fmt.Errorf("downloading ovnfs %s: SHA-256 %x, expected %s", b.Tag, sum, b.SHA256)
	}
	return data, nil
}
