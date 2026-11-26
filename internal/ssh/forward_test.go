package ssh

// forward_test.go — ForwardDial against an in-process SSH server that
// answers direct-tcpip channel opens the way sshd does: it dials the
// address the client asked for and pipes the channel to it, or rejects the
// open. The tests pin what the sandbox commands rely on: a service on the
// server's loopback is reached through the connection, at the address
// asked for; a refusal says whether nothing listens or the server forbids
// forwarding; a context bounds a server that never answers; a forwarded
// connection ends with the SSH connection; and net/http runs over the
// channel as it is.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// directTCPIPMsg is the direct-tcpip open's payload (RFC 4254 §7.2), as
// the server reads it.
type directTCPIPMsg struct {
	Addr              string
	Port              uint32
	OriginatorAddress string
	OriginatorPort    uint32
}

// errForwardProhibited makes the test server reject the channel as a
// server with AllowTcpForwarding no does, instead of connect-failed.
var errForwardProhibited = errors.New("forwarding is administratively prohibited")

// forwardTestServer is the test server's side of a direct-tcpip open:
// return a connection to addr, or an error.
type forwardTestServer func(addr string) (net.Conn, error)

// dialForward dials addr on the server's side, as sshd does.
func dialForward(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 5*time.Second)
}

// startForwardSSHServer runs an SSH server on 127.0.0.1:0 that serves only
// direct-tcpip channels; every other channel type is rejected.
func startForwardSSHServer(t *testing.T, forward forwardTestServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	cfg := &gossh.ServerConfig{
		PublicKeyCallback: func(conn gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			return &gossh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(testServerKey(t))
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				sconn, chans, reqs, err := gossh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer sconn.Close()
				go gossh.DiscardRequests(reqs)
				for newCh := range chans {
					if newCh.ChannelType() != "direct-tcpip" {
						newCh.Reject(gossh.UnknownChannelType, "forwarding only")
						continue
					}
					var msg directTCPIPMsg
					if err := gossh.Unmarshal(newCh.ExtraData(), &msg); err != nil {
						newCh.Reject(gossh.ConnectionFailed, "could not parse direct-tcpip payload")
						continue
					}
					target, err := forward(net.JoinHostPort(msg.Addr, strconv.Itoa(int(msg.Port))))
					if errors.Is(err, errForwardProhibited) {
						newCh.Reject(gossh.Prohibited, "administratively prohibited")
						continue
					}
					if err != nil {
						newCh.Reject(gossh.ConnectionFailed, "connect failed")
						continue
					}
					ch, requests, err := newCh.Accept()
					if err != nil {
						target.Close()
						continue
					}
					go gossh.DiscardRequests(requests)
					go func() {
						io.Copy(ch, target)
						ch.CloseWrite()
					}()
					go func() {
						io.Copy(target, ch)
						target.Close()
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().String()
}

// echoServer is a TCP service that returns what it is sent.
func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().String()
}

func requireNetwork(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("network + processes")
	}
}

// readFullWithin reads len(buf) bytes from conn, or fails the test after
// limit. A forwarded connection supports no read deadline (ForwardDial),
// so the bound has to come from here.
func readFullWithin(t *testing.T, conn net.Conn, buf []byte, limit time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(conn, buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
	case <-time.After(limit):
		t.Fatalf("nothing arrived in %s", limit)
	}
}

// A service bound to the server's loopback is reached through the
// connection, at the address asked for, and carries data both ways.
func TestForwardDialReachesTheServersLoopback(t *testing.T) {
	requireNetwork(t)
	echo := echoServer(t)
	var mu sync.Mutex
	var asked []string
	addr := startForwardSSHServer(t, func(addr string) (net.Conn, error) {
		mu.Lock()
		asked = append(asked, addr)
		mu.Unlock()
		return dialForward(addr)
	})
	exec := connectTestServer(t, addr, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := exec.ForwardDial(ctx, echo)
	if err != nil {
		t.Fatalf("ForwardDial: %v", err)
	}
	defer conn.Close()

	message := []byte("through the tunnel\n")
	if _, err := conn.Write(message); err != nil {
		t.Fatalf("writing: %v", err)
	}
	got := make([]byte, len(message))
	readFullWithin(t, conn, got, 5*time.Second)
	if !bytes.Equal(got, message) {
		t.Fatalf("echo came back %q, want %q", got, message)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != echo {
		t.Fatalf("the server was asked for %v, want exactly %s", asked, echo)
	}
}

// A forwarded connection ends with the SSH connection: a reader on it is
// released rather than left waiting.
func TestForwardDialEndsWithTheConnection(t *testing.T) {
	requireNetwork(t)
	echo := echoServer(t)
	addr := startForwardSSHServer(t, dialForward)
	exec := connectTestServer(t, addr, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := exec.ForwardDial(ctx, echo)
	if err != nil {
		t.Fatalf("ForwardDial: %v", err)
	}
	if err := exec.Close(); err != nil {
		t.Fatalf("closing the connection: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reading a connection whose SSH connection is closed returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarded connection outlived the SSH connection")
	}
}

// Nothing listening at the address: the server rejects the channel and
// ForwardRefused names it as a refused connection, not a policy.
func TestForwardDialRefusedWhenNothingListens(t *testing.T) {
	requireNetwork(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	nowhere := closed.Addr().String()
	closed.Close()

	addr := startForwardSSHServer(t, dialForward)
	exec := connectTestServer(t, addr, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := exec.ForwardDial(ctx, nowhere)
	if err == nil {
		conn.Close()
		t.Fatal("ForwardDial to a closed port succeeded")
	}
	refused, prohibited := ForwardRefused(err)
	if !refused || prohibited {
		t.Fatalf("ForwardRefused = refused %v, prohibited %v; want true, false (err: %v)", refused, prohibited, err)
	}
}

// sshd forbidding the forward is a different answer, and the two are told
// apart: the remedy is the server's sshd configuration, not the service.
func TestForwardDialProhibited(t *testing.T) {
	requireNetwork(t)
	echo := echoServer(t)
	addr := startForwardSSHServer(t, func(string) (net.Conn, error) {
		return nil, errForwardProhibited
	})
	exec := connectTestServer(t, addr, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := exec.ForwardDial(ctx, echo)
	if err == nil {
		t.Fatal("ForwardDial succeeded through a server that forbids forwarding")
	}
	refused, prohibited := ForwardRefused(err)
	if refused || !prohibited {
		t.Fatalf("ForwardRefused = refused %v, prohibited %v; want false, true (err: %v)", refused, prohibited, err)
	}
}

// A server that never answers the channel open cannot hold the caller: the
// context bounds the wait, and the error is the context's — neither of
// the server's two refusals.
func TestForwardDialIsBoundedByItsContext(t *testing.T) {
	requireNetwork(t)
	never := make(chan struct{})
	addr := startForwardSSHServer(t, func(string) (net.Conn, error) {
		<-never // the connect never returns
		return nil, io.ErrClosedPipe
	})
	t.Cleanup(func() { close(never) })
	exec := connectTestServer(t, addr, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	conn, err := exec.ForwardDial(ctx, "127.0.0.1:9")
	if err == nil {
		conn.Close()
		t.Fatal("ForwardDial returned a connection the server never confirmed")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context's deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the wait outlived the context: %s", elapsed)
	}
	if refused, prohibited := ForwardRefused(err); refused || prohibited {
		t.Fatal("a context deadline is neither a refused connection nor a prohibition")
	}
}

// net/http runs over the forwarded channel as it is: the channel supports
// no deadlines, and HTTP/1.1 never sets them. A body far larger than one
// SSH channel window arrives whole, and a request context still ends a
// response that never comes.
func TestForwardDialCarriesHTTP(t *testing.T) {
	requireNetwork(t)
	body := make([]byte, 3<<20) // well past one SSH channel window
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(service.Close)
	serviceURL := service.Listener.Addr().String()
	addr := startForwardSSHServer(t, dialForward)
	exec := connectTestServer(t, addr, nil)

	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return exec.ForwardDial(ctx, serviceURL)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://sandbox.invalid/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET through the tunnel: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("status %d, %d bytes of %d", resp.StatusCode, len(got), len(body))
	}

	// A handler that never answers ends when the request's context does.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	slow, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sandbox.invalid/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err = client.Do(slow)
	if err == nil {
		resp.Body.Close()
		t.Fatal("a request with a deadline outlived it")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the request context's deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the request outlived its context: %s", elapsed)
	}
}
