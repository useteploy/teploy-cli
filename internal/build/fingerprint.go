package build

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// ContextFingerprint returns the sha256 fingerprint of the build-context
// tree dir would transfer: every file's path and content, every directory's
// path, and every symlink's target, with the given exclude patterns
// (rsync-style: matched against each entry's base name and its
// slash-separated relative path) left out — the fingerprint describes the
// SOURCE the builder consumes, not the operator's local clutter.
//
// Encoding discipline mirrors the static deployer's v3 tree hash (audit
// F51/TCL-38): typed, length-prefixed records for EVERY entry —
// directories included — emitted in sorted path order, so no two distinct
// trees can collide through framing ambiguity. Permission bits are ignored
// (umask stability across machines). Unlike the static hash, symlinks are
// INCLUDED, hashed by target: rsync -a preserves links into the build
// context, so a link is build input whose identity is what it points at.
// This is a provenance identity, not a security boundary.
func ContextFingerprint(dir string, excludes []string) (string, error) {
	if dir == "" {
		dir = "."
	}
	type entry struct {
		rel    string
		kind   byte // 'f' file, 'd' directory, 'l' symlink
		size   int64
		digest [32]byte
		target string
	}
	var entries []entry
	pruned := func(rel string) bool {
		if len(excludes) == 0 {
			return false
		}
		base := filepath.Base(rel)
		for _, pat := range excludes {
			if pat == "" {
				continue
			}
			if ok, _ := filepath.Match(pat, base); ok {
				return true
			}
			if ok, _ := filepath.Match(pat, rel); ok {
				return true
			}
		}
		return false
	}

	root := filepath.Clean(dir)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if pruned(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entries = append(entries, entry{rel: rel, kind: 'l', target: target})
		case info.IsDir():
			entries = append(entries, entry{rel: rel, kind: 'd'})
		case info.Mode().IsRegular():
			digest, size, err := hashFile(p)
			if err != nil {
				return err
			}
			entries = append(entries, entry{rel: rel, kind: 'f', size: size, digest: digest})
		default:
			// Sockets, devices and FIFOs cannot be synced as build input;
			// record their presence so the fingerprint still moves.
			entries = append(entries, entry{rel: rel, kind: 's'})
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprinting build context %s: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	h := sha256.New()
	h.Write([]byte("teploy-context-v1\x00"))
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], uint64(len(entries)))
	h.Write(num[:])
	var len8 [8]byte
	writeStr := func(s string) {
		binary.BigEndian.PutUint64(len8[:], uint64(len(s)))
		h.Write(len8[:])
		h.Write([]byte(s))
	}
	for _, e := range entries {
		h.Write([]byte{e.kind})
		writeStr(e.rel)
		switch e.kind {
		case 'f':
			binary.BigEndian.PutUint64(len8[:], uint64(e.size))
			h.Write(len8[:])
			h.Write(e.digest[:])
		case 'l':
			writeStr(e.target)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(path string) ([32]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return [32]byte{}, 0, err
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, size, nil
}

// EffectiveLocalPlatform returns the platform a LOCAL Dockerfile build
// targets for the given configured platform: an explicit platform wins;
// otherwise building on Apple silicon targets linux/amd64 (the deploy
// default since the first local-build support — an arm64 macOS host
// otherwise produces images the typical amd64 server cannot run); anywhere
// else the daemon's native default applies (empty). Shared by the build
// itself and the C04 provenance record so both name the same platform.
func EffectiveLocalPlatform(platform string) string {
	if platform != "" {
		return platform
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "linux/amd64"
	}
	return ""
}
