package upgrade

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeReleaseServer serves the GitHub latest-release payload shape. It
// records the requests it receives so tests can assert on headers.
func fakeReleaseServer(t *testing.T, status int, payload map[string]any) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var reqs []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Clone(r.Context()))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if payload != nil {
			_ = json.NewEncoder(w).Encode(payload)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

// releasePayload builds a minimal API payload around a tag and assets.
func releasePayload(tag string, assets ...string) map[string]any {
	var list []map[string]any
	for _, name := range assets {
		list = append(list, map[string]any{
			"name":                 name,
			"browser_download_url": "https://download.example/" + name,
		})
	}
	return map[string]any{"tag_name": tag, "assets": list}
}

func TestUpgrade_LatestRelease_Success(t *testing.T) {
	srv, reqs := fakeReleaseServer(t, http.StatusOK, releasePayload("v0.2.0",
		"plax_v0.2.0_linux_amd64.tar.gz", "checksums.txt"))
	t.Cleanup(func() { APIBase = DefaultAPIBase })
	APIBase = srv.URL

	rel, err := LatestRelease(http.DefaultClient, "apollopower/plax")
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Tag != "v0.2.0" {
		t.Fatalf("Tag = %q, want v0.2.0", rel.Tag)
	}
	if len(rel.Assets) != 2 {
		t.Fatalf("Assets = %d, want 2", len(rel.Assets))
	}
	if got := (*reqs)[0].Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Fatalf("Accept header = %q", got)
	}
}

func TestUpgrade_LatestRelease_NoReleases(t *testing.T) {
	srv, _ := fakeReleaseServer(t, http.StatusNotFound, nil)
	t.Cleanup(func() { APIBase = DefaultAPIBase })
	APIBase = srv.URL

	rel, err := LatestRelease(http.DefaultClient, "apollopower/plax")
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Tag != "" {
		t.Fatalf("Tag = %q, want empty", rel.Tag)
	}
}

func TestUpgrade_LatestRelease_ServerError(t *testing.T) {
	srv, _ := fakeReleaseServer(t, http.StatusInternalServerError, nil)
	t.Cleanup(func() { APIBase = DefaultAPIBase })
	APIBase = srv.URL

	if _, err := LatestRelease(http.DefaultClient, "apollopower/plax"); err == nil {
		t.Fatal("LatestRelease = nil error, want error")
	}
}

func TestUpgrade_LatestRelease_RateLimited(t *testing.T) {
	srv, _ := fakeReleaseServer(t, http.StatusForbidden, nil)
	t.Cleanup(func() { APIBase = DefaultAPIBase })
	APIBase = srv.URL

	_, err := LatestRelease(http.DefaultClient, "apollopower/plax")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("LatestRelease error = %v, want rate-limit (403) error", err)
	}
}

func TestUpgrade_AssetURL_Linux(t *testing.T) {
	assets := []Asset{
		{Name: "plax_v0.2.0_linux_arm64.tar.gz", URL: "a"},
		{Name: "plax_v0.2.0_linux_amd64.tar.gz", URL: "b"},
		{Name: "checksums.txt", URL: "c"},
	}
	if got := AssetURL(assets, "linux", "amd64"); got != "b" {
		t.Fatalf("AssetURL(linux, amd64) = %q, want b", got)
	}
}

func TestUpgrade_AssetURL_Darwin(t *testing.T) {
	assets := []Asset{
		{Name: "plax_v0.2.0_darwin_arm64.tar.gz", URL: "wrong"},
		{Name: "plax_v0.2.0_darwin_arm64.zip", URL: "right"},
	}
	if got := AssetURL(assets, "darwin", "arm64"); got != "right" {
		t.Fatalf("AssetURL(darwin, arm64) = %q, want right (zip wins)", got)
	}
}

func TestUpgrade_AssetURL_Missing(t *testing.T) {
	assets := []Asset{{Name: "plax_v0.2.0_linux_amd64.tar.gz", URL: "b"}}
	if got := AssetURL(assets, "windows", "amd64"); got != "" {
		t.Fatalf("AssetURL(windows, amd64) = %q, want empty", got)
	}
}

func TestUpgrade_AssetURL_UnderscoreTag(t *testing.T) {
	assets := []Asset{{Name: "plax_v0.2.0_rc_1_linux_amd64.tar.gz", URL: "b"}}
	if got := AssetURL(assets, "linux", "amd64"); got != "b" {
		t.Fatalf("AssetURL with underscore tag = %q, want b", got)
	}
}

func TestUpgrade_Download_FetchesAndExtracts(t *testing.T) {
	payload := []byte("#!/bin/sh\necho hi\n")
	archiveBytes, err := os.ReadFile(makeTarGz(t, t.TempDir(), "plax", payload))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archiveBytes)
	}))
	t.Cleanup(srv.Close)

	// The asset URL ends with the archive name so the download keeps a
	// format-detecting extension, as real release URLs do.
	dl, err := Download(srv.Client(), srv.URL+"/plax_v0.2.0_linux_amd64.tar.gz", t.TempDir())
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer func() { _ = os.Remove(dl) }()

	data, err := os.ReadFile(dl)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(archiveBytes) {
		t.Fatalf("downloaded bytes differ")
	}

	// The extracted binary is executable.
	bin, err := ExtractArchive(dl, t.TempDir())
	if err != nil {
		t.Fatalf("ExtractArchive: %v", err)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("extracted mode = %o, want 755", info.Mode().Perm())
	}
}

func TestUpgrade_Download_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	_, err := Download(srv.Client(), srv.URL, dir)
	if err == nil {
		t.Fatal("Download = nil error, want error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dir has %d leftover files after failed download", len(entries))
	}
}

func TestUpgrade_Extract_TarGz(t *testing.T) {
	payload := []byte("binary-bytes")
	dir := t.TempDir()
	archive := makeTarGz(t, dir, "plax", payload)

	bin, err := ExtractArchive(archive, t.TempDir())
	if err != nil {
		t.Fatalf("ExtractArchive: %v", err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) {
		t.Fatal("extracted tar.gz content differs")
	}
}

func TestUpgrade_Extract_Zip(t *testing.T) {
	payload := []byte("binary-bytes")
	dir := t.TempDir()
	archive := filepath.Join(dir, "plax_v0.2.0_darwin_arm64.zip")
	if err := makeZip(archive, "LICENSE", []byte("MIT"), "plax", payload); err != nil {
		t.Fatal(err)
	}

	bin, err := ExtractArchive(archive, t.TempDir())
	if err != nil {
		t.Fatalf("ExtractArchive: %v", err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) {
		t.Fatal("extracted zip content differs")
	}
}

func TestUpgrade_Extract_PicksBinaryOverLicense(t *testing.T) {
	// The real goreleaser archives carry LICENSE and README.md before the
	// plax binary; extraction must select the binary, never the first entry.
	payload := []byte("the-binary")
	dir := t.TempDir()
	archive := filepath.Join(dir, "plax_v0.2.0_darwin_arm64.zip")
	if err := makeZip(archive, "LICENSE", []byte("MIT"), "README.md", []byte("readme"), "plax", payload); err != nil {
		t.Fatal(err)
	}

	bin, err := ExtractArchive(archive, t.TempDir())
	if err != nil {
		t.Fatalf("ExtractArchive: %v", err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) {
		t.Fatalf("extracted = %q, want the plax binary", data)
	}
}

func TestUpgrade_Extract_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "plax_v0.2.0_linux_amd64.tar.gz")
	if err := makeZip(archive, "LICENSE", []byte("MIT")); err != nil {
		t.Fatal(err)
	}

	if _, err := ExtractArchive(archive, t.TempDir()); err == nil {
		t.Fatal("ExtractArchive = nil error, want missing-binary error")
	}
}

func TestUpgrade_VerifyChecksum_Match(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "plax_v0.2.0_linux_amd64.tar.gz")
	payload := []byte("archive-bytes")
	if err := os.WriteFile(archive, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256Hex(payload)
	body := sum + "  plax_v0.2.0_linux_amd64.tar.gz\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	if err := VerifyChecksum(srv.Client(), archive, srv.URL, "plax_v0.2.0_linux_amd64.tar.gz"); err != nil {
		t.Fatalf("VerifyChecksum: %v", err)
	}
}

func TestUpgrade_VerifyChecksum_TempArchiveName(t *testing.T) {
	// The archive lives in a temp file whose random name never appears in
	// the checksum file — the asset name is what must match. Regression for
	// the smoke-test failure where the temp basename was compared instead.
	dir := t.TempDir()
	archive := filepath.Join(dir, ".plax-download-123456789.tar.gz")
	payload := []byte("archive-bytes")
	if err := os.WriteFile(archive, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256Hex(payload)
	body := sum + "  plax_v0.2.0_linux_amd64.tar.gz\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	if err := VerifyChecksum(srv.Client(), archive, srv.URL, "plax_v0.2.0_linux_amd64.tar.gz"); err != nil {
		t.Fatalf("VerifyChecksum with temp-named archive: %v", err)
	}
}

func TestUpgrade_VerifyChecksum_Mismatch(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "plax_v0.2.0_linux_amd64.tar.gz")
	if err := os.WriteFile(archive, []byte("archive-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("0", 64) + "  plax_v0.2.0_linux_amd64.tar.gz\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	if err := VerifyChecksum(srv.Client(), archive, srv.URL, "plax_v0.2.0_linux_amd64.tar.gz"); err == nil {
		t.Fatal("VerifyChecksum = nil error, want mismatch error")
	}
}

// sha256Hex returns the hex SHA-256 of payload.
func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// makeTarGz writes a tar.gz archive into dir containing LICENSE, README.md,
// and the plax binary (the real goreleaser layout) and returns its path.
func makeTarGz(t *testing.T, dir, name string, payload []byte) string {
	t.Helper()
	path := filepath.Join(dir, "plax_v0.2.0_linux_amd64.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"LICENSE", 0o644, []byte("MIT")},
		{"README.md", 0o644, []byte("readme")},
		{name, 0o755, payload},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// makeZip writes a zip archive with the given name/content pairs.
func makeZip(path string, entries ...any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for i := 0; i+1 < len(entries); i += 2 {
		name, ok := entries[i].(string)
		if !ok {
			return fmt.Errorf("entry %d: name is not a string", i)
		}
		body, ok := entries[i+1].([]byte)
		if !ok {
			return fmt.Errorf("entry %d: body is not []byte", i)
		}
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Close()
}
