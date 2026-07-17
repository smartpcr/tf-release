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
		// file's relative path (drive + leading separators stripped) so basenames
		// from different directories don't collide, then Compress-Archive'd.
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
  $rel = $p -replace '^[A-Za-z]:[\\/]+','' -replace '^[\\/]+',''
  $target = Join-Path $stage $rel
  New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force | Out-Null
  Copy-Item -LiteralPath $p -Destination $target -Force
}
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath %s -Force
Remove-Item -Recurse -Force $stage
Write-Output 'ZIPPED'
exit 0`, strings.Join(items, ","), psq(archive))}
	}
	// Injection-safe Linux staging. Each glob is passed as DATA to a `stage`
	// function whose args are BOTH single-quoted (shq); `find -path` — not the
	// shell — interprets the wildcards, so command substitutions/metacharacters
	// inside a manifest glob are inert. Files are copied into a temp dir that
	// PRESERVES their relative path (leading '/' stripped) before tar.
	var b strings.Builder
	b.WriteString(`set +e
tmp=$(mktemp -d) || exit 1
n=0
stage(){
  find "$1" -type f -path "$2" 2>/dev/null | while IFS= read -r f; do
    rel=${f#/}
    d=$tmp/$(dirname "$rel")
    mkdir -p "$d"
    cp "$f" "$d"/ 2>/dev/null && echo x
  done | wc -l
}
`)
	for _, g := range globs {
		root := globRoot(g)
		b.WriteString(fmt.Sprintf("c=$(stage %s %s); n=$((n+c))\n", shq(root), shq(g)))
	}
	b.WriteString(fmt.Sprintf(`if [ "$n" = "0" ]; then rm -rf "$tmp"; echo NOFILES; exit 0; fi
tar czf %s -C "$tmp" .
rm -rf "$tmp"
echo ZIPPED`, shq(archive)))
	return transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 300, Script: b.String()}
}

// globRoot returns the deepest non-wildcard directory prefix of a POSIX glob so
// `find` can start from a bounded root instead of scanning the whole filesystem.
func globRoot(pattern string) string {
	if !strings.HasPrefix(pattern, "/") {
		return "."
	}
	segs := strings.Split(pattern, "/")
	var kept []string
	for _, s := range segs {
		if strings.ContainsAny(s, "*?[") {
			break
		}
		kept = append(kept, s)
	}
	root := strings.Join(kept, "/")
	if root == "" {
		return "/"
	}
	// Drop the trailing element if it's a filename (i.e. the prefix ended mid-path
	// with no wildcard yet); keep only the directory portion as the search root.
	if len(kept) == len(segs) {
		root = strings.Join(kept[:len(kept)-1], "/")
		if root == "" {
			return "/"
		}
	}
	return root
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
// BASENAME matches one of the result patterns' basenames. Both the patterns and
// the matched paths are DEDUPLICATED so a file is never counted twice even when
// overlapping/duplicate patterns are supplied (e.g. "*.trx" listed twice, or the
// archive was extracted into nested directories).
func SumResults(format string, baseDir string, patterns []string) (Counters, []string, error) {
	var total Counters
	wantBase := make(map[string]struct{})
	for _, pat := range patterns {
		wantBase[filepath.Base(pat)] = struct{}{}
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
		base := filepath.Base(path)
		for want := range wantBase {
			ok, mErr := filepath.Match(want, base)
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

var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitize(s string) string { return unsafeRe.ReplaceAllString(s, "_") }

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
