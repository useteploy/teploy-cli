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
	if strings.HasPrefix(cmd, "mv -f -- ") || strings.HasPrefix(cmd, "rm -f -- ") {
		m.applyFileCommand(cmd)
		m.mu.Unlock()
		return "", nil
	}
	m.mu.Unlock()
	return "", fmt.Errorf("mock: unexpected command: %s", cmd)
}

// evalFenceGuard recognizes the guard fragment produced by state.Lock. It
// returns the remaining effect command ("" for a bare guard), whether the
// guard holds against the recorded files, and whether cmd was a guard at
// all. Must be called with m.mu held.
func evalFenceGuard(files map[string][]byte, cmd string) (rest string, held, ok bool) {
	const guardSep = " || { printf 'TEPLOY_FENCE_LOST\\n' >&2; exit 75; }; "
	if !strings.HasPrefix(cmd, "grep -q ") {
		return "", false, false
	}
	guard, effect := cmd, ""
	if i := strings.Index(cmd, guardSep); i >= 0 {
		guard, effect = cmd[:i], cmd[i+len(guardSep):]
	}
	owner, path, parsed := parseFenceGuard(guard)
	if !parsed {
		return "", false, false
	}
	data, present := files[path]
	held = present && bytes.Contains(data, []byte(owner))
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
	if len(fields) == 5 && fields[0] == "mv" && fields[1] == "-f" && fields[2] == "--" {
		if data, ok := m.Files[fields[3]]; ok {
			m.Files[fields[4]] = data
			delete(m.Files, fields[3])
		}
	}
	if len(fields) == 4 && fields[0] == "rm" && fields[1] == "-f" && fields[2] == "--" {
		delete(m.Files, fields[3])
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
	_, err := io.Copy(io.Discard, stdin)
	if err != nil {
		return err
	}
	_, err = m.Run(ctx, cmd)
	return err
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
