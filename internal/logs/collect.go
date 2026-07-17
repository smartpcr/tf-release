// Package logs collects target logs/event logs and parses test results
// (DESIGN §8.5 + TestRun §7).
package logs

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// CollectFiles zips remote glob matches, downloads the archive to destDir/<host>/,
// and EXTRACTS it there so callers get ready-to-parse files. The archive PRESERVES
// each file's relative directory (drive/leading-slash stripped) so two files that
// share a basename in different directories never collide; extraction is Zip-Slip
// guarded and reports (never silently overwrites) any residual collision. No
// external unzip/tar dependency. Best-effort: returns extracted file paths +
// warnings, never a hard failure.
func CollectFiles(ctx context.Context, t transport.Transport, globs []string, destDir string) ([]string, []string) {
	var warns []string
	if len(globs) == 0 {
		return nil, warns
	}
	hostDir := filepath.Join(destDir, sanitize(t.Host()))
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return nil, []string{fmt.Sprintf("mkdir %s: %v", hostDir, err)}
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")

	var remoteArchive, localArchive string
	var rmCmd transport.Cmd
	if t.OS() == spec.OSWindows {
		remoteArchive = fmt.Sprintf(`%s\labdeploy-logs-%s.zip`, `C:\Windows\Temp`, stamp)
		localArchive = filepath.Join(hostDir, "logs.zip")
		rmCmd = transport.Cmd{Shell: transport.ShellPowerShell,
			Script: fmt.Sprintf(`Remove-Item -Force -ErrorAction SilentlyContinue %s`, psq(remoteArchive)), TimeoutSec: 30}
	} else {
		remoteArchive = fmt.Sprintf("/tmp/labdeploy-logs-%s.tar.gz", stamp)
		localArchive = filepath.Join(hostDir, "logs.tar.gz")
		rmCmd = transport.Cmd{Shell: transport.ShellSh, Script: "rm -f " + shq(remoteArchive), TimeoutSec: 30}
	}

	cmd := buildZipScript(t.OS(), globs, remoteArchive)
	r, err := t.Exec(ctx, cmd)
	if err != nil || r.ExitCode != 0 {
		return nil, []string{fmt.Sprintf("log archive on %s: err=%v %s", t.Host(), err, r.Stderr)}
	}
	if strings.Contains(r.Stdout, "NOFILES") {
		return nil, []string{fmt.Sprintf("no log files matched on %s: %v", t.Host(), globs)}
	}
	if err := t.Download(ctx, remoteArchive, localArchive); err != nil {
		return nil, []string{fmt.Sprintf("log download from %s: %v", t.Host(), err)}
	}
	_, _ = t.Exec(ctx, rmCmd)

	extracted, exWarns, err := extractArchive(localArchive, hostDir)
	warns = append(warns, exWarns...)
	if err != nil {
		warns = append(warns, fmt.Sprintf("extract %s: %v", localArchive, err))
	}
	_ = os.Remove(localArchive) // keep only the extracted files
	return extracted, warns
}

// extractArchive unpacks a .zip or .tar.gz into destDir, PRESERVING each entry's
// relative path (Zip-Slip guarded). Returns extracted paths + collision warnings.
func extractArchive(archivePath, destDir string) ([]string, []string, error) {
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return extractZip(archivePath, destDir)
	}
	return extractTarGz(archivePath, destDir)
}

func extractZip(archivePath, destDir string) ([]string, []string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, nil, err
	}
	defer zr.Close()
	var out, warns []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		dst, ok := safeExtractPath(destDir, f.Name)
		if !ok {
			warns = append(warns, "skipped unsafe archive entry "+f.Name)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return out, warns, err
		}
		final, collided, err := writeExtractedUnique(dst, rc)
		rc.Close()
		if err != nil {
			return out, warns, err
		}
		if collided {
			warns = append(warns, fmt.Sprintf("basename collision: wrote %s for entry %s", final, f.Name))
		}
		out = append(out, final)
	}
	return out, warns, nil
}

func extractTarGz(archivePath, destDir string) ([]string, []string, error) {
	fh, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		return nil, nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var out, warns []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, warns, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		dst, ok := safeExtractPath(destDir, hdr.Name)
		if !ok {
			warns = append(warns, "skipped unsafe archive entry "+hdr.Name)
			continue
		}
		final, collided, err := writeExtractedUnique(dst, tr)
		if err != nil {
			return out, warns, err
		}
		if collided {
			warns = append(warns, fmt.Sprintf("basename collision: wrote %s for entry %s", final, hdr.Name))
		}
		out = append(out, final)
	}
	return out, warns, nil
}

// safeExtractPath maps an archive entry name to a destination UNDER destDir,
// rejecting absolute paths and `..` traversal (Zip-Slip). Returns (path, ok).
func safeExtractPath(destDir, name string) (string, bool) {
	clean := filepath.Clean("/" + filepath.ToSlash(name))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", false
	}
	dst := filepath.Join(destDir, filepath.FromSlash(clean))
	rel, err := filepath.Rel(destDir, dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return dst, true
}

// writeExtractedUnique writes src to dst, creating parent dirs. If dst already
// exists it does NOT truncate: it writes to a `.dupN`-suffixed sibling and reports
// collided=true so the caller can surface a warning instead of losing data.
func writeExtractedUnique(dst string, src io.Reader) (string, bool, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", false, err
	}
	final, collided := dst, false
	ext := filepath.Ext(dst)
	stem := strings.TrimSuffix(dst, ext)
	for i := 1; ; i++ {
		if _, err := os.Stat(final); os.IsNotExist(err) {
			break
		}
		collided = true
		final = fmt.Sprintf("%s.dup%d%s", stem, i, ext)
	}
	f, err := os.OpenFile(final, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", collided, err
	}
	defer f.Close()
	if _, err := io.Copy(f, src); err != nil {
		return "", collided, err
	}
	return final, collided, nil
}

// buildZipScript renders the glob-to-archive command executed ON THE TARGET
// (DESIGN §8.5, T8 "glob→zip script golden"). Windows uses Compress-Archive over
// Get-ChildItem matches (which flattens to basenames and expands wildcards even
// from quoted -Path values). Linux copies each glob match into a temp dir and
// tars THAT (flat, basename-only entries) so the extracted layout is
// discoverable by relative/basename result globs. Emits the sentinel "NOFILES"
// (exit 0) when nothing matched. Pure function so it can be golden-tested.
func buildZipScript(os spec.OSKind, globs []string, archive string) transport.Cmd {
	if os == spec.OSWindows {
		// Each glob is single-quoted (psq) and fed to Get-ChildItem as DATA, never
		// evaluated as code. Matches are staged into a temp dir that PRESERVES each
		// file's relative path INCLUDING a drive identifier (C:\a.log -> C\a.log,
		// \\srv\share\a.log -> UNC\srv\share\a.log) so identically named files on
		// different drives/shares don't collide, then Compress-Archive'd.
		items := make([]string, len(globs))
		for i, g := range globs {
			items[i] = psq(g)
		}
		return transport.Cmd{Shell: transport.ShellPowerShell, TimeoutSec: 300, Script: fmt.Sprintf(
			`$ErrorActionPreference='Continue'
$stage = Join-Path $env:TEMP ('labdeploy-stage-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $stage -Force | Out-Null
$paths = @(%s) | ForEach-Object { Get-ChildItem -Path $_ -File -ErrorAction SilentlyContinue } | Select-Object -ExpandProperty FullName -Unique
if(-not $paths){ Remove-Item -Recurse -Force $stage; Write-Output 'NOFILES'; exit 0 }
foreach($p in $paths){
  if($p -match '^([A-Za-z]):[\\/]+'){ $rel = $Matches[1] + '\' + ($p -replace '^[A-Za-z]:[\\/]+','') }
  elseif($p -match '^[\\/]{2}'){ $rel = 'UNC\' + ($p -replace '^[\\/]{2}','') }
  else { $rel = $p -replace '^[\\/]+','' }
  $target = Join-Path $stage $rel
  New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force | Out-Null
  Copy-Item -LiteralPath $p -Destination $target -Force
}
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath %s -Force
Remove-Item -Recurse -Force $stage
Write-Output 'ZIPPED'
exit 0`, strings.Join(items, ","), psq(archive))}
	}
	// Injection-safe Linux staging with SEGMENT-AWARE glob semantics using ONLY
	// portable `find` options (GNU, BSD and BusyBox all accept
	// -maxdepth/-mindepth/-path/-type). For each glob we pin the search DEPTH to
	// the exact number of path components below its non-wildcard root, so `-path`'s
	// `*` cannot span a `/` (a match always has precisely that many components).
	// The glob is passed as DATA (single-quoted, shq) to a `stage` function; find —
	// not the shell — matches, so command substitutions inside a manifest glob are
	// inert. find's exit status is checked and surfaced (FINDERR/exit 3) rather than
	// silently degrading to NOFILES on an unsupported/failed scan.
	var b strings.Builder
	b.WriteString(`set +e
tmp=$(mktemp -d) || exit 1
stagedir=$tmp/stage
mkdir -p "$stagedir"
n=0
ferr=0
stage(){
  find "$1" -maxdepth "$2" -mindepth "$2" -type f -path "$3" > "$tmp/list" 2> "$tmp/err"
  if [ $? -ne 0 ]; then ferr=1; cat "$tmp/err" >&2; return; fi
  while IFS= read -r f; do
    rel=${f#/}
    rel=${rel#./}
    d=$stagedir/$(dirname "$rel")
    mkdir -p "$d"
    cp "$f" "$d"/ 2>/dev/null && n=$((n+1))
  done < "$tmp/list"
}
`)
	for _, g := range globs {
		root := globRoot(g)
		depth := findDepth(g)
		patt := g
		if !strings.HasPrefix(g, "/") {
			// find prints relative results as ./<path>; match that form.
			patt = "./" + g
		}
		b.WriteString(fmt.Sprintf("stage %s %s %s\n", shq(root), shq(strconv.Itoa(depth)), shq(patt)))
	}
	b.WriteString(fmt.Sprintf(`if [ "$ferr" -ne 0 ]; then rm -rf "$tmp"; echo FINDERR; exit 3; fi
if [ "$n" = "0" ]; then rm -rf "$tmp"; echo NOFILES; exit 0; fi
tar czf %s -C "$stagedir" .
rm -rf "$tmp"
echo ZIPPED`, shq(archive)))
	return transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 300, Script: b.String()}
}

// findDepth returns the number of path components below a glob's non-wildcard
// root (globRoot). Pinning find's -maxdepth == -mindepth to this value keeps
// `-path`'s wildcards segment-aware without relying on GNU-only -regex.
func findDepth(g string) int {
	trimmed := strings.TrimPrefix(g, "/")
	gSegs := len(strings.Split(trimmed, "/"))
	root := globRoot(g)
	rootTrim := strings.TrimPrefix(root, "/")
	rootTrim = strings.TrimPrefix(rootTrim, "./")
	if rootTrim == "." {
		rootTrim = ""
	}
	rootN := 0
	if rootTrim != "" {
		rootN = len(strings.Split(rootTrim, "/"))
	}
	d := gSegs - rootN
	if d < 1 {
		d = 1
	}
	return d
}

// globRoot returns the deepest non-wildcard directory prefix of a POSIX glob so
// `find` starts from a bounded root instead of scanning the whole filesystem.
// The returned root matches how find prints paths: absolute globs yield an
// absolute root, relative globs yield a `./`-prefixed root.
func globRoot(pattern string) string {
	abs := strings.HasPrefix(pattern, "/")
	segs := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	var kept []string
	// Never include the final (filename) segment as a search-root component.
	for _, s := range segs[:len(segs)-1] {
		if strings.ContainsAny(s, "*?[") {
			break
		}
		kept = append(kept, s)
	}
	if abs {
		return "/" + strings.Join(kept, "/")
	}
	if len(kept) == 0 {
		return "."
	}
	return "./" + strings.Join(kept, "/")
}

// CollectEventLogs exports matching Windows events since `since` as JSON lines
// into destDir/<host>/events-<log>.json (DESIGN §7 collect.windows_event_logs).
func CollectEventLogs(ctx context.Context, t transport.Transport, specs []spec.EventLogSpec, since time.Time, destDir string) ([]string, []string) {
	var out, warns []string
	if len(specs) == 0 || t.OS() != spec.OSWindows {
		return out, warns
	}
	hostDir := filepath.Join(destDir, sanitize(t.Host()))
	_ = os.MkdirAll(hostDir, 0o755)
	for _, es := range specs {
		prov := ""
		if es.Provider != "" {
			prov = fmt.Sprintf("; ProviderName=%s", psq(es.Provider))
		}
		lv := es.MaxLevel
		if lv == 0 {
			lv = 3 // Error+Warning+Critical
		}
		script := fmt.Sprintf(`$f = @{LogName=%s; StartTime=[datetime]::Parse(%s)%s}
$evts = Get-WinEvent -FilterHashtable $f -MaxEvents 500 -ErrorAction SilentlyContinue | Where-Object { $_.Level -le %d -and $_.Level -ge 1 }
$evts | Select-Object TimeCreated, Id, LevelDisplayName, ProviderName, Message | ConvertTo-Json -Depth 3
exit 0`, psq(es.Log), psq(since.UTC().Format(time.RFC3339)), prov, lv)
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 120})
		if err != nil || r.ExitCode != 0 {
			warns = append(warns, fmt.Sprintf("event log %s on %s: err=%v %s", es.Log, t.Host(), err, r.Stderr))
			continue
		}
		body := strings.TrimSpace(r.Stdout)
		if body == "" {
			body = "[]"
		}
		local := filepath.Join(hostDir, "events-"+sanitize(es.Log)+".json")
		if err := os.WriteFile(local, []byte(body), 0o644); err != nil {
			warns = append(warns, fmt.Sprintf("write %s: %v", local, err))
			continue
		}
		out = append(out, local)
	}
	return out, warns
}

// ----------------------------- result parsing --------------------------------

type Counters struct{ Total, Passed, Failed, Skipped int }

// ParseTRX reads <ResultSummary><Counters .../> (DESIGN §7 results.format=trx).
func ParseTRX(path string) (Counters, error) {
	var doc struct {
		XMLName       xml.Name `xml:"TestRun"`
		ResultSummary struct {
			Counters struct {
				Total       int `xml:"total,attr"`
				Passed      int `xml:"passed,attr"`
				Failed      int `xml:"failed,attr"`
				NotExecuted int `xml:"notExecuted,attr"`
			} `xml:"Counters"`
		} `xml:"ResultSummary"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Counters{}, err
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return Counters{}, fmt.Errorf("trx parse %s: %w", path, err)
	}
	c := doc.ResultSummary.Counters
	return Counters{Total: c.Total, Passed: c.Passed, Failed: c.Failed, Skipped: c.NotExecuted}, nil
}

// ParseJUnit sums <testsuite tests/failures/errors/skipped> across suites.
func ParseJUnit(path string) (Counters, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Counters{}, err
	}
	type suite struct {
		Tests    int `xml:"tests,attr"`
		Failures int `xml:"failures,attr"`
		Errors   int `xml:"errors,attr"`
		Skipped  int `xml:"skipped,attr"`
	}
	var out Counters
	add := func(s suite) {
		out.Total += s.Tests
		out.Failed += s.Failures + s.Errors
		out.Skipped += s.Skipped
	}
	var multi struct {
		XMLName xml.Name `xml:"testsuites"`
		Suites  []suite  `xml:"testsuite"`
	}
	if err := xml.Unmarshal(raw, &multi); err == nil && multi.XMLName.Local == "testsuites" {
		for _, s := range multi.Suites {
			add(s)
		}
	} else {
		var single struct {
			XMLName xml.Name `xml:"testsuite"`
			suite
		}
		if err := xml.Unmarshal(raw, &single); err != nil {
			return Counters{}, fmt.Errorf("junit parse %s: %w", path, err)
		}
		add(single.suite)
	}
	out.Passed = out.Total - out.Failed - out.Skipped
	return out, nil
}

// SumResults walks baseDir recursively and merges counters from every file whose
// PATH matches one of the result patterns DIRECTORY-AWARELY: a pattern's trailing
// path segments (e.g. `expected/*.trx`) must align with the file's own trailing
// segments, so a stale/misplaced `other/foo.trx` does NOT satisfy `expected/*.trx`
// (unlike a bare basename match). Matched paths are DEDUPLICATED so a file is
// never counted twice even with overlapping/duplicate patterns.
func SumResults(format string, baseDir string, patterns []string) (Counters, []string, error) {
	var total Counters
	// Dedup patterns (as normalized slash strings) and split each into segments.
	patSegs := make([][]string, 0, len(patterns))
	seenPat := make(map[string]struct{})
	for _, pat := range patterns {
		norm := strings.Trim(filepath.ToSlash(pat), "/")
		if norm == "" {
			continue
		}
		if _, dup := seenPat[norm]; dup {
			continue
		}
		seenPat[norm] = struct{}{}
		patSegs = append(patSegs, strings.Split(norm, "/"))
	}
	seen := make(map[string]struct{})
	var files []string
	err := filepath.WalkDir(baseDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort walk
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(baseDir, path)
		if rerr != nil {
			return nil
		}
		fileSegs := strings.Split(filepath.ToSlash(rel), "/")
		for _, ps := range patSegs {
			ok, mErr := matchSegmentsSuffix(ps, fileSegs)
			if mErr != nil {
				return mErr
			}
			if ok {
				if _, dup := seen[path]; !dup {
					seen[path] = struct{}{}
					files = append(files, path)
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		return total, files, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return total, files, fmt.Errorf("no result files matched %v under %s", patterns, baseDir)
	}
	for _, f := range files {
		var c Counters
		var perr error
		if format == "trx" {
			c, perr = ParseTRX(f)
		} else {
			c, perr = ParseJUnit(f)
		}
		if perr != nil {
			return total, files, perr
		}
		total.Total += c.Total
		total.Passed += c.Passed
		total.Failed += c.Failed
		total.Skipped += c.Skipped
	}
	return total, files, nil
}

// matchSegmentsSuffix reports whether the pattern segments match the TRAILING
// segments of the file's path, each segment matched independently (so `*` never
// crosses a directory boundary). Requires the file to have at least as many
// segments as the pattern. This enforces `expected/*.trx`-style directory-aware
// matching while remaining agnostic to any absolute prefix embedded during
// extraction.
func matchSegmentsSuffix(patSegs, fileSegs []string) (bool, error) {
	if len(patSegs) > len(fileSegs) {
		return false, nil
	}
	off := len(fileSegs) - len(patSegs)
	for i, ps := range patSegs {
		ok, err := filepath.Match(ps, fileSegs[off+i])
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitize(s string) string { return unsafeRe.ReplaceAllString(s, "_") }

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
