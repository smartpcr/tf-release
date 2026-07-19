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
// archive exactly: the versioned binary plus README.md (GoReleaser bundles
// README* by default), so build-hook and release archives are interchangeable.
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

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

// writeZip creates zipPath containing binPath (stored under its base name) plus
// README.md when present, matching the release pipeline's archive layout. The
// hook runs with the repo root as its working directory, so README.md resolves
// relative to it.
func writeZip(zipPath, binPath string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := zip.NewWriter(out)

	if err := addFile(zw, binPath, filepath.Base(binPath)); err != nil {
		return err
	}
	// GoReleaser's default archive bundles README*; mirror that so the build-hook
	// zip is byte-for-content identical to the release archive.
	if _, statErr := os.Stat("README.md"); statErr == nil {
		if err := addFile(zw, "README.md", "README.md"); err != nil {
			return err
		}
	}
	return zw.Close()
}

// addFile copies srcPath into the zip under nameInZip, preserving its file mode.
func addFile(zw *zip.Writer, srcPath, nameInZip string) error {
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

	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}

