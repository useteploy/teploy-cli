package ssh

import (
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
