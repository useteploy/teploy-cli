package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
			}
			m.mu.Unlock()
			return c.Output, c.Err
		}
	}
	if strings.HasPrefix(cmd, "mv -f -- ") || strings.HasPrefix(cmd, "mv -fT -- ") ||
		strings.HasPrefix(cmd, "rm -f -- ") || strings.HasPrefix(cmd, "rm -rf -- ") {
		m.applyFileCommand(cmd)
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
		err = m.RunInput(ctx, cmd, stdin)
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
