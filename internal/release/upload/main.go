// Command upload publishes a release's binaries to Stencil's build service,
// which the installers, `ovnfs update` and `ovnfs send` download them from.
//
//	go run ./internal/release/upload v1.2.3
//
// release.yml runs it after GoReleaser, which lists the binaries it built in
// dist/artifacts.json; every platform in [release.Targets] must be there. It
// authenticates with the run's GitHub OIDC token for audience build.stencil.so
// (the job needs `permissions: id-token: write`), which the service accepts
// from stencil-hq/ovnfs runs of tag v1.2.3. It declares the build (version and
// every binary's target, size and SHA-256), PUTs each binary to the presigned
// URL the service returns, then publishes the build. OVNFS_BUILD_URL replaces
// the service origin.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/internal/release"
)

// audience is the OIDC audience the build service accepts tokens for.
const audience = "build.stencil.so"

// attempts bounds the tries of a request that fails transiently.
const attempts = 3

// file is one binary as the build service declares it.
type file struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Kind     string `json:"kind"`
	Size     int    `json:"size"`
	SHA256   string `json:"sha256"`
	data     []byte
}

// upload is where the service takes one declared file's bytes.
type upload struct {
	Name string `json:"name"`
	URL  string `json:"url"` // presigned PUT
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "upload:", err)
		os.Exit(1)
	}
}

// run publishes release tag args[0].
func run(ctx context.Context, args []string) error {
	if len(args) != 1 || !release.IsTag(args[0]) {
		return errors.New("usage: upload vX.Y.Z")
	}
	version := strings.TrimPrefix(args[0], "v")
	files, err := binaries("dist/artifacts.json")
	if err != nil {
		return err
	}
	token, err := oidcToken(ctx)
	if err != nil {
		return err
	}
	auth := "Bearer " + token
	builds := release.URL() + "/api/products/" + release.Product + "/builds"

	declared, err := json.Marshal(map[string]any{"version": version, "channel": "stable", "files": files})
	if err != nil {
		return err
	}
	// One attempt: a retry after a lost answer would find the build taken (409).
	answer, err := do(ctx, 1, http.MethodPost, builds, http.Header{"Authorization": {auth}, "Content-Type": {"application/json"}}, declared)
	if err != nil {
		return fmt.Errorf("creating the build: %w", err)
	}
	var created struct {
		Build struct {
			ID string `json:"id"`
		} `json:"build"`
		Uploads []upload `json:"uploads"`
	}
	if err := json.Unmarshal(answer, &created); err != nil {
		return fmt.Errorf("creating the build: %w", err)
	}
	fmt.Printf("created %s build %s (%s)\n", release.Product, created.Build.ID, version)

	for _, f := range files {
		i := slices.IndexFunc(created.Uploads, func(u upload) bool { return u.Name == f.Name })
		if i < 0 {
			return fmt.Errorf("the service returned no upload URL for %s", f.Name)
		}
		fmt.Printf("uploading %s\n", f.Name)
		// The presigned URL takes the bare body: no auth, no content type.
		if _, err := do(ctx, attempts, http.MethodPut, created.Uploads[i].URL, nil, f.data); err != nil {
			return fmt.Errorf("uploading %s: %w", f.Name, err)
		}
	}

	publish := builds + "/" + url.PathEscape(created.Build.ID) + "/publish"
	if _, err := do(ctx, attempts, http.MethodPost, publish, http.Header{"Authorization": {auth}}, nil); err != nil {
		return fmt.Errorf("publishing build %s: %w", created.Build.ID, err)
	}
	fmt.Printf("published %s build %s\n", release.Product, created.Build.ID)
	return nil
}

// binaries reads the binaries GoReleaser lists in artifacts, one for each of
// [release.Targets], named and placed as the build service publishes them.
func binaries(artifacts string) ([]file, error) {
	raw, err := os.ReadFile(artifacts)
	if err != nil {
		return nil, err
	}
	var listed []struct {
		Type   string `json:"type"`
		Path   string `json:"path"`
		Goos   string `json:"goos"`
		Goarch string `json:"goarch"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, fmt.Errorf("%s: %w", artifacts, err)
	}
	var files []file
	for _, a := range listed {
		if a.Type != "Binary" {
			continue
		}
		t, ok := release.Targets[release.Platform{OS: a.Goos, Arch: a.Goarch}]
		if !ok {
			return nil, fmt.Errorf("%s: %s is for %s/%s, which has no build service target", artifacts, a.Path, a.Goos, a.Goarch)
		}
		if slices.ContainsFunc(files, func(f file) bool { return f.Name == t.Name }) {
			return nil, fmt.Errorf("%s: two binaries for %s/%s", artifacts, a.Goos, a.Goarch)
		}
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		f := file{Name: t.Name, Platform: t.Platform, Arch: t.Arch, Kind: "archive", Size: len(data), SHA256: hex.EncodeToString(sum[:]), data: data}
		fmt.Printf("%s\t%s-%s\t%d\t%s\n", f.Name, f.Platform, f.Arch, f.Size, f.SHA256)
		files = append(files, f)
	}
	if len(files) != len(release.Targets) {
		return nil, fmt.Errorf("%s: %d binaries, want one for each of %d platforms", artifacts, len(files), len(release.Targets))
	}
	return files, nil
}

// oidcToken requests this GitHub Actions run's OIDC token for the build service.
func oidcToken(ctx context.Context) (string, error) {
	requestURL, requestToken := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if requestURL == "" || requestToken == "" {
		return "", errors.New("ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN unset: run in GitHub Actions with permissions id-token: write")
	}
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	answer, err := do(ctx, attempts, http.MethodGet, u.String(), http.Header{"Authorization": {"bearer " + requestToken}}, nil)
	if err != nil {
		return "", fmt.Errorf("requesting the OIDC token: %w", err)
	}
	var token struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(answer, &token); err != nil || token.Value == "" {
		return "", fmt.Errorf("requesting the OIDC token: no token in %.200s", answer)
	}
	return token.Value, nil
}

// do sends a request up to tries times while the network fails or the answer
// is transient (408, 429, 5xx), and returns the body of a 2xx answer. Errors
// quote the answer, never the URL, which may be presigned.
func do(ctx context.Context, tries int, method, target string, header http.Header, body []byte) ([]byte, error) {
	for try := 1; ; try++ {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		maps.Copy(req.Header, header)
		answer, retry, err := send(req)
		if err == nil {
			return answer, nil
		}
		if !retry || try == tries {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "%v; retrying\n", err)
		time.Sleep(time.Duration(try) * 2 * time.Second)
	}
}

// send performs req, reporting whether a failure is worth retrying.
func send(req *http.Request) (answer []byte, retry bool, err error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, true, err
	}
	defer resp.Body.Close()
	answer, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, err
	}
	if resp.StatusCode/100 != 2 {
		code := resp.StatusCode
		return nil, code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500,
			fmt.Errorf("%s %s", resp.Status, bytes.TrimSpace(answer))
	}
	return answer, false, nil
}
