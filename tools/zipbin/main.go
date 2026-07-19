// Command zipbin packages a single built provider binary into the
// filesystem-mirror artifact zip that GoReleaser's release pipeline also emits,
// but does so as a per-target build hook so the plain `goreleaser build`
// subcommand produces the `terraform-provider-labdeploy_v<version>_<os>_<arch>.zip`
// matrix too (the `build` subcommand never runs the `archives` stage).
//
// Usage: zipbin <binaryPath> <version> <os> <arch>
//
// The zip is written next to the binary (inside GoReleaser's per-target output
// directory) so it never collides with the release `archives` stage, which
// writes the same file name into the dist root. Its contents mirror the release
// archive exactly: the versioned binary -- stored executable (mode 0755), just
// as GoReleaser force-sets it -- plus the same auxiliary files GoReleaser
// bundles by default (README*, LICENSE*, CHANGELOG*), so build-hook and release
// archives are interchangeable.
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// binaryMode mirrors GoReleaser's archive pipeline, which force-sets built
// binaries to 0755 (config archive.BuildsInfo.Mode) regardless of the mode the
// build host reports. Windows/NTFS reports a cross-compiled linux binary as
// mode 0666 (no executable bit); shipping that in the linux zip makes
// `terraform init`/`plan` fail with `fork/exec ... permission denied` once the
// binary is installed into the filesystem mirror on a linux runner (DESIGN
// §16.1). Forcing 0755 keeps the build-hook zip interchangeable with the
// release archive on every build host.
const binaryMode os.FileMode = 0o755

// bundledGlobs mirrors GoReleaser's default archive `files` globs (see its
// internal/pipe/archive Default()), so the build-hook zip bundles exactly what
// the release `archives` stage does -- including any LICENSE*/CHANGELOG* added
// to the repo later, which a hard-coded README-only list would silently drop.
var bundledGlobs = []string{
	"license*",
	"LICENSE*",
	"readme*",
	"README*",
	"changelog*",
	"CHANGELOG*",
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: zipbin <binaryPath> <version> <os> <arch>")
		os.Exit(2)
	}
	binPath, version, goos, goarch := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	zipName := fmt.Sprintf("terraform-provider-labdeploy_v%s_%s_%s.zip", version, goos, goarch)
	zipPath := filepath.Join(filepath.Dir(binPath), zipName)

	if err := writeZip(zipPath, binPath); err != nil {
		fmt.Fprintf(os.Stderr, "zipbin: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("zipbin: wrote %s\n", zipPath)
}

// writeZip creates zipPath containing binPath (stored executable under its base
// name) plus the auxiliary files GoReleaser bundles by default, matching the
// release pipeline's archive layout. The hook runs with the repo root as its
// working directory, so the bundled-file globs resolve relative to it.
func writeZip(zipPath, binPath string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := zip.NewWriter(out)

	// GoReleaser adds the default bundled files first (sorted, de-duplicated by
	// archive name) and the binary last; mirror that ordering and preserve each
	// bundled file's on-disk mode, exactly as GoReleaser's default file entries
	// (which carry no explicit mode) do.
	bundled, err := bundledFiles()
	if err != nil {
		return err
	}
	for _, name := range bundled {
		if err := addFile(zw, name, name, 0); err != nil {
			return err
		}
	}

	// Force the provider binary to 0755 so the linux artifact stays executable
	// even when built on a Windows/NTFS host (see binaryMode).
	if err := addFile(zw, binPath, filepath.Base(binPath), binaryMode); err != nil {
		return err
	}

	return zw.Close()
}

// bundledFiles resolves bundledGlobs against the current (repo-root) working
// directory into the set of regular files to bundle, de-duplicated by archive
// name and sorted, matching how GoReleaser evaluates its default archive files.
func bundledFiles() ([]string, error) {
	seen := make(map[string]bool)
	var names []string
	for _, pattern := range bundledGlobs {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				return nil, err
			}
			if info.IsDir() || seen[match] {
				continue
			}
			seen[match] = true
			names = append(names, match)
		}
	}
	sort.Strings(names)
	return names, nil
}

// addFile copies srcPath into the zip under nameInZip. A non-zero mode is
// force-set on the entry (mirroring GoReleaser, which forces 0755 on binaries);
// a zero mode preserves srcPath's on-disk mode, as GoReleaser does for its
// default bundled files.
func addFile(zw *zip.Writer, srcPath, nameInZip string, mode os.FileMode) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	hdr.Name = nameInZip
	hdr.Method = zip.Deflate
	if mode != 0 {
		hdr.SetMode(mode)
	}

	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}
