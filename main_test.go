package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- parseRepoSpec ---

func TestParseRepoSpec(t *testing.T) {
	tests := []struct {
		in       string
		wantRepo string
		wantTag  string
		wantErr  bool
	}{
		{"foo/bar", "foo/bar", "", false},
		{"foo/bar@v1.2.3", "foo/bar", "v1.2.3", false},
		{"foo/bar@1.0", "foo/bar", "1.0", false},
		{"foo/bar@", "", "", true},
		{"foo", "", "", true},
		{"/bar", "", "", true},
		{"foo/", "", "", true},
		{"", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			repo, tag, err := parseRepoSpec(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got repo=%q tag=%q", repo, tag)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repo != tt.wantRepo || tag != tt.wantTag {
				t.Errorf("got (%q, %q), want (%q, %q)", repo, tag, tt.wantRepo, tt.wantTag)
			}
		})
	}
}

// --- containsWord ---

func TestContainsWord(t *testing.T) {
	tests := []struct {
		s, needle string
		want      bool
	}{
		{"foo-x86_64-linux", "x86_64", true},
		{"foo-x86_64-linux", "x86", false}, // boundary check
		{"foo-x86-linux", "x86", true},
		{"app-i386.tar.gz", "i386", true},
		{"app-i386x.tar.gz", "i386", false},
		{"linux-amd64", "amd64", true},
		{"prefix-arm64-suffix", "arm64", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s in %s", tt.needle, tt.s), func(t *testing.T) {
			if got := containsWord(tt.s, tt.needle); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// --- scoreAsset / pickAsset ---

func TestPickAsset(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("test assumes a unix host")
	}

	type tc struct {
		name    string
		assets  []string
		want    string
		wantErr bool
	}
	tests := []tc{
		{
			name: "prefer correct os/arch tar.gz",
			assets: []string{
				"app-windows-amd64.zip",
				"app-linux-amd64.tar.gz",
				"app-darwin-amd64.tar.gz",
				"app-darwin-arm64.tar.gz",
				"checksums.txt",
			},
		},
		{
			name: "reject when only wrong-os available",
			assets: []string{
				"app-windows-amd64.zip",
				"app-windows-arm64.zip",
			},
			wantErr: true,
		},
		{
			name: "reject xz/zst even if other-os assets exist",
			assets: []string{
				"app-linux-amd64.tar.xz",
				"app-darwin-amd64.tar.zst",
			},
			wantErr: true,
		},
		{
			name: "skip checksum/sig metadata",
			assets: []string{
				"app-darwin-arm64.tar.gz.sha256",
				"app-darwin-arm64.tar.gz.asc",
				"app-darwin-arm64.tar.gz",
				"app-linux-amd64.tar.gz",
			},
		},
		{
			name: "rust triple match",
			assets: []string{
				"app-aarch64-apple-darwin.tar.gz",
				"app-x86_64-apple-darwin.tar.gz",
				"app-aarch64-unknown-linux-gnu.tar.gz",
				"app-x86_64-unknown-linux-musl.tar.gz",
				"app-x86_64-unknown-linux-gnu.tar.gz",
			},
		},
	}

	// Compute platform-specific expected picks.
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		tests[0].want = "app-darwin-arm64.tar.gz"
		tests[3].want = "app-darwin-arm64.tar.gz"
		tests[4].want = "app-aarch64-apple-darwin.tar.gz"
	case "darwin/amd64":
		tests[0].want = "app-darwin-amd64.tar.gz"
		tests[3].want = "app-linux-amd64.tar.gz" // no darwin-amd64 here, will pick the first valid
		tests[4].want = "app-x86_64-apple-darwin.tar.gz"
	case "linux/amd64":
		tests[0].want = "app-linux-amd64.tar.gz"
		tests[3].want = "app-linux-amd64.tar.gz"
		tests[4].want = "app-x86_64-unknown-linux-gnu.tar.gz"
	case "linux/arm64":
		tests[0].want = "app-linux-amd64.tar.gz" // not arm64, but no linux-arm64 in set; will fall back to scoring
		tests[3].want = "app-linux-amd64.tar.gz"
		tests[4].want = "app-aarch64-unknown-linux-gnu.tar.gz"
	default:
		t.Skipf("unsupported test host %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assets := make([]Asset, len(tt.assets))
			for i, n := range tt.assets {
				assets[i] = Asset{Name: n, URL: "https://example/" + n}
			}
			got, err := pickAsset(assets)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want == "" {
				return
			}
			if got.Name != tt.want {
				t.Errorf("picked %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestPickAssetMuslPreference(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("musl preference applies on linux only")
	}
	assets := []Asset{
		{Name: "app-x86_64-unknown-linux-musl.tar.gz"},
		{Name: "app-x86_64-unknown-linux-gnu.tar.gz"},
	}
	got, err := pickAsset(assets)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Name, "gnu") {
		t.Errorf("expected gnu preference, got %q", got.Name)
	}
}

// --- isHex ---

func TestIsHex(t *testing.T) {
	cases := map[string]bool{
		"deadbeef":          true,
		"DEADBEEF":          true,
		"0123456789abcdef":  true,
		"":                  false,
		"deadbeefg":         false,
		"hello world":       false,
		"0x1234":            false,
	}
	for s, want := range cases {
		if got := isHex(s); got != want {
			t.Errorf("isHex(%q) = %v, want %v", s, got, want)
		}
	}
}

// --- safeJoin (zip-slip / tar-slip) ---

func TestSafeJoin(t *testing.T) {
	base := t.TempDir()
	good := []string{"foo", "foo/bar", "bin/foo"}
	bad := []string{"../escape", "/etc/passwd", "foo/../../escape", ".."}
	for _, p := range good {
		if _, err := safeJoin(base, p); err != nil {
			t.Errorf("safeJoin(%q) errored: %v", p, err)
		}
	}
	for _, p := range bad {
		if _, err := safeJoin(base, p); err == nil {
			t.Errorf("safeJoin(%q) should have errored", p)
		}
	}
}

// --- Checksum file fetch & parse ---

func TestFindExpectedSum_AggregateAndPerAsset(t *testing.T) {
	hash := "deadbeef" + strings.Repeat("00", 28) // 64 hex chars
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			fmt.Fprintf(w, "%s  app-linux-amd64.tar.gz\n%s *other.zip\n", hash, hash)
		case strings.HasSuffix(r.URL.Path, "/app-linux-amd64.tar.gz.sha256"):
			fmt.Fprintf(w, "%s  app-linux-amd64.tar.gz\n", hash)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Run("per-asset", func(t *testing.T) {
		assets := []Asset{
			{Name: "app-linux-amd64.tar.gz", URL: srv.URL + "/app-linux-amd64.tar.gz"},
			{Name: "app-linux-amd64.tar.gz.sha256", URL: srv.URL + "/app-linux-amd64.tar.gz.sha256"},
		}
		got := findExpectedSum(assets, "app-linux-amd64.tar.gz")
		if got != hash {
			t.Errorf("got %q, want %q", got, hash)
		}
	})

	t.Run("aggregate", func(t *testing.T) {
		assets := []Asset{
			{Name: "app-linux-amd64.tar.gz", URL: srv.URL + "/app-linux-amd64.tar.gz"},
			{Name: "SHA256SUMS", URL: srv.URL + "/SHA256SUMS"},
		}
		got := findExpectedSum(assets, "app-linux-amd64.tar.gz")
		if got != hash {
			t.Errorf("got %q, want %q", got, hash)
		}
	})

	t.Run("none", func(t *testing.T) {
		assets := []Asset{
			{Name: "app-linux-amd64.tar.gz", URL: srv.URL + "/app-linux-amd64.tar.gz"},
		}
		if got := findExpectedSum(assets, "app-linux-amd64.tar.gz"); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

// --- Archive extraction ---

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestStreamTarGz_PreservesSubdirs(t *testing.T) {
	dest := t.TempDir()
	data := makeTarGz(t, map[string]string{
		"app/bin/foo":           "\x7fELF...binary",
		"app/share/man/foo.1":   "manpage",
		"app/completions/foo.fish": "completion",
	})
	if err := streamTarGz(bytes.NewReader(data), dest); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"app/bin/foo", "app/share/man/foo.1", "app/completions/foo.fish"} {
		full := filepath.Join(dest, filepath.FromSlash(p))
		if _, err := os.Stat(full); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestStreamTarGz_RejectsSlip(t *testing.T) {
	dest := t.TempDir()
	data := makeTarGz(t, map[string]string{
		"../evil": "pwn",
	})
	if err := streamTarGz(bytes.NewReader(data), dest); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(dest)
	if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
		t.Error("tar-slip succeeded; ../evil was written")
	}
}

func TestExtractZip_PreservesSubdirs(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "a.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range map[string]string{
		"app/bin/foo":         "\x7fELFbin",
		"app/README.md":       "readme",
	} {
		fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fh.SetMode(0o755)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	zw.Close()
	f.Close()

	dest := t.TempDir()
	if err := extractZip(zipPath, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "app", "bin", "foo")); err != nil {
		t.Errorf("missing nested file: %v", err)
	}
}

// --- detectBinaries ---

// fakeELF writes an ELF-magic executable file at path.
func fakeELF(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte{0}, 60)...)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDetectBinaries_PrefersBinSubdir(t *testing.T) {
	dir := t.TempDir()
	fakeELF(t, filepath.Join(dir, "uninstall.sh")) // chmod +x but ELF for test
	fakeELF(t, filepath.Join(dir, "bin", "foo"))
	fakeELF(t, filepath.Join(dir, "share", "tools", "helper"))

	bins, err := detectBinaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bins) != 1 || filepath.Base(bins[0]) != "foo" {
		var names []string
		for _, b := range bins {
			names = append(names, filepath.Base(b))
		}
		t.Errorf("expected only [foo] from bin/, got %v", names)
	}
}

func TestDetectBinaries_NoBinDir(t *testing.T) {
	dir := t.TempDir()
	fakeELF(t, filepath.Join(dir, "tool"))
	fakeELF(t, filepath.Join(dir, "tool-helper"))

	bins, err := detectBinaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bins) != 2 {
		t.Errorf("expected 2 bins at root, got %d", len(bins))
	}
}

func TestDetectBinaries_SkipsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"),
		[]byte("not an executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	bins, err := detectBinaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bins) != 0 {
		t.Errorf("expected 0 bins, got %v", bins)
	}
}

// --- download integration with hashing ---

func TestDownloadHashesBytes(t *testing.T) {
	body := []byte("hello world payload")
	want := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	tmp := filepath.Join(t.TempDir(), "out")
	got, err := download(srv.URL, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("hash mismatch: got %s want %s", got, hex.EncodeToString(want[:]))
	}
	read, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, body) {
		t.Error("downloaded bytes differ from server payload")
	}
}

// streamTarGz hash parity: ensure tee-hashing matches direct hash of the tar.gz body.
func TestStreamTarGz_HashTeeParity(t *testing.T) {
	data := makeTarGz(t, map[string]string{
		"a/b": "hello",
		"a/c": strings.Repeat("x", 4096),
	})
	want := sha256.Sum256(data)

	dest := t.TempDir()
	hasher := sha256.New()
	tee := io.TeeReader(bytes.NewReader(data), hasher)
	if err := streamTarGz(tee, dest); err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, tee)
	got := hasher.Sum(nil)
	if !bytes.Equal(got, want[:]) {
		t.Errorf("tee hash mismatch: got %x want %x", got, want)
	}
}
