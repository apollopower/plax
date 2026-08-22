package upgrade

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultAPIBase is the production GitHub REST API base URL; tests restore
// APIBase to it after pointing the variable at an httptest server.
const DefaultAPIBase = "https://api.github.com"

// APIBase is the GitHub REST API base URL. Tests point it at an httptest
// server so both the package and the CLI can exercise the lookup flow
// without network access.
var APIBase = DefaultAPIBase

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name string // e.g. plax_v0.2.0_linux_amd64.tar.gz
	URL  string // browser_download_url — the download host, not the API
}

// Release is the latest-release payload shape plax reads. Everything else
// GitHub returns is ignored.
type Release struct {
	Tag    string // e.g. v0.2.0
	Assets []Asset
}

// githubRelease mirrors the API response fields we decode.
type githubRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// releaseAsset mirrors one API asset entry.
type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// LatestRelease fetches the newest release of repo. A 404 (no releases yet)
// returns an empty Release with no error — there is nothing to be outdated
// against.
func LatestRelease(client *http.Client, repo string) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", APIBase, repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "plax-upgrade")

	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("checking %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var rel githubRelease
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
			return Release{}, fmt.Errorf("parsing release from %s: %w", url, err)
		}
		out := Release{Tag: rel.TagName}
		for _, a := range rel.Assets {
			out.Assets = append(out.Assets, Asset{Name: a.Name, URL: a.BrowserDownloadURL})
		}
		return out, nil
	case http.StatusNotFound:
		return Release{}, nil
	case http.StatusForbidden:
		// The unauthenticated endpoint is shared per IP; a 403 is a rate
		// limit, not a missing release.
		return Release{}, fmt.Errorf("checking %s: rate limited (HTTP 403) — retry later", url)
	default:
		return Release{}, fmt.Errorf("checking %s: HTTP %s", url, resp.Status)
	}
}

// AssetFor returns the archive asset matching goos/goarch, or false when
// the release carries none. Asset names follow .goreleaser.yml:
// plax_<tag>_<goos>_<goarch>.tar.gz (zip on darwin), and the format is
// checked too — a darwin release never uses tar.gz.
func AssetFor(assets []Asset, goos, goarch string) (Asset, bool) {
	wantSuffix := ".tar.gz"
	if goos == "darwin" {
		wantSuffix = ".zip"
	}
	for _, a := range assets {
		if matchesAsset(a.Name, goos, goarch, wantSuffix) {
			return a, true
		}
	}
	return Asset{}, false
}

// AssetURL returns the download URL of the archive matching goos/goarch,
// or "" when the release carries no such asset.
func AssetURL(assets []Asset, goos, goarch string) string {
	a, ok := AssetFor(assets, goos, goarch)
	if !ok {
		return ""
	}
	return a.URL
}

// matchesAsset parses "plax_<tag>_<goos>_<goarch><suffix>" and compares the
// trailing goos/goarch, so a tag containing underscores cannot confuse the
// match. checksums.txt and other assets never match.
func matchesAsset(name, goos, goarch, suffix string) bool {
	rest, ok := strings.CutSuffix(name, suffix)
	if !ok {
		return false
	}
	rest, ok = strings.CutPrefix(rest, "plax_")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "_")
	return len(parts) >= 2 && parts[len(parts)-2] == goos && parts[len(parts)-1] == goarch
}

// ChecksumsURL returns the download URL of the release's checksums.txt
// asset, or "" when the release carries none.
func ChecksumsURL(assets []Asset) string {
	for _, a := range assets {
		if a.Name == "checksums.txt" {
			return a.URL
		}
	}
	return ""
}

// Download fetches url into a temp file inside dir and returns its path.
// The temp name keeps the URL's archive extension so ExtractArchive can
// pick the format; the caller renames the result into place or removes it
// on failure.
func Download(client *http.Client, rawURL, dir string) (string, error) {
	pattern := ".plax-download-*"
	if u, err := url.Parse(rawURL); err == nil {
		switch {
		case strings.HasSuffix(u.Path, ".tar.gz"):
			pattern = ".plax-download-*.tar.gz"
		case strings.HasSuffix(u.Path, ".zip"):
			pattern = ".plax-download-*.zip"
		}
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	resp, err := client.Get(rawURL)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("downloading %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		cleanup()
		return "", fmt.Errorf("downloading %s: HTTP %s", rawURL, resp.Status)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		cleanup()
		return "", fmt.Errorf("downloading %s: %w", rawURL, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", err
	}
	return tmp.Name(), nil
}

// ExtractArchive unpacks the release archive at path into dir and returns
// the extracted binary path. The goreleaser archives carry LICENSE and
// README.md next to the plax binary, so extraction selects the entry named
// "plax" and errors when it is absent. Format is chosen by extension.
func ExtractArchive(path, dir string) (string, error) {
	switch {
	case strings.HasSuffix(path, ".tar.gz"), strings.HasSuffix(path, ".tgz"):
		return extractTarGz(path, dir)
	case strings.HasSuffix(path, ".zip"):
		return extractZip(path, dir)
	default:
		return "", fmt.Errorf("unsupported archive format: %s", path)
	}
}

// extractTarGz extracts the "plax" entry from a tar.gz archive into dir.
func extractTarGz(path, dir string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("archive %s contains no plax binary", path)
		}
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
		if hdr.Typeflag != tar.TypeDir && filepath.Base(hdr.Name) == "plax" {
			return writeBinary(dir, hdr.Name, tr)
		}
	}
}

// extractZip extracts the "plax" entry from a zip archive into dir.
func extractZip(path, dir string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = zr.Close() }()

	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Base(f.Name) != "plax" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer func() { _ = rc.Close() }()
		return writeBinary(dir, f.Name, rc)
	}
	return "", fmt.Errorf("archive %s contains no plax binary", path)
}

// writeBinary copies one stream into a fresh 0755 file in dir.
func writeBinary(dir, name string, r io.Reader) (string, error) {
	path := filepath.Join(dir, filepath.Base(name))
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, r); err != nil {
		_ = dst.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// VerifyChecksum checks the downloaded archive against the release's
// checksums.txt asset: it downloads the checksum file, computes the
// archive's SHA-256, and requires an exact "hash  <assetName>" match. The
// archive is never trusted — and never renamed into place — before this
// passes. assetName is the real release asset name; the archive sits in a
// temp file whose random name never appears in the checksum file.
func VerifyChecksum(client *http.Client, archivePath, sumURL, assetName string) error {
	sums, err := Download(client, sumURL, filepath.Dir(archivePath))
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(sums) }()

	body, err := os.ReadFile(sums)
	if err != nil {
		return err
	}

	sum, err := sha256File(archivePath)
	if err != nil {
		return err
	}

	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName && strings.EqualFold(fields[0], sum) {
			return nil
		}
	}
	return fmt.Errorf("checksum mismatch for %s: not found in %s", assetName, filepath.Base(sums))
}

// sha256File returns the hex SHA-256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
