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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// CollectFiles zips remote glob matches, downloads the archive to destDir/<host>/,
// and EXTRACTS it there so callers get ready-to-parse files (flattened to
// basenames) — no external unzip/tar dependency. Best-effort: returns the
// extracted file paths + warnings, never a hard failure.
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

	extracted, err := extractArchive(localArchive, hostDir)
	if err != nil {
		return nil, []string{fmt.Sprintf("extract %s: %v", localArchive, err)}
	}
	_ = os.Remove(localArchive) // keep only the extracted files
	return extracted, warns
}

// extractArchive unpacks a .zip or .tar.gz into destDir, FLATTENING every entry
// to its basename (guards against Zip-Slip and guarantees the extracted layout
// is discoverable by relative/basename result globs). Returns extracted paths.
func extractArchive(archivePath, destDir string) ([]string, error) {
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return extractZip(archivePath, destDir)
	}
	return extractTarGz(archivePath, destDir)
}

func extractZip(archivePath, destDir string) ([]string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return out, err
		}
		dst := filepath.Join(destDir, filepath.Base(f.Name))
		if err := writeExtracted(dst, rc); err != nil {
			rc.Close()
			return out, err
		}
		rc.Close()
		out = append(out, dst)
	}
	return out, nil
}

func extractTarGz(archivePath, destDir string) ([]string, error) {
	fh, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var out []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		dst := filepath.Join(destDir, filepath.Base(hdr.Name))
		if err := writeExtracted(dst, tr); err != nil {
			return out, err
		}
		out = append(out, dst)
	}
	return out, nil
}

func writeExtracted(dst string, src io.Reader) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, src)
	return err
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
		items := make([]string, len(globs))
		for i, g := range globs {
			items[i] = psq(g)
		}
		return transport.Cmd{Shell: transport.ShellPowerShell, TimeoutSec: 300, Script: fmt.Sprintf(
			`$ErrorActionPreference='Continue'
$paths = @(%s) | ForEach-Object { Get-ChildItem -Path $_ -File -ErrorAction SilentlyContinue } | Select-Object -ExpandProperty FullName -Unique
if(-not $paths){ Write-Output 'NOFILES'; exit 0 }
Compress-Archive -Path $paths -DestinationPath %s -Force
Write-Output 'ZIPPED'
exit 0`, strings.Join(items, ","), psq(archive))}
	}
	// Glob patterns are intentionally LEFT UNQUOTED so the shell expands the
	// wildcards; matches are copied (flattened) into a temp dir which is then
	// tarred, so the archive holds basename-only entries. The archive path and
	// temp dir ARE quoted.
	return transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 300, Script: fmt.Sprintf(
		`set +e
tmp=$(mktemp -d) || exit 1
n=0
for f in %s; do
  [ -f "$f" ] || continue
  cp "$f" "$tmp"/ 2>/dev/null && n=$((n+1))
done
if [ "$n" = "0" ]; then rm -rf "$tmp"; echo NOFILES; exit 0; fi
tar czf %s -C "$tmp" .
rm -rf "$tmp"
echo ZIPPED`, strings.Join(globs, " "), shq(archive))}
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

// SumResults globs results paths under baseDir and merges counters.
func SumResults(format string, baseDir string, patterns []string) (Counters, []string, error) {
	var total Counters
	var files []string
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(baseDir, pat))
		if err != nil {
			return total, files, err
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		return total, files, fmt.Errorf("no result files matched %v under %s", patterns, baseDir)
	}
	for _, f := range files {
		var c Counters
		var err error
		if format == "trx" {
			c, err = ParseTRX(f)
		} else {
			c, err = ParseJUnit(f)
		}
		if err != nil {
			return total, files, err
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
