package ssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Compile-time check: MockExecutor implements Executor.
var _ Executor = (*MockExecutor)(nil)

// MockExecutor implements Executor for testing.
// Commands are matched against registered responses.
type MockExecutor struct {
	host     string
	commands []MockCommand

	mu    sync.Mutex
	Calls []string          // records every command executed
	Files map[string][]byte // records uploaded file contents by path
	// Inputs records the stdin payload of every RunInput invocation in
	// call order, so tests can assert secret material traveled by stdin
	// and NOT in the command string (the C08 secret-transport pins).
	Inputs []string

	// GuardTransportFailures, when > 0, makes the next that-many GUARDED
	// commands (the fence-guard shape) fail with a plain transport error
	// instead of being evaluated against Files — modeling an SSH channel
	// dying mid-command, the ambiguous-release case of audit T02.
	GuardTransportFailures int

	// GenerationLabels models the docker generation labels (C01-8/9) for
	// the composed generation-check fragment docker emits: container
	// name → decimal generation. Absent names read as 0 (legacy/unlabeled
	// — the compat rule), so tests that don't care keep working.
	GenerationLabels map[string]string
}

// MockCommand maps a command prefix to a response.
type MockCommand struct {
	Match  string // prefix to match against
	Output string // stdout to return
	Err    error  // error to return
	Once   bool   // if true, remove after first match
}

// NewMockExecutor creates a mock executor for the given host.
func NewMockExecutor(host string, commands ...MockCommand) *MockExecutor {
	return &MockExecutor{
		host:     host,
		commands: commands,
		Files:    make(map[string][]byte),
	}
}

func (m *MockExecutor) Run(ctx context.Context, cmd string) (string, error) {
	m.mu.Lock()
	m.Calls = append(m.Calls, cmd)

	// Fenced-lock guards (internal/state, audit F16) arrive either alone
	// (`grep -q '<owner>' '<info>'`) or composed with the effect they gate
	// (`grep ... || { printf 'TEPLOY_FENCE_LOST\n' >&2; exit 75; }; <effect>`).
	// Evaluating them against the recorded file state models the server:
	// the guard passes while the uploaded lock info still names the owner
	// and refuses once it does not, which is what the fence tests need to
	// prove a refused effect never executes.
	if rest, held, ok := evalFenceGuard(m.Files, cmd); ok {
		if m.GuardTransportFailures > 0 {
			m.GuardTransportFailures--
			m.mu.Unlock()
			return "", fmt.Errorf("ssh: connection timed out")
		}
		if !held {
			m.mu.Unlock()
			return "", fmt.Errorf("exit status 75: TEPLOY_FENCE_LOST")
		}
		if rest == "" {
			m.mu.Unlock()
			return "", nil
		}
		cmd = rest
		m.Calls = append(m.Calls, cmd)
	}

	// The committed-generation CAS prefix (internal/state,
	// GenerationCASPrefix — C01-8/9): evaluate the sidecar read exactly as
	// the server shell would, so a stale plan's commit is refused against
	// the recorded file state and a current one proceeds.
	if rest, refused, found, expected, ok := evalGenerationCAS(m.Files, cmd); ok {
		if refused {
			m.mu.Unlock()
			if found == 0 && expected == 0 {
				return "", fmt.Errorf("exit status 74: TEPLOY_GENERATION_BADGEN")
			}
			return "", fmt.Errorf("exit status 74: TEPLOY_GENERATION_FENCED %d %d", found, expected)
		}
		cmd = rest
		m.Calls = append(m.Calls, cmd)
	}

	// docker's composed generation label check (internal/docker,
	// generationCheckFragment — C01-8/9): evaluate the container's
	// generation label from GenerationLabels against the expected bound.
	if rest, refused, found, expected, ok := m.evalGenerationLabelCheck(cmd); ok {
		if refused {
			m.mu.Unlock()
			return "", fmt.Errorf("exit status 74: TEPLOY_GENERATION_FENCED %d %d", found, expected)
		}
		cmd = rest
		m.Calls = append(m.Calls, cmd)
	}

	// The exact-block route CAS on Caddyfile commits (internal/caddy,
	// routeCASFragment — A12/T05): hash the app's managed region from the
	// recorded Caddyfile with caddy's normalization and compare against
	// the acceptable hashes embedded in the command.
	if rest, refused, foundGen, expectedGen, ok := evalRouteCAS(m.Files, cmd); ok {
		if refused {
			m.mu.Unlock()
			return "", fmt.Errorf("exit status 76: TEPLOY_ROUTE_CAS_MISMATCH found_generation=%d expected_generation=%d", foundGen, expectedGen)
		}
		cmd = rest
		m.Calls = append(m.Calls, cmd)
	}

	// `cat <path>` answers from the recorded file state when the mock has
	// one (the real server re-reads whatever earlier writes left); an
	// explicit registration still wins for paths the mock has no file for.
	if rest, ok := strings.CutPrefix(cmd, "cat "); ok && !strings.Contains(rest, " | ") {
		path := strings.Trim(strings.TrimSpace(rest), "'")
		if data, present := m.Files[path]; present {
			m.mu.Unlock()
			return string(data), nil
		}
	}
	for i, c := range m.commands {
		if mockCommandMatches(cmd, c.Match) {
			if c.Once {
				m.commands = append(m.commands[:i], m.commands[i+1:]...)
			}
			if c.Err == nil {
				m.applyFileCommand(cmd)
				// A compound file op (`mv a b && mv c d`, C01-8/9's
				// state+sidecar commit) matched a broad registration;
				// apply each segment so the recorded file state still
				// reflects what the server shell did.
				if isFileOpCommand(cmd) {
					for _, seg := range strings.Split(cmd, " && ") {
						m.applyFileCommand(seg)
					}
				}
			}
			m.mu.Unlock()
			return c.Output, c.Err
		}
	}
	if isFileOpCommand(cmd) {
		for _, seg := range strings.Split(cmd, " && ") {
			if isFileOpCommand(seg) {
				m.applyFileCommand(seg)
			}
		}
		m.mu.Unlock()
		return "", nil
	}
	// The conditional lock release (internal/state, audit T02): remove the
	// lock directory only when its info still names the releasing owner.
	// Modeled against the recorded file state like evalFenceGuard.
	if dir, owner, ok := parseConditionalLockRelease(cmd); ok {
		info := dir + "/info"
		if data, present := m.Files[info]; present && bytes.Contains(data, []byte(owner)) {
			m.applyFileCommand("rm -rf -- " + dir)
		}
		m.mu.Unlock()
		return "", nil
	}
	// The server-side adapt gate (internal/caddy, F48/F49) streams the
	// proposed Caddyfile over stdin; the mock cannot run a real caddy, so
	// it models "the server's caddy accepted it" — tests that need the
	// refusal register an explicit Err command for the same prefix, which
	// wins because matching above takes precedence.
	if strings.HasPrefix(cmd, "docker exec -i caddy caddy adapt") {
		m.mu.Unlock()
		return "", nil
	}
	// Framed server-file reads (state.ReadRemoteFile, caddy's webhook
	// descriptor): answer from the recorded file state when no explicit
	// registration matches, so tests exercising route/state edits do not
	// need to stub every read individually.
	if rest, ok := strings.CutPrefix(cmd, "if [ ! -e "); ok && strings.Contains(cmd, "printf 'absent\\n'") {
		if pathQ, _, found := strings.Cut(rest, " ]; then"); found {
			path := strings.Trim(pathQ, "'")
			if data, present := m.Files[path]; present {
				m.mu.Unlock()
				return "present\n" + string(data), nil
			}
			m.mu.Unlock()
			return "absent", nil
		}
	}
	m.mu.Unlock()
	return "", fmt.Errorf("mock: unexpected command: %s", cmd)
}

// evalFenceGuard recognizes guard fragments produced by state.Lock and
// the caddy lock (C01-3) — possibly CHAINED (an app-fence guard followed
// by the caddy-lock guard on one commit command, C01-2/C01-3
// composition). It returns the remaining effect command ("" for a bare
// guard), whether EVERY guard holds against the recorded files, and
// whether cmd carried at least one guard at all. Must be called with m.mu
// held.
func evalFenceGuard(files map[string][]byte, cmd string) (rest string, held, ok bool) {
	const guardSep = " || { printf 'TEPLOY_FENCE_LOST\\n' >&2; exit 75; }; "
	if !strings.HasPrefix(cmd, "grep -q ") {
		return "", false, false
	}
	effect := ""
	held = true
	ok = false
	for strings.HasPrefix(cmd, "grep -q ") {
		guard := cmd
		if i := strings.Index(cmd, guardSep); i >= 0 {
			guard, effect = cmd[:i], cmd[i+len(guardSep):]
		} else {
			effect = ""
		}
		owner, path, parsed := parseFenceGuard(guard)
		if !parsed {
			break
		}
		ok = true
		data, present := files[path]
		if !present || !bytes.Contains(data, []byte(owner)) {
			held = false
		}
		cmd = effect
	}
	if !ok {
		return "", false, false
	}
	return effect, held, true
}

// parseFenceGuard splits `grep -q '<owner>' '<path>'` into its two
// single-quoted arguments.
func parseFenceGuard(guard string) (owner, path string, ok bool) {
	s := strings.TrimPrefix(guard, "grep -q ")
	parts := strings.Split(s, " ")
	if len(parts) != 2 {
		return "", "", false
	}
	for _, p := range parts {
		if len(p) < 2 || p[0] != '\'' || p[len(p)-1] != '\'' {
			return "", "", false
		}
	}
	return parts[0][1 : len(parts[0])-1], parts[1][1 : len(parts[1])-1], true
}

// parseConditionalLockRelease recognizes the single-command conditional
// release emitted by state.ReleaseLockFenced's ambiguous-failure fallback:
// `if [ -d '<dir>' ] && grep -q '<owner>' '<dir>/info' 2>/dev/null; then rm -rf -- '<dir>'; fi`
func parseConditionalLockRelease(cmd string) (dir, owner string, ok bool) {
	unquote := func(s string) (string, bool) {
		if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
			return s[1 : len(s)-1], true
		}
		return "", false
	}
	rest, found := strings.CutPrefix(cmd, "if [ -d ")
	if !found {
		return "", "", false
	}
	dirField, rest, found := strings.Cut(rest, " ] && grep -q ")
	if !found {
		return "", "", false
	}
	ownerField, rest, found := strings.Cut(rest, " ")
	if !found {
		return "", "", false
	}
	infoField, rest, found := strings.Cut(rest, " 2>/dev/null; then rm -rf -- ")
	if !found {
		return "", "", false
	}
	rmField, found := strings.CutSuffix(rest, "; fi")
	if !found {
		return "", "", false
	}
	dir, ok = unquote(dirField)
	if !ok {
		return "", "", false
	}
	owner, ok = unquote(ownerField)
	if !ok {
		return "", "", false
	}
	info, ok := unquote(infoField)
	if !ok || info != dir+"/info" {
		return "", "", false
	}
	rm, ok := unquote(rmField)
	if !ok || rm != dir {
		return "", "", false
	}
	return dir, owner, true
}

// isFileOpCommand reports whether cmd (or its first && -chained segment)
// is a plain file mutation the mock models against Files. Compound
// commands made ENTIRELY of such segments (state.WriteFenced's
// `mv state && mv sidecar`, C01-8/9) are applied segment by segment; a
// compound carrying anything else falls through to the registrations.
func isFileOpCommand(cmd string) bool {
	for _, seg := range strings.Split(cmd, " && ") {
		seg = strings.TrimSpace(seg)
		if !strings.HasPrefix(seg, "mv -f -- ") && !strings.HasPrefix(seg, "mv -fT -- ") &&
			!strings.HasPrefix(seg, "rm -f -- ") && !strings.HasPrefix(seg, "rm -rf -- ") {
			return false
		}
	}
	return true
}

// evalGenerationCAS recognizes the committed-generation CAS prefix emitted
// by state.GenerationCASPrefix:
//
//	if [ -f '<sidecar>' ]; then tg=$(cat '<sidecar>' 2>/dev/null); case
//	"$tg" in ''|*[!0-9]*) ... BADGEN ...;; esac; [ "$tg" -le <E> ] ||
//	{ ... TEPLOY_GENERATION_FENCED ...;; }; fi; <effect>
//
// It evaluates the sidecar against the recorded files: absent passes
// (generation 0 — the targetguard contract), a newer committed generation
// than expected refuses, and a present-but-non-numeric sidecar refuses
// fail-closed. Returns the remaining effect command, whether the CAS
// refused, the committed and expected generations (for the refusal error),
// and whether cmd carried the prefix at all.
func evalGenerationCAS(files map[string][]byte, cmd string) (rest string, refused bool, found, expected uint64, ok bool) {
	const casHead = "if [ -f '"
	if !strings.HasPrefix(cmd, casHead) {
		return "", false, 0, 0, false
	}
	endQuote := strings.Index(cmd[len(casHead):], "'")
	if endQuote < 0 {
		return "", false, 0, 0, false
	}
	path := cmd[len(casHead) : len(casHead)+endQuote]
	// The prefix ends at the first `fi; ` after the sidecar test.
	fiAt := strings.Index(cmd, "fi; ")
	if fiAt < 0 {
		return "", false, 0, 0, false
	}
	prefix := cmd[:fiAt]
	// Expected generation from the `[ "$tg" -le <E> ]` comparison.
	leAt := strings.Index(prefix, `[ "$tg" -le `)
	if leAt < 0 {
		return "", false, 0, 0, false
	}
	numRest := prefix[leAt+len(`[ "$tg" -le `):]
	numEnd := strings.Index(numRest, " ]")
	if numEnd < 0 {
		return "", false, 0, 0, false
	}
	exp, err := strconv.ParseUint(numRest[:numEnd], 10, 64)
	if err != nil {
		return "", false, 0, 0, false
	}
	data, present := files[path]
	if !present {
		return cmd[fiAt+len("fi; "):], false, 0, exp, true
	}
	committed, perr := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if perr != nil {
		// BADGEN: found==expected==0 disambiguates from a numeric refusal.
		return "", true, 0, 0, true
	}
	if committed > exp {
		return "", true, committed, exp, true
	}
	return cmd[fiAt+len("fi; "):], false, committed, exp, true
}

// evalGenerationLabelCheck recognizes docker's composed generation label
// check (internal/docker.generationCheckFragment):
//
//	g=$(docker inspect -f '{{index .Config.Labels "teploy.generation"}}'
//	 '<ref>' 2>/dev/null || printf '0'); case "$g" in ''|*[!0-9]*) g=0;;
//	esac; [ "$g" -le <E> ] || { ...TEPLOY_GENERATION_FENCED...; exit 74; };
//
// The label is answered from m.GenerationLabels (absent = 0, the legacy
// compat rule). Must be called with m.mu held.
func (m *MockExecutor) evalGenerationLabelCheck(cmd string) (rest string, refused bool, found, expected uint64, ok bool) {
	const head = `g=$(docker inspect -f '{{index .Config.Labels "teploy.generation"}}' '`
	if !strings.HasPrefix(cmd, head) {
		return "", false, 0, 0, false
	}
	restQuote := cmd[len(head):]
	endQuote := strings.Index(restQuote, "' 2>/dev/null")
	if endQuote < 0 {
		return "", false, 0, 0, false
	}
	ref := restQuote[:endQuote]
	leAt := strings.Index(cmd, `[ "$g" -le `)
	if leAt < 0 {
		return "", false, 0, 0, false
	}
	numRest := cmd[leAt+len(`[ "$g" -le `):]
	numEnd := strings.Index(numRest, " ]")
	if numEnd < 0 {
		return "", false, 0, 0, false
	}
	exp, err := strconv.ParseUint(numRest[:numEnd], 10, 64)
	if err != nil {
		return "", false, 0, 0, false
	}
	gen := uint64(0)
	if v, present := m.GenerationLabels[ref]; present {
		if parsed, perr := strconv.ParseUint(strings.TrimSpace(v), 10, 64); perr == nil {
			gen = parsed
		}
	}
	if gen > exp {
		return "", true, gen, exp, true
	}
	// Strip through the refusal block's closing `; }; ` — the fragment's
	// shape is `[ "$g" -le N ] || { ...; exit 74; }; <effect>`.
	tail := numRest[numEnd:]
	end := strings.Index(tail, "; }; ")
	if end < 0 {
		return "", false, 0, 0, false
	}
	return tail[end+len("; }; "):], false, gen, exp, true
}

// evalRouteCAS recognizes caddy's exact-block compare-and-swap fragment
// (internal/caddy.routeCASFragment — A12/T05):
//
//	cur=$(sed -n '/^# TEPLOY BEGIN <app>$/,/^# TEPLOY END <app>$/p'
//	 '<caddyfile>' 2>/dev/null | sha256sum | cut -d' ' -f1);
//	case "$cur" in <h1>|<h2>) ;; *) ...TEPLOY_ROUTE_CAS_MISMATCH...;; esac;
//
// It extracts the app's marker-inclusive region from the recorded
// Caddyfile, hashes it with caddy's normalization (region + "\n", empty
// input for an absent region) and compares against the acceptable hashes.
// foundGen reports the live region's stamp (0 when unstamped) for the
// refusal error, expectedGen the fragment's expectation.
func evalRouteCAS(files map[string][]byte, cmd string) (rest string, refused bool, foundGen, expectedGen uint64, ok bool) {
	const head = `cur=$(sed -n '/^# TEPLOY BEGIN `
	if !strings.HasPrefix(cmd, head) {
		return "", false, 0, 0, false
	}
	// App name: between "BEGIN " and the range separator "$/,/^# TEPLOY END".
	beginIdx := strings.Index(cmd[len(head):], "$/,/^# TEPLOY END ")
	if beginIdx < 0 {
		return "", false, 0, 0, false
	}
	app := cmd[len(head) : len(head)+beginIdx]
	// The case list: between `case "$cur" in ` and `) ;; *)`.
	caseAt := strings.Index(cmd, `case "$cur" in `)
	if caseAt < 0 {
		return "", false, 0, 0, false
	}
	listRest := cmd[caseAt+len(`case "$cur" in `):]
	listEnd := strings.Index(listRest, ") ;; *)")
	if listEnd < 0 {
		return "", false, 0, 0, false
	}
	acceptable := strings.Split(listRest[:listEnd], "|")
	// The expectation rides the refusal printf: expected_generation=<E>.
	if at := strings.Index(cmd, "expected_generation="); at >= 0 {
		num := cmd[at+len("expected_generation="):]
		end := strings.IndexAny(num, " \n")
		if end < 0 {
			end = len(num)
		}
		if v, err := strconv.ParseUint(num[:end], 10, 64); err == nil {
			expectedGen = v
		}
	}
	// The fragment ends at `esac; `.
	esacAt := strings.Index(cmd, "esac; ")
	if esacAt < 0 {
		return "", false, 0, 0, false
	}
	// Region extraction, mirroring the server sed: marker-inclusive lines.
	fileData, present := files["/deployments/caddy/Caddyfile"]
	if !present {
		fileData = nil
	}
	regionText := extractMockManagedRegion(string(fileData), app)
	var payload []byte
	if regionText != "" {
		payload = append([]byte(regionText), '\n')
	}
	sum := sha256.Sum256(payload)
	actual := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(regionText, "\n") {
		if strings.HasPrefix(line, "# TEPLOY GENERATION ") {
			if v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "# TEPLOY GENERATION ")), 10, 64); err == nil {
				foundGen = v
			}
			break
		}
	}
	for _, h := range acceptable {
		if h == actual {
			return cmd[esacAt+len("esac; "):], false, foundGen, expectedGen, true
		}
	}
	return "", true, foundGen, expectedGen, true
}

// extractMockManagedRegion is the mock's mirror of caddy's
// extractManagedRegion (marker-inclusive region text, "" when absent) —
// duplicated rather than imported so the ssh package keeps no caddy
// dependency; the two MUST stay byte-compatible (exact-line marker
// matching, lines joined with \n, no trailing newline).
func extractMockManagedRegion(content, app string) string {
	lines := strings.Split(content, "\n")
	var out []string
	active := false
	for _, line := range lines {
		if !active {
			if line == "# TEPLOY BEGIN "+app {
				active = true
				out = append(out, line)
			}
			continue
		}
		out = append(out, line)
		if line == "# TEPLOY END "+app {
			return strings.Join(out, "\n")
		}
	}
	return ""
}

func mockCommandMatches(cmd, match string) bool {
	if !strings.HasPrefix(cmd, match) {
		return false
	}
	// A path match for ".../state" must not also consume ".../state.json".
	// Prefix matching remains available for command arguments and shell suffixes.
	return len(cmd) == len(match) || cmd[len(match)] != '.'
}

func (m *MockExecutor) applyFileCommand(cmd string) {
	fields := strings.Fields(cmd)
	for i := range fields {
		fields[i] = strings.Trim(fields[i], "'")
	}
	if len(fields) == 5 && fields[0] == "mv" && (fields[1] == "-f" || fields[1] == "-fT") && fields[2] == "--" {
		if data, ok := m.Files[fields[3]]; ok {
			m.Files[fields[4]] = data
			delete(m.Files, fields[3])
		}
	}
	if len(fields) == 4 && fields[0] == "rm" && fields[1] == "-f" && fields[2] == "--" {
		delete(m.Files, fields[3])
	}
	// rm -rf -- <dir>: a recursive removal deletes the directory AND every
	// recorded file beneath it — the guarded lock release (state package,
	// audit A04) removes /deployments/<app>/.lock and its info together.
	if len(fields) == 4 && fields[0] == "rm" && fields[1] == "-rf" && fields[2] == "--" {
		prefix := strings.TrimSuffix(fields[3], "/") + "/"
		for p := range m.Files {
			if p == fields[3] || strings.HasPrefix(p, prefix) {
				delete(m.Files, p)
			}
		}
	}
}

func (m *MockExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	output, err := m.Run(ctx, cmd)
	if output != "" {
		fmt.Fprint(stdout, output)
	}
	return err
}

func (m *MockExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.Inputs = append(m.Inputs, string(data))
		m.mu.Unlock()
	}
	_, err := m.Run(ctx, cmd)
	return err
}

// runDetailed is MockExecutor's native structured capture: the
// registered Output/Err become Stdout/ExitCode exactly as the real
// executors report them (an error carrying "exit status N" is a command
// failure with that code; an error without one is a transport failure).
func (m *MockExecutor) runDetailed(ctx context.Context, cmd string, stdin io.Reader, limit int64) Result {
	if res, done := contextFailureResult(ctx); done {
		return res
	}
	var out string
	var err error
	if stdin != nil {
		// Record the payload (the secret-transport pins read Inputs),
		// then evaluate the command with the same output contract as the
		// non-stdin path — RunInput's error-only signature would lose
		// the output the structured caller exists to see.
		data, readErr := io.ReadAll(stdin)
		if readErr != nil {
			return Result{ExitCode: -1, Err: readErr}
		}
		m.mu.Lock()
		m.Inputs = append(m.Inputs, string(data))
		m.mu.Unlock()
		out, err = m.Run(ctx, cmd)
	} else {
		out, err = m.Run(ctx, cmd)
	}
	res := Result{ExitCode: -1}
	if limit > 0 && int64(len(out)) > limit {
		out = out[:limit]
		res.Truncated = true
	}
	res.Stdout = []byte(out)
	if err == nil {
		res.ExitCode = 0
		return res
	}
	if code, ok := exitCodeFromError(err); ok {
		res.ExitCode = code
		res.Stderr = []byte(err.Error())
		return res
	}
	res.Err = err
	return res
}

func (m *MockExecutor) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}

	m.mu.Lock()
	call := fmt.Sprintf("UPLOAD:%s (mode %s)", remotePath, mode)
	m.Calls = append(m.Calls, call)
	for i, c := range m.commands {
		if strings.HasPrefix(call, c.Match) {
			if c.Once {
				m.commands = append(m.commands[:i], m.commands[i+1:]...)
			}
			if c.Err != nil {
				m.mu.Unlock()
				return c.Err
			}
			break
		}
	}
	m.Files[remotePath] = data
	m.mu.Unlock()

	return nil
}

func (m *MockExecutor) Close() error {
	return nil
}

func (m *MockExecutor) Host() string {
	return m.host
}

func (m *MockExecutor) User() string {
	return "root"
}
