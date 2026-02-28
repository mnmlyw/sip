package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const version = "0.1.0"

var httpClient = &http.Client{Timeout: 30 * time.Second}

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
	switch os.Args[1] {
	case "--help", "-h":
		printUsage()
		os.Exit(0)
	case "--version", "-v":
		fmt.Println("sip " + version)
		os.Exit(0)
	}
	var err error
	switch os.Args[1] {
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

func installRelease(repo, name string, rel *Release) error {
	asset, err := pickAsset(rel.Assets)
	if err != nil {
		return err
	}

	fmt.Printf("%s %s (%s)\n", color("36", "downloading"), asset.Name, rel.TagName)

	// Download to temp file
	tmp, err := os.CreateTemp("", "sip-*-"+asset.Name)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := download(asset.URL, tmpPath); err != nil {
		return err
	}

	// Prepare pkg dir
	dest := pkgDir(name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	// Extract
	fmt.Printf("%s %s\n", color("36", "extracting"), asset.Name)
	lower := strings.ToLower(asset.Name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		if err := extractTarGz(tmpPath, dest); err != nil {
			os.RemoveAll(dest)
			return err
		}
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

	// Write metadata
	if err := os.WriteFile(filepath.Join(dest, ".repo"), []byte(repo), 0o644); err != nil {
		os.RemoveAll(dest)
		return err
	}
	if err := os.WriteFile(filepath.Join(dest, ".version"), []byte(rel.TagName), 0o644); err != nil {
		os.RemoveAll(dest)
		return err
	}

	// Detect and link binaries
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
		dir := pkgDir(name)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return fmt.Errorf("%s is not installed", name)
		}

		repoBytes, err := os.ReadFile(filepath.Join(dir, ".repo"))
		if err != nil {
			return fmt.Errorf("%s: missing .repo metadata", name)
		}
		versionBytes, err := os.ReadFile(filepath.Join(dir, ".version"))
		if err != nil {
			return fmt.Errorf("%s: missing .version metadata", name)
		}
		repo := strings.TrimSpace(string(repoBytes))
		current := strings.TrimSpace(string(versionBytes))

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

	var upgraded, total int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dir := pkgDir(name)
		total++

		repoBytes, err := os.ReadFile(filepath.Join(dir, ".repo"))
		if err != nil {
			fmt.Printf("%s %s: %v\n", color("33", "skip"), name, fmt.Errorf("missing .repo metadata"))
			continue
		}
		versionBytes, err := os.ReadFile(filepath.Join(dir, ".version"))
		if err != nil {
			fmt.Printf("%s %s: %v\n", color("33", "skip"), name, fmt.Errorf("missing .version metadata"))
			continue
		}
		repo := strings.TrimSpace(string(repoBytes))
		current := strings.TrimSpace(string(versionBytes))

		rel, err := fetchRelease(repo)
		if err != nil {
			fmt.Printf("%s %s: %v\n", color("33", "skip"), name, err)
			continue
		}

		if rel.TagName == current {
			fmt.Printf("%s %s (%s)\n", color("90", "up-to-date"), name, current)
			continue
		}

		fmt.Printf("%s %s %s → %s\n", color("36", "upgrading"), name, current, rel.TagName)

		if err := atomicUpgrade(name, repo, rel); err != nil {
			fmt.Printf("%s %s: %v\n", color("33", "skip"), name, err)
			continue
		}

		upgraded++
		fmt.Printf("%s %s (%s)\n", color("32", "upgraded"), name, rel.TagName)
	}

	fmt.Printf("upgraded %d/%d packages\n", upgraded, total)
	return nil
}

func atomicUpgrade(name, repo string, rel *Release) error {
	stagingName := name + ".new"
	stagingDir := pkgDir(stagingName)

	// Clean up any leftover staging dir
	os.RemoveAll(stagingDir)

	// Install new version to staging directory
	if err := installRelease(repo, stagingName, rel); err != nil {
		os.RemoveAll(stagingDir)
		unlinkBins(stagingName)
		return err
	}

	// Unlink staging bins (installRelease linked them under the staging name)
	unlinkBins(stagingName)

	// Swap: remove old, rename staging to final
	oldDir := pkgDir(name)
	unlinkBins(name)
	os.RemoveAll(oldDir)

	if err := os.Rename(stagingDir, oldDir); err != nil {
		return err
	}

	// Re-detect and link binaries from the final location
	bins, _ := detectBinaries(oldDir)
	if len(bins) > 0 {
		linkBins(name, bins)
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

func download(url, dest string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(f, io.LimitReader(resp.Body, 1<<30))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// --- Cellar ---

func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Skip directories and non-regular files
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Flatten: extract all files directly into dest
		name := filepath.Base(hdr.Name)
		if name == "." || name == ".." {
			continue
		}

		target := filepath.Join(dest, name)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
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
		if _, err := io.Copy(out, rc); err != nil {
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
		if isExecutableBinary(path) {
			bins = append(bins, path)
		}
	}
	return bins, nil
}

// isExecutableBinary checks file magic bytes for ELF, Mach-O, or Windows PE.
func isExecutableBinary(path string) bool {
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
	// Mach-O (32/64-bit, big/little endian)
	if magic[0] == 0xfe && magic[1] == 0xed && magic[2] == 0xfa && (magic[3] == 0xce || magic[3] == 0xcf) {
		return true
	}
	if (magic[0] == 0xce || magic[0] == 0xcf) && magic[1] == 0xfa && magic[2] == 0xed && magic[3] == 0xfe {
		return true
	}
	// Mach-O fat/universal binary
	if magic[0] == 0xca && magic[1] == 0xfe && magic[2] == 0xba && magic[3] == 0xbe {
		return true
	}
	// Windows PE
	if magic[0] == 'M' && magic[1] == 'Z' {
		return true
	}
	return false
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
	entries, err := os.ReadDir(bdir)
	if err != nil {
		return
	}
	prefix := filepath.Join("..", "pkg", name) + string(os.PathSeparator)
	for _, e := range entries {
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

func sipDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, color("31", "error: ")+"cannot determine home directory: "+err.Error())
		os.Exit(1)
	}
	return filepath.Join(home, ".sip")
}

func pkgDir(name string) string {
	return filepath.Join(sipDir(), "pkg", name)
}

func binDir() string {
	return filepath.Join(sipDir(), "bin")
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
