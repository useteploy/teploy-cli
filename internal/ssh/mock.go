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
		if strings.HasPrefix(cmd, c.Match) {
			if c.Once {
				m.commands = append(m.commands[:i], m.commands[i+1:]...)
			}
			m.mu.Unlock()
			return c.Output, c.Err
		}
	}
	m.mu.Unlock()
	return "", fmt.Errorf("mock: unexpected command: %s", cmd)
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
	m.Calls = append(m.Calls, fmt.Sprintf("UPLOAD:%s (mode %s)", remotePath, mode))
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
