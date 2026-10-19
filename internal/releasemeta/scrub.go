package releasemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// ScrubOptions selects what `teploy meta scrub` covers on one host.
type ScrubOptions struct {
	App   string // empty = every app under /deployments
	Apply bool   // false = dry run: read and report only
}

// ScrubRecord reports one release record that carries (or carried) secret
// env values. Keys are names only — a scrub report never holds a value.
type ScrubRecord struct {
	App          string   `json:"app"`
	Path         string   `json:"path"`
	RedactedKeys []string `json:"redacted_keys"`
	Rewritten    bool     `json:"rewritten"`
}

// ScrubReport is the outcome of one scrub run.
type ScrubReport struct {
	Apply           bool          `json:"apply"`
	RecordsScanned  int           `json:"records_scanned"`
	Records         []ScrubRecord `json:"records"`
	AttemptEnvFiles []string      `json:"attempt_env_files"`
	AttemptsRemoved int           `json:"attempt_env_files_removed"`
	Skipped         []string      `json:"skipped"`
}

// attemptEnvMinAgeMinutes keeps a scrub away from a deploy in flight: an
// attempt env file is consumed within the deploy that wrote it, so one that
// is older than this has no remaining reader.
const attemptEnvMinAgeMinutes = 30

// Scrub rewrites every release record on the host into the redacted shape
// and removes leftover attempt env files (plaintext copies of resolved
// secrets). Dry run by default. With Apply, each app is handled under its
// deploy lock; an app whose lock is held is skipped and reported, never
// waited on or broken. Idempotent: a second run finds nothing to change.
func Scrub(ctx context.Context, exec ssh.Executor, dk *docker.Client, opts ScrubOptions) (*ScrubReport, error) {
	report := &ScrubReport{Apply: opts.Apply, Records: []ScrubRecord{}, AttemptEnvFiles: []string{}, Skipped: []string{}}
	pattern := deploymentsDir + "/*/meta/*.json"
	attPattern := deploymentsDir + "/*/meta/att/*/env"
	if opts.App != "" {
		if err := config.ValidateName(opts.App); err != nil {
			return nil, err
		}
		pattern = fmt.Sprintf("%s/%s/meta/*.json", deploymentsDir, opts.App)
		attPattern = fmt.Sprintf("%s/%s/meta/att/*/env", deploymentsDir, opts.App)
	}
	files, err := listFiles(ctx, exec, pattern, 0)
	if err != nil {
		return nil, fmt.Errorf("listing release records: %w", err)
	}
	attFiles, err := listFiles(ctx, exec, attPattern, attemptEnvMinAgeMinutes)
	if err != nil {
		return nil, fmt.Errorf("listing attempt env files: %w", err)
	}

	byApp := map[string][]string{}
	for _, f := range files {
		byApp[appOf(f)] = append(byApp[appOf(f)], f)
	}
	attByApp := map[string][]string{}
	for _, f := range attFiles {
		attByApp[appOf(f)] = append(attByApp[appOf(f)], f)
	}
	apps := map[string]bool{}
	for a := range byApp {
		apps[a] = true
	}
	for a := range attByApp {
		apps[a] = true
	}
	names := make([]string, 0, len(apps))
	for a := range apps {
		names = append(names, a)
	}
	sort.Strings(names)

	imageEnvCache := map[string]map[string]string{}
	for _, app := range names {
		var lk *state.Lock
		if opts.Apply {
			lk, err = state.AcquireLockFenced(ctx, exec, app)
			if err != nil {
				report.Skipped = append(report.Skipped, fmt.Sprintf("%s: deploy lock unavailable (%v) — rerun when it is free", app, err))
				continue
			}
		}
		scrubApp(ctx, exec, dk, app, byApp[app], attByApp[app], opts.Apply, imageEnvCache, report)
		if lk != nil {
			state.ReleaseLockFenced(exec, lk, app)
		}
	}
	return report, nil
}

func scrubApp(ctx context.Context, exec ssh.Executor, dk *docker.Client, app string, records, attFiles []string, apply bool, cache map[string]map[string]string, report *ScrubReport) {
	for _, path := range records {
		report.RecordsScanned++
		data, present, err := state.ReadRemoteFile(ctx, exec, path)
		if err != nil || !present {
			report.Skipped = append(report.Skipped, path+": unreadable")
			continue
		}
		imageEnv := imageEnvFor(ctx, dk, data, cache)
		clean, keys, changed, err := RedactRaw(data, imageEnv)
		if err != nil {
			// The parse error text is not echoed: it can quote file bytes.
			report.Skipped = append(report.Skipped, path+": not a valid release record")
			continue
		}
		if !changed {
			continue
		}
		entry := ScrubRecord{App: app, Path: path, RedactedKeys: keys}
		if apply {
			if err := ssh.UploadAtomic(ctx, exec, strings.NewReader(string(clean)), path, "0600"); err != nil {
				report.Skipped = append(report.Skipped, path+": rewrite failed: "+err.Error())
			} else {
				entry.Rewritten = true
			}
		}
		report.Records = append(report.Records, entry)
	}
	for _, f := range attFiles {
		report.AttemptEnvFiles = append(report.AttemptEnvFiles, f)
		if !apply {
			continue
		}
		if _, err := exec.Run(ctx, "rm -f -- "+ssh.ShellQuote(f)); err != nil {
			report.Skipped = append(report.Skipped, f+": remove failed: "+err.Error())
			continue
		}
		report.AttemptsRemoved++
	}
}

// imageEnvFor returns the record's image ENV defaults (cached per image), or
// nil when the image is gone — nil means "redact more", never less.
func imageEnvFor(ctx context.Context, dk *docker.Client, data []byte, cache map[string]map[string]string) map[string]string {
	if dk == nil {
		return nil
	}
	var rec struct {
		Recreate *struct {
			ImageID  string `json:"image_id"`
			ImageRef string `json:"image_ref"`
		} `json:"recreate"`
		ImageDigest string `json:"image_digest"`
		ImageRef    string `json:"image_ref"`
	}
	if json.Unmarshal(data, &rec) != nil {
		return nil
	}
	image := rec.ImageDigest
	if rec.Recreate != nil && rec.Recreate.ImageID != "" {
		image = rec.Recreate.ImageID
	}
	if image == "" || !strings.HasPrefix(image, "sha256:") {
		return nil // a mutable tag may now name a different image
	}
	if env, ok := cache[image]; ok {
		return env
	}
	env, err := dk.ImageEnv(ctx, image)
	if err != nil {
		env = nil
	}
	cache[image] = env
	return env
}

// listFiles expands a /deployments glob on the host (optionally only files
// older than minAgeMinutes) and returns the matching regular files, sorted.
func listFiles(ctx context.Context, exec ssh.Executor, pattern string, minAgeMinutes int) ([]string, error) {
	age := ""
	if minAgeMinutes > 0 {
		age = fmt.Sprintf(" -mmin +%d", minAgeMinutes)
	}
	// The glob is expanded by the remote shell from a fixed, validated
	// pattern; find filters to regular files.
	out, err := exec.Run(ctx, fmt.Sprintf("for f in %s; do [ -f \"$f\" ] && find \"$f\" -maxdepth 0 -type f%s; done 2>/dev/null; true", pattern, age))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	return files, nil
}

func appOf(path string) string {
	rest := strings.TrimPrefix(path, deploymentsDir+"/")
	app, _, _ := strings.Cut(rest, "/")
	return app
}
