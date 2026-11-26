package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"

	gossh "golang.org/x/crypto/ssh"
)

// Forwarder is an Executor whose connection can also carry a forwarded TCP
// connection to an address on the server's side. RemoteExecutor is one.
type Forwarder interface {
	Executor

	// ForwardDial opens a TCP connection to addr (host:port) as the SERVER
	// sees it: a direct-tcpip channel on the SSH connection.
	ForwardDial(ctx context.Context, addr string) (net.Conn, error)
}

// Compile-time check: RemoteExecutor is a Forwarder.
var _ Forwarder = (*RemoteExecutor)(nil)

// ForwardDial opens a TCP connection to addr (host:port) as the server
// sees it: a direct-tcpip channel on this SSH connection. It is how a
// service bound to the server's loopback is reached without exposing it:
// the service listens on 127.0.0.1 and the only way in is this connection.
// The connection closes with the SSH connection.
//
// A refusal by the server comes back at once as an *gossh.OpenChannelError;
// ForwardRefused says which of the two usual kinds it is.
func (e *RemoteExecutor) ForwardDial(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := e.client.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("forwarding to %s: %w", addr, err)
	}
	return conn, nil
}

// ForwardRefused reports why a forwarded connection did not open. refused
// is true when nothing on the server listens at the address (the server's
// own connect failed); prohibited when the server's sshd does not allow
// the forward (AllowTcpForwarding no, or PermitOpen). Both are false for
// any other error: a dead connection, a canceled context.
func ForwardRefused(err error) (refused, prohibited bool) {
	var openErr *gossh.OpenChannelError
	if !errors.As(err, &openErr) {
		return false, false
	}
	switch openErr.Reason {
	case gossh.ConnectionFailed:
		return true, false
	case gossh.Prohibited:
		return false, true
	}
	return false, false
}
