package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
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

const version = "0.1.0"

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
  sip i <owner/repo>   install a package
  sip r <name>          remove a package
  sip u [name]          upgrade all or one package
  sip l                 list installed packages
  sip s <query>         search installed packages
  sip n <name>          show package info
`)
}

// --- Commands ---

func runInstall(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sip i <owner/repo>")
	}
	repo := args[0]
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid repo format, expected owner/repo")
	}
	name := parts[1]

	// Check if already installed
	if _, err := os.Stat(pkgDir(name)); err == nil {
		return fmt.Errorf("%s is already installed", name)
	}

	fmt.Printf("%s %s\n", color("36", "fetching"), repo)
	rel, err := fetchRelease(repo)
	if err != nil {
		return err
	}

	return installRelease(repo, name, rel)
}

// downloadRelease downloads, extracts, and writes metadata to pkgDir(name).
// It does not link binaries or print the final "installed" message.
func downloadRelease(repo, name string, rel *Release) error {
	asset, err := pickAsset(rel.Assets)
	if err != nil {
		return err
	}

	fmt.Printf("%s %s (%s)\n", color("36", "downloading"), asset.Name, rel.TagName)

	// Prepare pkg dir
	dest := pkgDir(name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	lower := strings.ToLower(asset.Name)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		// Stream tar.gz directly from HTTP — no temp file
		resp, err := httpGet(asset.URL)
		if err != nil {
			os.RemoveAll(dest)
			return err
		}
		defer resp.Body.Close()

		fmt.Printf("%s %s\n", color("36", "extracting"), asset.Name)
		if err := streamTarGz(resp.Body, dest); err != nil {
			os.RemoveAll(dest)
			return err
		}
	} else {
		// Download to temp file for zip / raw binary
		tmp, err := os.CreateTemp("", "sip-*-"+asset.Name)
		if err != nil {
			os.RemoveAll(dest)
			return err
		}
		tmpPath := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpPath)

		if err := download(asset.URL, tmpPath); err != nil {
			os.RemoveAll(dest)
			return err
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

	// Write metadata
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

	if err := linkBins(name, bins); err != nil {
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

		rel, err := fetchRelease(repo)
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

	// Phase 1: Read metadata (local) and fetch releases (network) in parallel
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
			rel, err := fetchRelease(repo)
			results[i] = fetchResult{rel, err}
		}(i, p.repo)
	}
	wg.Wait()

	// Phase 2: Filter up-to-date packages, upgrade the rest in parallel
	type upgradeJob struct {
		idx  int
		pkg  pkgInfo
		rel  *Release
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

	// Clean up any leftover staging dir
	os.RemoveAll(stagingDir)

	// Download new version to staging directory (no bin linking)
	if err := downloadRelease(repo, stagingName, rel); err != nil {
		os.RemoveAll(stagingDir)
		return err
	}

	// Swap: remove old, rename staging to final
	oldDir := pkgDir(name)
	unlinkBins(name)
	os.RemoveAll(oldDir)

	if err := os.Rename(stagingDir, oldDir); err != nil {
		return err
	}

	// Detect and link binaries from the final location
	bins, _ := detectBinaries(oldDir)
	if len(bins) > 0 {
		if err := linkBins(name, bins); err != nil {
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

func fetchRelease(repo string) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
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

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github api: %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func pickAsset(assets []Asset) (*Asset, error) {
	if len(assets) == 0 {
		return nil, fmt.Errorf("no release assets found")
	}

	goos := runtime.GOOS
	goarch := runtime.GOARCH

	type scored struct {
		asset Asset
		score int
	}

	var candidates []scored
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		s := 0

		// OS matching
		switch goos {
		case "darwin":
			if strings.Contains(name, "darwin") || strings.Contains(name, "macos") || strings.Contains(name, "apple") {
				s += 10
			}
		case "linux":
			if strings.Contains(name, "linux") {
				s += 10
			}
		case "windows":
			if strings.Contains(name, "windows") || strings.Contains(name, "win64") || strings.Contains(name, "win32") {
				s += 10
			}
		}

		// Arch matching
		switch goarch {
		case "arm64":
			if strings.Contains(name, "arm64") || strings.Contains(name, "aarch64") {
				s += 10
			}
		case "amd64":
			if strings.Contains(name, "amd64") || strings.Contains(name, "x86_64") || strings.Contains(name, "x64") {
				s += 10
			}
		}

		// Format preference
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			s += 5
		} else if strings.HasSuffix(name, ".zip") {
			s += 3
		}

		// Negative signals — wrong OS or non-binary files
		if goos != "linux" && strings.Contains(name, "linux") {
			s -= 100
		}
		if goos != "windows" && (strings.Contains(name, "windows") || strings.Contains(name, "win64") || strings.Contains(name, "win32") || strings.Contains(name, ".exe") || strings.Contains(name, ".msi")) {
			s -= 100
		}
		if goos != "darwin" && (strings.Contains(name, "darwin") || strings.Contains(name, "macos")) {
			s -= 100
		}
		if strings.HasSuffix(name, ".sha256") || strings.HasSuffix(name, ".sig") ||
			strings.HasSuffix(name, ".asc") || strings.HasSuffix(name, ".sbom") ||
			strings.HasSuffix(name, ".txt") || strings.HasSuffix(name, ".json") {
			s -= 100
		}
		// Wrong arch
		if goarch == "arm64" && (strings.Contains(name, "amd64") || strings.Contains(name, "x86_64") || strings.Contains(name, "x64")) {
			s -= 100
		}
		if goarch == "amd64" && (strings.Contains(name, "arm64") || strings.Contains(name, "aarch64")) {
			s -= 100
		}
		// musl vs gnu — prefer gnu/default on non-musl systems
		if strings.Contains(name, "musl") {
			s -= 1
		}

		candidates = append(candidates, scored{a, s})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no compatible assets found")
	}

	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.score > best.score {
			best = c
		}
	}

	if best.score < 0 {
		return nil, fmt.Errorf("no compatible asset for %s/%s", goos, goarch)
	}

	return &best.asset, nil
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

func download(url, dest string) error {
	resp, err := httpGet(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	bw := bufio.NewWriterSize(f, 256*1024)
	_, copyErr := io.Copy(bw, io.LimitReader(resp.Body, 1<<30))
	flushErr := bw.Flush()
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// --- Cellar ---

// streamTarGz decompresses and extracts a tar.gz stream directly into dest.
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

		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(hdr.Name)
		if name == "." || name == ".." {
			continue
		}

		target := filepath.Join(dest, name)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(hdr.Mode))
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
		if f.FileInfo().IsDir() {
			continue
		}

		// Flatten
		name := filepath.Base(f.Name)
		if name == "." || name == ".." {
			continue
		}

		target := filepath.Join(dest, name)
		rc, err := f.Open()
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, f.Mode())
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
	}
	return nil
}

func detectBinaries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var magic [4]byte
	var bins []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0o111 == 0 {
			continue
		}
		path := filepath.Join(dir, name)

		f, err := os.Open(path)
		if err != nil {
			continue
		}
		_, err = io.ReadFull(f, magic[:])
		f.Close()
		if err != nil {
			continue
		}

		// ELF
		if magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F' {
			bins = append(bins, path)
			continue
		}
		// Mach-O (32/64-bit, big/little endian)
		if magic[0] == 0xfe && magic[1] == 0xed && magic[2] == 0xfa && (magic[3] == 0xce || magic[3] == 0xcf) {
			bins = append(bins, path)
			continue
		}
		if (magic[0] == 0xce || magic[0] == 0xcf) && magic[1] == 0xfa && magic[2] == 0xed && magic[3] == 0xfe {
			bins = append(bins, path)
			continue
		}
		// Mach-O fat/universal binary
		if magic[0] == 0xca && magic[1] == 0xfe && magic[2] == 0xba && magic[3] == 0xbe {
			bins = append(bins, path)
			continue
		}
		// Windows PE
		if magic[0] == 'M' && magic[1] == 'Z' {
			bins = append(bins, path)
			continue
		}
	}
	return bins, nil
}

func linkBins(name string, bins []string) error {
	bdir := binDir()
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return err
	}

	for _, bin := range bins {
		linkName := filepath.Join(bdir, filepath.Base(bin))
		// Relative symlink: ../pkg/<name>/<binary>
		relTarget := filepath.Join("..", "pkg", name, filepath.Base(bin))
		os.Remove(linkName) // remove stale link
		if err := os.Symlink(relTarget, linkName); err != nil {
			return err
		}
	}
	return nil
}

func unlinkBins(name string) {
	bdir := binDir()
	pdir := pkgDir(name)
	entries, err := os.ReadDir(pdir)
	if err != nil {
		return
	}
	prefix := filepath.Join("..", "pkg", name) + string(os.PathSeparator)
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		link := filepath.Join(bdir, e.Name())
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		if strings.HasPrefix(target, prefix) {
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
