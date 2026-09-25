package build

// Source-sync selection (L14). A server build uploads the build context
// over rsync. Until L14 that upload was "the whole directory minus
// DefaultIgnore and .teployignore", so gitignored local-only files rode
// along — live on 2026-09-24 that put teploy.home.yml (an admin password in
// plaintext) and other teploy.*.yml overlays into world-readable build
// directories on the host. The rule now:
//
//	transferred = (git-visible ∪ allowlisted) − protected − excluded
//
//   - git-visible: tracked files plus untracked files git does not ignore
//     (`git ls-files --cached --others --exclude-standard`), so .gitignore,
//     .git/info/exclude and the global excludes file all apply. Outside a
//     git work tree every entry is visible — there is no .gitignore to
//     consult — and Source.GitAware says so for the caller to report.
//   - allowlisted: `!pattern` lines in .teployignore re-include what
//     .gitignore hides — the explicit door for build outputs a Dockerfile
//     COPYs (a locally built dist/). Never implicit.
//   - protected: DefaultIgnore. Never transferred; `!` cannot re-include.
//   - excluded: the other .teployignore lines. They beat the allowlist, so
//     `!/web/dist/` plus `/web/dist/**/*.map` ships dist without maps.
//
// The resolved entry list is handed to rsync (--files-from) AND to the
// provenance fingerprint, so the recorded identity is exactly what the
// build host received.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Source is the resolved set of entries a sync of Root transfers.
type Source struct {
	Root string
	// Entries are slash-separated paths relative to Root, sorted: files,
	// symlinks and — outside git, where empty directories are part of the
	// tree — directories. Inside git, directories are implied by the files.
	Entries []string
	// GitAware is true when .gitignore was applied (Root is in a git work
	// tree). False means every non-protected, non-excluded entry is sent.
	GitAware bool
	Rules    *Rules
}

// ResolveSource computes what a source sync of dir transfers.
func ResolveSource(dir string) (*Source, error) {
	if dir == "" {
		dir = "."
	}
	rules, err := LoadRules(dir)
	if err != nil {
		return nil, err
	}
	root := filepath.Clean(dir)
	visible, gitAware, err := listVisible(root, "", rules)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(visible))
	for _, rel := range visible {
		set[rel] = true
	}
	if gitAware && len(rules.includes) > 0 {
		if err := addAllowlisted(root, rules, set); err != nil {
			return nil, err
		}
	}
	entries := make([]string, 0, len(set))
	for rel := range set {
		entries = append(entries, rel)
	}
	sort.Strings(entries)
	return &Source{Root: root, Entries: entries, GitAware: gitAware, Rules: rules}, nil
}

// listVisible lists the admitted entries of root/prefix: through git when
// it is a work tree, by walking otherwise. prefix is the path of this
// directory relative to the source root ("" for the root itself).
func listVisible(root, prefix string, rules *Rules) ([]string, bool, error) {
	dir := filepath.Join(root, filepath.FromSlash(prefix))
	inGit, err := isGitWorkTree(dir)
	if err != nil {
		return nil, false, err
	}
	if !inGit {
		out, err := walkAdmitted(root, prefix, rules, nil)
		return out, false, err
	}
	raw, err := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, false, fmt.Errorf("listing files with git in %s: %s", dir, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, false, fmt.Errorf("listing files with git in %s: %w", dir, err)
	}
	var out []string
	for _, name := range strings.Split(string(raw), "\x00") {
		if name == "" {
			continue
		}
		name = strings.TrimSuffix(name, "/")
		rel := name
		if prefix != "" {
			rel = prefix + "/" + name
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			// Tracked but deleted from the work tree: nothing to send.
			continue
		}
		if info.IsDir() {
			// A submodule (gitlink) or a nested repository git reports as a
			// single entry: list it by its own rules, as the old walk did.
			if rules.IsExcluded(rel, true) {
				continue
			}
			var sub []string
			var subErr error
			if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(rel), ".git")); statErr == nil {
				sub, _, subErr = listVisible(root, rel, rules)
			} else {
				// No repository of its own (an uninitialized submodule):
				// nothing git could list, so walk what is there.
				sub, subErr = walkAdmitted(root, rel, rules, nil)
			}
			if err := subErr; err != nil {
				return nil, false, err
			}
			out = append(out, sub...)
			continue
		}
		if rules.IsExcluded(rel, false) {
			continue
		}
		out = append(out, rel)
	}
	return out, true, nil
}

// isGitWorkTree reports whether dir is inside a git work tree. Without git
// on PATH, a work tree is detected by a .git entry in dir or an ancestor
// and refused: .gitignore cannot be honored, and silently uploading what
// it hides is the leak L14 closed.
func isGitWorkTree(dir string) (bool, error) {
	if _, err := exec.LookPath("git"); err != nil {
		abs, absErr := filepath.Abs(dir)
		if absErr != nil {
			return false, absErr
		}
		for d := abs; ; d = filepath.Dir(d) {
			if _, statErr := os.Lstat(filepath.Join(d, ".git")); statErr == nil {
				return false, fmt.Errorf("%s is in a git work tree but git is not on PATH — install git so the source sync can honor .gitignore", dir)
			}
			if filepath.Dir(d) == d {
				return false, nil
			}
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// walkAdmitted walks root/prefix and returns every entry the rules admit
// (pruning protected/excluded directories). keep, when non-nil, further
// filters entries (the allowlist walk).
func walkAdmitted(root, prefix string, rules *Rules, keep func(rel string, isDir bool) bool) ([]string, error) {
	start := filepath.Join(root, filepath.FromSlash(prefix))
	var out []string
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == start && errors.Is(walkErr, fs.ErrNotExist) {
				return fs.SkipDir
			}
			return walkErr
		}
		if p == root {
			return nil
		}
		relOS, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relOS)
		isDir := d.IsDir()
		if rules.IsExcluded(rel, isDir) {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		if keep != nil && !keep(rel, isDir) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	return out, err
}

// addAllowlisted adds the files and symlinks `!` lines re-include. Anchored
// literal paths are walked directly; any glob forces a full (pruned) walk.
func addAllowlisted(root string, rules *Rules, set map[string]bool) error {
	keep := func(rel string, isDir bool) bool { return !isDir && rules.IsAllowlisted(rel, false) }
	starts := []string{""}
	allLiteral := true
	var literals []string
	for _, r := range rules.includes {
		if r.literal == "" {
			allLiteral = false
			break
		}
		literals = append(literals, r.literal)
	}
	if allLiteral {
		starts = literals
	}
	for _, s := range starts {
		// walkAdmitted judges every entry's ancestors too, so a literal
		// under a protected or excluded directory still yields nothing.
		found, err := walkAdmitted(root, s, rules, keep)
		if err != nil {
			return fmt.Errorf("resolving .teployignore allowlist: %w", err)
		}
		for _, rel := range found {
			set[rel] = true
		}
	}
	return nil
}

// Contains reports whether rel is transferred, or — for a directory —
// whether anything beneath it is.
func (s *Source) Contains(rel string) bool {
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return len(s.Entries) > 0
	}
	i := sort.SearchStrings(s.Entries, rel)
	if i < len(s.Entries) && s.Entries[i] == rel {
		return true
	}
	// Entries are sorted, so the first path under rel+"/" (if any) sorts
	// at or after rel; scan forward past siblings that merely share the
	// prefix ("dist-old" sorts between "dist" and "dist/").
	for ; i < len(s.Entries); i++ {
		e := s.Entries[i]
		if strings.HasPrefix(e, rel+"/") {
			return true
		}
		if !strings.HasPrefix(e, rel) {
			return false
		}
	}
	return false
}

// FileList renders Entries as an rsync --from0 --files-from list.
func (s *Source) FileList() []byte {
	var b bytes.Buffer
	for _, e := range s.Entries {
		b.WriteString(e)
		b.WriteByte(0)
	}
	return b.Bytes()
}

// Fingerprint returns the sha256 fingerprint of the entries under the
// build-context subdirectory sub ("" or "." for the root): every entry's
// path relative to sub, every file's content, every symlink's target and
// every directory (listed, or implied by an entry beneath it), in typed,
// length-prefixed records emitted in sorted order — so no two distinct
// trees collide through framing ambiguity (the TCL-38 discipline).
// Permission bits are ignored (umask stability across machines). The
// fingerprint describes what the build host RECEIVED — a provenance
// identity, not a security boundary.
func (s *Source) Fingerprint(sub string) (string, error) {
	sub = strings.Trim(path.Clean("/"+filepath.ToSlash(sub)), "/")
	type entry struct {
		rel    string
		kind   byte // 'f' file, 'd' directory, 'l' symlink, 's' special
		size   int64
		digest [32]byte
		target string
	}
	byRel := map[string]entry{}
	addDirs := func(rel string) {
		for i := 0; i < len(rel); i++ {
			if rel[i] == '/' {
				d := rel[:i]
				if _, ok := byRel[d]; !ok {
					byRel[d] = entry{rel: d, kind: 'd'}
				}
			}
		}
	}
	for _, full := range s.Entries {
		rel := full
		if sub != "" {
			var ok bool
			if rel, ok = strings.CutPrefix(full, sub+"/"); !ok {
				continue
			}
		}
		p := filepath.Join(s.Root, filepath.FromSlash(full))
		info, err := os.Lstat(p)
		if err != nil {
			return "", fmt.Errorf("fingerprinting build context: %w", err)
		}
		addDirs(rel)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return "", err
			}
			byRel[rel] = entry{rel: rel, kind: 'l', target: target}
		case info.IsDir():
			byRel[rel] = entry{rel: rel, kind: 'd'}
		case info.Mode().IsRegular():
			digest, size, err := hashFile(p)
			if err != nil {
				return "", err
			}
			byRel[rel] = entry{rel: rel, kind: 'f', size: size, digest: digest}
		default:
			// Sockets, devices and FIFOs cannot be synced as build input;
			// record their presence so the fingerprint still moves.
			byRel[rel] = entry{rel: rel, kind: 's'}
		}
	}
	entries := make([]entry, 0, len(byRel))
	for _, e := range byRel {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	h := sha256.New()
	// v2: the entry set is the L14 selection (gitignore-aware), not v1's
	// walk-minus-excludes — a new version so the two are never compared.
	h.Write([]byte("teploy-context-v2\x00"))
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

// CheckDockerfile fails fast when the Dockerfile — or a local path it
// COPYs/ADDs — exists on this machine but would not reach the build host:
// gitignored without a `!` allowlist line, excluded, or protected. The
// remote build would fail on it anyway ("COPY failed: not found"); this
// names the cause and the fix before anything is uploaded. Sources with
// globs, variables, URLs, heredocs or --from stages are not judged.
func (s *Source) CheckDockerfile(contextSub, dockerfile string) error {
	sub := strings.Trim(path.Clean("/"+filepath.ToSlash(contextSub)), "/")
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dfRel := strings.Trim(path.Clean("/"+path.Join(sub, filepath.ToSlash(dockerfile))), "/")
	data, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(dfRel)))
	if err != nil {
		return nil // DetectAt already judged presence; nothing to parse
	}
	if !s.Contains(dfRel) {
		return s.notUploaded(dfRel, "the Dockerfile")
	}
	for _, src := range copySources(data) {
		rel := strings.Trim(path.Clean("/"+path.Join(sub, src)), "/")
		if rel == "" || rel == sub {
			continue
		}
		if _, err := os.Lstat(filepath.Join(s.Root, filepath.FromSlash(rel))); err != nil {
			continue // absent locally: the build's own error is the right one
		}
		if !s.Contains(rel) {
			return s.notUploaded(rel, "the Dockerfile's COPY/ADD source")
		}
	}
	return nil
}

func (s *Source) notUploaded(rel, what string) error {
	info, _ := os.Lstat(filepath.Join(s.Root, filepath.FromSlash(rel)))
	isDir := info != nil && info.IsDir()
	if s.Rules.IsProtected(rel, isDir) {
		return fmt.Errorf("%s %s is protected (env files, teploy config, secrets stores) and is never uploaded to the build host — the image must not depend on it", what, rel)
	}
	if s.Rules.IsExcluded(rel, isDir) {
		return fmt.Errorf("%s %s is excluded by .teployignore, so the remote build cannot see it — remove the exclude", what, rel)
	}
	suffix := ""
	if isDir {
		suffix = "/"
	}
	return fmt.Errorf("%s %s is gitignored, so it is not uploaded to the build host — if the image genuinely needs it (a locally built artifact), allowlist it with a line `!/%s%s` in .teployignore", what, rel, rel, suffix)
}

// copySources extracts the local source paths of COPY/ADD instructions.
func copySources(dockerfile []byte) []string {
	var instructions []string
	var cur strings.Builder
	for _, line := range strings.Split(string(dockerfile), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, `\`) {
			cur.WriteString(strings.TrimSuffix(trimmed, `\`) + " ")
			continue
		}
		cur.WriteString(trimmed)
		if s := strings.TrimSpace(cur.String()); s != "" {
			instructions = append(instructions, s)
		}
		cur.Reset()
	}
	var out []string
	for _, ins := range instructions {
		fields := strings.Fields(ins)
		if len(fields) < 3 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "COPY", "ADD":
		default:
			continue
		}
		args := fields[1:]
		staged := false
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			if strings.HasPrefix(args[0], "--from") {
				staged = true
			}
			args = args[1:]
		}
		if staged || len(args) == 0 {
			continue
		}
		if rest := strings.Join(args, " "); strings.HasPrefix(rest, "[") {
			var arr []string
			if json.Unmarshal([]byte(rest), &arr) != nil {
				continue
			}
			args = arr
		}
		if len(args) < 2 {
			continue
		}
		for _, src := range args[:len(args)-1] {
			if strings.ContainsAny(src, "*?[$") || strings.Contains(src, "://") ||
				strings.HasPrefix(src, "<<") || strings.HasPrefix(src, "git@") {
				continue
			}
			out = append(out, src)
		}
	}
	return out
}
