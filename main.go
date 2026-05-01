package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const version = "0.2.0"

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 10,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	},
}

// --- Types ---

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// --- CLI ---

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "--help", "-h":
		printUsage()
		os.Exit(0)
	case "--version", "-v":
		fmt.Println("sip " + version)
		os.Exit(0)
	case "i", "install":
		err = runInstall(os.Args[2:])
	case "r", "remove":
		err = runRemove(os.Args[2:])
	case "u", "upgrade":
		err = runUpgrade(os.Args[2:])
	case "l", "list":
		err = runList()
	case "s", "search":
		err = runSearch(os.Args[2:])
	case "n", "info":
		err = runInfo(os.Args[2:])
	default:
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, color("31", "error: ")+err.Error())
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`sip — a tiny package manager

usage:
  sip i <owner/repo>[@version]   install a package (latest or pinned)
  sip r <name>                   remove a package
  sip u [name]                   upgrade all or one package
  sip l                          list installed packages
  sip s <query>                  search installed packages
  sip n <name>                   show package info
`)
}

// --- Commands ---

func parseRepoSpec(spec string) (repo, tag string, err error) {
	if r, t, ok := strings.Cut(spec, "@"); ok {
		if t == "" {
			return "", "", fmt.Errorf("empty version in %q", spec)
		}
		repo, tag = r, t
	} else {
		repo = spec
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("invalid repo format, expected owner/repo[@version]")
	}
	return repo, tag, nil
}

func runInstall(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sip i <owner/repo>[@version]")
	}
	repo, tag, err := parseRepoSpec(args[0])
	if err != nil {
		return err
	}
	_, name, _ := strings.Cut(repo, "/")

	if _, err := os.Stat(pkgDir(name)); err == nil {
		return fmt.Errorf("%s is already installed", name)
	}

	if tag == "" {
		fmt.Printf("%s %s\n", color("36", "fetching"), repo)
	} else {
		fmt.Printf("%s %s@%s\n", color("36", "fetching"), repo, tag)
	}
	rel, err := fetchRelease(repo, tag)
	if err != nil {
		return err
	}

	return installRelease(repo, name, rel)
}

// downloadRelease downloads, verifies, extracts, and writes metadata to pkgDir(name).
// It does not link binaries or print the final "installed" message.
func downloadRelease(repo, name string, rel *Release) error {
	asset, err := pickAsset(rel.Assets)
	if err != nil {
		return err
	}

	expected := findExpectedSum(rel.Assets, asset.Name)

	fmt.Printf("%s %s (%s)\n", color("36", "downloading"), asset.Name, rel.TagName)

	dest := pkgDir(name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	lower := strings.ToLower(asset.Name)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		// Stream tar.gz directly from HTTP — no temp file, but hash on the fly.
		resp, err := httpGet(asset.URL)
		if err != nil {
			os.RemoveAll(dest)
			return err
		}
		defer resp.Body.Close()

		fmt.Printf("%s %s\n", color("36", "extracting"), asset.Name)
		hasher := sha256.New()
		tee := io.TeeReader(resp.Body, hasher)
		if err := streamTarGz(tee, dest); err != nil {
			os.RemoveAll(dest)
			return err
		}
		// Drain anything past the gzip footer so the hash covers the full body.
		io.Copy(io.Discard, tee)
		if expected != "" {
			got := hex.EncodeToString(hasher.Sum(nil))
			if !strings.EqualFold(got, expected) {
				os.RemoveAll(dest)
				return fmt.Errorf("checksum mismatch for %s\n  expected: %s\n  got:      %s", asset.Name, expected, got)
			}
			fmt.Printf("%s %s\n", color("32", "verified"), shortHash(got))
		}
	} else {
		tmp, err := os.CreateTemp("", "sip-*-"+asset.Name)
		if err != nil {
			os.RemoveAll(dest)
			return err
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath)

		sum, err := download(asset.URL, tmpPath)
		if err != nil {
			os.RemoveAll(dest)
			return err
		}
		if expected != "" {
			if !strings.EqualFold(sum, expected) {
				os.RemoveAll(dest)
				return fmt.Errorf("checksum mismatch for %s\n  expected: %s\n  got:      %s", asset.Name, expected, sum)
			}
			fmt.Printf("%s %s\n", color("32", "verified"), shortHash(sum))
		}

		fmt.Printf("%s %s\n", color("36", "extracting"), asset.Name)
		switch {
		case strings.HasSuffix(lower, ".zip"):
			if err := extractZip(tmpPath, dest); err != nil {
				os.RemoveAll(dest)
				return err
			}
		default:
			// Raw binary
			bin := filepath.Join(dest, name)
			if err := copyFile(tmpPath, bin); err != nil {
				os.RemoveAll(dest)
				return err
			}
			os.Chmod(bin, 0o755)
		}
	}

	if err := os.WriteFile(filepath.Join(dest, ".repo"), []byte(repo), 0o644); err != nil {
		os.RemoveAll(dest)
		return err
	}
	if err := os.WriteFile(filepath.Join(dest, ".version"), []byte(rel.TagName), 0o644); err != nil {
		os.RemoveAll(dest)
		return err
	}

	return nil
}

func installRelease(repo, name string, rel *Release) error {
	if err := downloadRelease(repo, name, rel); err != nil {
		return err
	}

	dest := pkgDir(name)
	bins, err := detectBinaries(dest)
	if err != nil || len(bins) == 0 {
		os.RemoveAll(dest)
		return fmt.Errorf("no binaries found in release")
	}

	if err := linkBins(bins); err != nil {
		os.RemoveAll(dest)
		return err
	}

	binNames := make([]string, len(bins))
	for i, b := range bins {
		binNames[i] = filepath.Base(b)
	}
	fmt.Printf("%s %s → %s\n", color("32", "installed"), name, strings.Join(binNames, ", "))
	return nil
}

func runRemove(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sip r <name>")
	}
	name := args[0]
	dir := pkgDir(name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("%s is not installed", name)
	}

	unlinkBins(name)
	os.RemoveAll(dir)
	fmt.Printf("%s %s\n", color("32", "removed"), name)
	return nil
}

func runUpgrade(args []string) error {
	// Single-package upgrade
	if len(args) > 0 {
		name := args[0]
		repo, current, err := readPkgMetadata(name)
		if err != nil {
			return err
		}

		rel, err := fetchRelease(repo, "")
		if err != nil {
			return err
		}
		if rel.TagName == current {
			fmt.Printf("%s %s (%s)\n", color("90", "up-to-date"), name, current)
			return nil
		}

		fmt.Printf("%s %s %s → %s\n", color("36", "upgrading"), name, current, rel.TagName)
		if err := atomicUpgrade(name, repo, rel); err != nil {
			return err
		}
		fmt.Printf("%s %s (%s)\n", color("32", "upgraded"), name, rel.TagName)
		return nil
	}

	entries, err := os.ReadDir(filepath.Join(sipDir(), "pkg"))
	if err != nil {
		return fmt.Errorf("nothing installed")
	}

	type pkgInfo struct {
		name, repo, current string
	}
	type fetchResult struct {
		rel *Release
		err error
	}

	var pkgs []pkgInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		repo, current, err := readPkgMetadata(name)
		if err != nil {
			fmt.Printf("%s %s: %v\n", color("33", "skip"), name, err)
			continue
		}
		pkgs = append(pkgs, pkgInfo{name, repo, current})
	}

	results := make([]fetchResult, len(pkgs))
	var wg sync.WaitGroup
	for i, p := range pkgs {
		wg.Add(1)
		go func(i int, repo string) {
			defer wg.Done()
			rel, err := fetchRelease(repo, "")
			results[i] = fetchResult{rel, err}
		}(i, p.repo)
	}
	wg.Wait()

	type upgradeJob struct {
		idx int
		pkg pkgInfo
		rel *Release
	}

	var jobs []upgradeJob
	for i, p := range pkgs {
		if results[i].err != nil {
			fmt.Printf("%s %s: %v\n", color("33", "skip"), p.name, results[i].err)
			continue
		}
		rel := results[i].rel
		if rel.TagName == p.current {
			fmt.Printf("%s %s (%s)\n", color("90", "up-to-date"), p.name, p.current)
			continue
		}
		fmt.Printf("%s %s %s → %s\n", color("36", "upgrading"), p.name, p.current, rel.TagName)
		jobs = append(jobs, upgradeJob{i, p, rel})
	}

	upgradeResults := make([]error, len(jobs))
	sem := make(chan struct{}, 4)
	var wg2 sync.WaitGroup
	for j, job := range jobs {
		wg2.Add(1)
		go func(j int, job upgradeJob) {
			defer wg2.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			upgradeResults[j] = atomicUpgrade(job.pkg.name, job.pkg.repo, job.rel)
		}(j, job)
	}
	wg2.Wait()

	var upgraded int
	for j, job := range jobs {
		if upgradeResults[j] != nil {
			fmt.Printf("%s %s: %v\n", color("33", "skip"), job.pkg.name, upgradeResults[j])
			continue
		}
		upgraded++
		fmt.Printf("%s %s (%s)\n", color("32", "upgraded"), job.pkg.name, job.rel.TagName)
	}

	fmt.Printf("upgraded %d/%d packages\n", upgraded, len(pkgs))
	return nil
}

func atomicUpgrade(name, repo string, rel *Release) error {
	stagingName := name + ".new"
	stagingDir := pkgDir(stagingName)

	os.RemoveAll(stagingDir)

	if err := downloadRelease(repo, stagingName, rel); err != nil {
		os.RemoveAll(stagingDir)
		return err
	}

	oldDir := pkgDir(name)
	unlinkBins(name)
	os.RemoveAll(oldDir)

	if err := os.Rename(stagingDir, oldDir); err != nil {
		return err
	}

	bins, _ := detectBinaries(oldDir)
	if len(bins) > 0 {
		if err := linkBins(bins); err != nil {
			return err
		}
	}
	return nil
}

func runList() error {
	entries, err := os.ReadDir(filepath.Join(sipDir(), "pkg"))
	if err != nil || len(entries) == 0 {
		fmt.Println("no packages installed")
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		version, _ := os.ReadFile(filepath.Join(pkgDir(name), ".version"))
		v := strings.TrimSpace(string(version))
		if v == "" {
			v = "unknown"
		}
		fmt.Printf("  %s %s\n", color("37", name), color("90", v))
	}
	return nil
}

func runSearch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sip s <query>")
	}
	query := strings.ToLower(args[0])
	entries, err := os.ReadDir(filepath.Join(sipDir(), "pkg"))
	if err != nil {
		fmt.Println("no packages installed")
		return nil
	}
	var found int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.Contains(strings.ToLower(e.Name()), query) {
			version, _ := os.ReadFile(filepath.Join(pkgDir(e.Name()), ".version"))
			v := strings.TrimSpace(string(version))
			if v == "" {
				v = "unknown"
			}
			fmt.Printf("  %s %s\n", color("37", e.Name()), color("90", v))
			found++
		}
	}
	if found == 0 {
		fmt.Println("no matches")
	}
	return nil
}

func runInfo(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sip n <name>")
	}
	name := args[0]
	dir := pkgDir(name)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("%s is not installed", name)
	}

	repo, _ := os.ReadFile(filepath.Join(dir, ".repo"))
	version, _ := os.ReadFile(filepath.Join(dir, ".version"))
	bins, _ := detectBinaries(dir)

	fmt.Printf("  %s  %s\n", color("90", "name"), name)
	fmt.Printf("  %s  %s\n", color("90", "repo"), strings.TrimSpace(string(repo)))
	fmt.Printf("  %s  %s\n", color("90", "ver "), strings.TrimSpace(string(version)))

	binNames := make([]string, len(bins))
	for i, b := range bins {
		binNames[i] = filepath.Base(b)
	}
	if len(binNames) > 0 {
		fmt.Printf("  %s  %s\n", color("90", "bins"), strings.Join(binNames, ", "))
	}
	return nil
}

// --- GitHub ---

func fetchRelease(repo, tag string) (*Release, error) {
	var url string
	if tag == "" {
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	} else {
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 && tag != "" {
		return nil, fmt.Errorf("release %s not found in %s", tag, repo)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github api: %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// scoreAsset returns a relevance score and whether the asset is supported.
// Assets with negative scores or unsupported formats are rejected.
func scoreAsset(name string) (score int, supported bool) {
	n := strings.ToLower(name)
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	// Skip known non-binary metadata files.
	for _, suf := range []string{".sha256", ".sha256sum", ".sha512", ".sig", ".asc", ".sbom", ".pem", ".cert", ".crt", ".txt", ".json", ".yaml", ".yml", ".md"} {
		if strings.HasSuffix(n, suf) {
			return 0, false
		}
	}
	// SHA256SUMS-style aggregate files.
	base := strings.ToLower(filepath.Base(n))
	if base == "sha256sums" || base == "checksums" || strings.HasPrefix(base, "checksums.") || strings.HasPrefix(base, "sha256sums.") {
		return 0, false
	}

	// Format support & preference. xz/zst rejected — no stdlib decoder.
	format := 0
	switch {
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		format = 5
	case strings.HasSuffix(n, ".zip"):
		format = 3
	case strings.HasSuffix(n, ".tar.xz"), strings.HasSuffix(n, ".txz"),
		strings.HasSuffix(n, ".tar.zst"), strings.HasSuffix(n, ".tzst"),
		strings.HasSuffix(n, ".tar.bz2"), strings.HasSuffix(n, ".tbz2"),
		strings.HasSuffix(n, ".7z"), strings.HasSuffix(n, ".rar"),
		strings.HasSuffix(n, ".deb"), strings.HasSuffix(n, ".rpm"),
		strings.HasSuffix(n, ".dmg"), strings.HasSuffix(n, ".pkg"),
		strings.HasSuffix(n, ".apk"), strings.HasSuffix(n, ".AppImage"),
		strings.HasSuffix(n, ".snap"), strings.HasSuffix(n, ".msi"):
		return 0, false
	default:
		// Treat as raw binary — only if no extension or just .exe.
		if strings.Contains(n, ".") && !strings.HasSuffix(n, ".exe") {
			// Has a dot but unrecognized extension — risky.
			return 0, false
		}
		format = 1
	}

	s := format

	// OS scoring. Reject hard mismatches.
	osHits := map[string][]string{
		"darwin":  {"darwin", "macos", "apple", "osx"},
		"linux":   {"linux"},
		"windows": {"windows", "win64", "win32", ".exe"},
		"freebsd": {"freebsd"},
		"openbsd": {"openbsd"},
		"netbsd":  {"netbsd"},
	}
	otherOS := func(self string) []string {
		var out []string
		for o, kws := range osHits {
			if o == self {
				continue
			}
			out = append(out, kws...)
		}
		return out
	}
	if hits, ok := osHits[goos]; ok {
		for _, kw := range hits {
			if strings.Contains(n, kw) {
				s += 10
				break
			}
		}
	}
	for _, kw := range otherOS(goos) {
		if strings.Contains(n, kw) {
			return -1, false
		}
	}

	// Arch scoring. Reject hard mismatches.
	archHits := map[string][]string{
		"amd64": {"amd64", "x86_64", "x64"},
		"arm64": {"arm64", "aarch64"},
		"386":   {"i386", "i686", "x86"},
		"arm":   {"armv7", "armhf", "armv6"},
	}
	if hits, ok := archHits[goarch]; ok {
		for _, kw := range hits {
			if strings.Contains(n, kw) {
				s += 10
				break
			}
		}
	}
	for arch, kws := range archHits {
		if arch == goarch {
			continue
		}
		for _, kw := range kws {
			// Avoid false positives — "x86" is a substring of "x86_64", check word-ish boundaries.
			if containsWord(n, kw) {
				// "386" / "x86" misclassifies amd64 variants, so guard it.
				if (kw == "x86" || kw == "i386" || kw == "i686") && goarch == "amd64" {
					continue
				}
				return -1, false
			}
		}
	}

	// libc preference on Linux: prefer gnu/glibc over musl by default.
	if goos == "linux" {
		if strings.Contains(n, "musl") {
			s -= 1
		}
		if strings.Contains(n, "gnu") || strings.Contains(n, "glibc") {
			s += 1
		}
	}

	return s, true
}

// containsWord returns true if needle appears in s bounded by non-alphanumeric chars
// (or string edges). Avoids matching "x86" inside "x86_64".
func containsWord(s, needle string) bool {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] != needle {
			continue
		}
		left := i == 0 || !isAlnum(s[i-1])
		right := i+len(needle) == len(s) || !isAlnum(s[i+len(needle)])
		if left && right {
			return true
		}
	}
	return false
}

func isAlnum(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func pickAsset(assets []Asset) (*Asset, error) {
	if len(assets) == 0 {
		return nil, fmt.Errorf("no release assets found")
	}

	bestScore := -1 << 30
	var best *Asset
	for i := range assets {
		s, ok := scoreAsset(assets[i].Name)
		if !ok {
			continue
		}
		if s > bestScore {
			bestScore = s
			best = &assets[i]
		}
	}
	if best == nil || bestScore < 10 {
		return nil, fmt.Errorf("no compatible asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return best, nil
}

// findExpectedSum looks for a SHA-256 checksum for assetName among the release assets.
// Returns the lowercase hex hash, or "" if no checksum file is found.
func findExpectedSum(assets []Asset, assetName string) string {
	lowerName := strings.ToLower(assetName)

	// Pattern 1: a per-asset file like <asset>.sha256 / <asset>.sha256sum.
	for _, a := range assets {
		ln := strings.ToLower(a.Name)
		if ln == lowerName+".sha256" || ln == lowerName+".sha256sum" {
			if h := fetchSingleHash(a.URL); h != "" {
				return h
			}
		}
	}

	// Pattern 2: an aggregate file like SHA256SUMS / checksums.txt / *_checksums.txt.
	for _, a := range assets {
		ln := strings.ToLower(filepath.Base(a.Name))
		if ln == "sha256sums" || ln == "sha256sums.txt" || ln == "checksums.txt" ||
			strings.HasSuffix(ln, "_checksums.txt") || strings.HasSuffix(ln, "-checksums.txt") ||
			strings.HasSuffix(ln, ".sha256sums") {
			if h := fetchSumFromList(a.URL, assetName); h != "" {
				return h
			}
		}
	}

	return ""
}

// fetchSingleHash fetches a "<hash>  <name>" or "<hash>" file and returns the hash.
func fetchSingleHash(url string) string {
	resp, err := httpGet(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(body))
	// Take first whitespace-separated token.
	if i := strings.IndexAny(line, " \t\n"); i > 0 {
		line = line[:i]
	}
	if isHex(line) && len(line) == 64 {
		return strings.ToLower(line)
	}
	return ""
}

// fetchSumFromList fetches a SHA256SUMS-style file and returns the hash for assetName.
func fetchSumFromList(url, assetName string) string {
	resp, err := httpGet(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	target := strings.ToLower(assetName)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: "<hash>  <filename>" or "<hash> *<filename>"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash := fields[0]
		fname := strings.TrimPrefix(fields[len(fields)-1], "*")
		if strings.ToLower(filepath.Base(fname)) == target && isHex(hash) && len(hash) == 64 {
			return strings.ToLower(hash)
		}
	}
	return ""
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func httpGet(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	return resp, nil
}

// download writes url to dest and returns the SHA-256 of the bytes written.
func download(url, dest string) (string, error) {
	resp, err := httpGet(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}

	hasher := sha256.New()
	bw := bufio.NewWriterSize(f, 256*1024)
	mw := io.MultiWriter(bw, hasher)
	_, copyErr := io.Copy(mw, io.LimitReader(resp.Body, 1<<30))
	flushErr := bw.Flush()
	closeErr := f.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if flushErr != nil {
		return "", flushErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// --- Cellar ---

// safeJoin joins base and rel, ensuring the result stays under base
// (defends against zip-slip / tar-slip).
func safeJoin(base, rel string) (string, error) {
	cleaned := filepath.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("unsafe path %q", rel)
	}
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute path %q in archive", rel)
	}
	target := filepath.Join(base, cleaned)
	if !strings.HasPrefix(target, base+string(os.PathSeparator)) && target != base {
		return "", fmt.Errorf("escaping path %q", rel)
	}
	return target, nil
}

// streamTarGz decompresses and extracts a tar.gz stream into dest, preserving
// directory structure.
func streamTarGz(r io.Reader, dest string) error {
	br := bufio.NewReaderSize(r, 128*1024)
	gz, err := gzip.NewReader(br)
	if err != nil {
		return err
	}
	defer gz.Close()

	buf := make([]byte, 256*1024)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			bw := bufio.NewWriter(out)
			if _, err := io.CopyBuffer(bw, tr, buf); err != nil {
				out.Close()
				return err
			}
			if err := bw.Flush(); err != nil {
				out.Close()
				return err
			}
			out.Close()
			// Re-apply mode in case umask masked the executable bit.
			os.Chmod(target, os.FileMode(hdr.Mode)&0o777)
		case tar.TypeSymlink, tar.TypeLink:
			// Skip — we only care about regular files.
			continue
		}
	}
	return nil
}

func extractZip(archive, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()

	buf := make([]byte, 256*1024)
	for _, f := range r.File {
		target, err := safeJoin(dest, f.Name)
		if err != nil {
			continue
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		mode := f.Mode() & 0o777
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		bw := bufio.NewWriter(out)
		if _, err := io.CopyBuffer(bw, rc, buf); err != nil {
			out.Close()
			rc.Close()
			return err
		}
		if err := bw.Flush(); err != nil {
			out.Close()
			rc.Close()
			return err
		}
		out.Close()
		rc.Close()
		os.Chmod(target, mode)
	}
	return nil
}

// detectBinaries walks dir and returns paths to executable binaries.
// It prefers files under bin/ subdirectories; if any are found, only
// those are returned to avoid linking ancillary tools at the archive root.
func detectBinaries(dir string) ([]string, error) {
	var all []string
	var inBin []string
	seen := map[string]bool{}

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode()&0o111 == 0 {
			return nil
		}
		if !isExecutableMagic(path) {
			return nil
		}
		// De-dup by basename — first match wins.
		if seen[name] {
			return nil
		}
		seen[name] = true
		all = append(all, path)

		// Track whether this path is under a "bin" component.
		rel, _ := filepath.Rel(dir, path)
		parts := strings.Split(rel, string(os.PathSeparator))
		for _, p := range parts[:len(parts)-1] {
			if strings.EqualFold(p, "bin") {
				inBin = append(inBin, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(inBin) > 0 {
		return inBin, nil
	}
	return all, nil
}

func isExecutableMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	// ELF
	if magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F' {
		return true
	}
	// Mach-O 32/64
	if magic[0] == 0xfe && magic[1] == 0xed && magic[2] == 0xfa && (magic[3] == 0xce || magic[3] == 0xcf) {
		return true
	}
	if (magic[0] == 0xce || magic[0] == 0xcf) && magic[1] == 0xfa && magic[2] == 0xed && magic[3] == 0xfe {
		return true
	}
	// Mach-O fat/universal
	if magic[0] == 0xca && magic[1] == 0xfe && magic[2] == 0xba && magic[3] == 0xbe {
		return true
	}
	// Windows PE
	if magic[0] == 'M' && magic[1] == 'Z' {
		return true
	}
	return false
}

func linkBins(bins []string) error {
	bdir := binDir()
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return err
	}

	for _, bin := range bins {
		linkName := filepath.Join(bdir, filepath.Base(bin))
		rel, err := filepath.Rel(bdir, bin)
		if err != nil {
			rel = bin
		}
		os.Remove(linkName)
		if err := os.Symlink(rel, linkName); err != nil {
			return err
		}
	}
	return nil
}

// unlinkBins removes any symlink in binDir whose resolved target points into pkgDir(name).
func unlinkBins(name string) {
	bdir := binDir()
	pdir := pkgDir(name)
	entries, err := os.ReadDir(bdir)
	if err != nil {
		return
	}
	for _, e := range entries {
		link := filepath.Join(bdir, e.Name())
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		// Resolve relative symlinks against bdir.
		resolved := target
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(bdir, target)
		}
		resolved = filepath.Clean(resolved)
		if resolved == pdir || strings.HasPrefix(resolved, pdir+string(os.PathSeparator)) {
			os.Remove(link)
		}
	}
}

// --- Helpers ---

var (
	sipDirOnce  sync.Once
	sipDirValue string
)

func sipDir() string {
	sipDirOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, color("31", "error: ")+"cannot determine home directory: "+err.Error())
			os.Exit(1)
		}
		sipDirValue = filepath.Join(home, ".sip")
	})
	return sipDirValue
}

func pkgDir(name string) string {
	return filepath.Join(sipDir(), "pkg", name)
}

func binDir() string {
	return filepath.Join(sipDir(), "bin")
}

func readPkgMetadata(name string) (repo, version string, err error) {
	dir := pkgDir(name)
	repoBytes, rerr := os.ReadFile(filepath.Join(dir, ".repo"))
	if rerr != nil {
		return "", "", fmt.Errorf("%s: missing .repo metadata", name)
	}
	versionBytes, verr := os.ReadFile(filepath.Join(dir, ".version"))
	if verr != nil {
		return "", "", fmt.Errorf("%s: missing .version metadata", name)
	}
	return strings.TrimSpace(string(repoBytes)), strings.TrimSpace(string(versionBytes)), nil
}

func color(code, msg string) string {
	return "\033[" + code + "m" + msg + "\033[0m"
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
