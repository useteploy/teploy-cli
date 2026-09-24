package ssh

// sshtest_test.go — an in-process SSH server for pinning RemoteExecutor
// behavior without a network fixture: structured results (exit codes,
// separated streams), the bounded-command deadline, cancellation, and
// concurrent first-connect TOFU (C08). The server speaks the real
// x/crypto/ssh wire protocol over 127.0.0.1, so Connect's full path —
// dial, handshake, host-key callback, session — runs for real.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// testServerKey generates a fresh ed25519 SSH host key.
func testServerKey(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating host key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("wrapping host key: %v", err)
	}
	return signer
}

// writeClientKeyFile generates a client identity and writes the private
// key PEM where resolveSigners can load it.
func writeClientKeyFile(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating client key: %v", err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshaling client key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("writing client key: %v", err)
	}
	return path
}

// sshTestHandler runs one exec request: writes to ch are the command's
// stdout; the returned int is its exit status.
type sshTestHandler func(cmd string, ch gossh.Channel) int

// startTestSSHServer runs an SSH server on 127.0.0.1:0 that accepts any
// public key and dispatches every exec request to handler. It returns
// the dial address.
func startTestSSHServer(t *testing.T, handler sshTestHandler) string {
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

	serve := func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleTestSSHConn(conn, cfg, handler)
		}
	}
	go serve()
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().String()
}

func handleTestSSHConn(conn net.Conn, cfg *gossh.ServerConfig, handler sshTestHandler) {
	sconn, chans, reqs, err := gossh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go gossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(gossh.UnknownChannelType, "session only")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		go handleTestSSHSession(ch, requests, handler)
	}
}

func handleTestSSHSession(ch gossh.Channel, requests <-chan *gossh.Request, handler sshTestHandler) {
	defer ch.Close()
	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		// exec payload: uint32 length + command string.
		cmd := string(req.Payload[4:])
		if req.WantReply {
			req.Reply(true, nil)
		}
		status := 0
		if handler != nil {
			status = handler(cmd, ch)
		}
		// exit-status payload: uint32 status, big-endian.
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, uint32(status))
		ch.SendRequest("exit-status", false, payload)
		return
	}
}

// connectTestServer dials the test server with the given extra config.
// Each test gets a fresh $HOME (no prior known_hosts) and connects with
// AcceptNewHost — the real TOFU enrollment path — so the host-key
// plumbing runs exactly as a first deploy would.
func connectTestServer(t *testing.T, addr string, mutate func(*ConnectConfig)) *RemoteExecutor {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	keyPath := writeClientKeyFile(t)
	cfg := ConnectConfig{Host: addr, User: "root", KeyPath: keyPath, AcceptNewHost: true}
	if mutate != nil {
		mutate(&cfg)
	}
	exec, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting to test server: %v", err)
	}
	t.Cleanup(func() { exec.Close() })
	return exec
}

// TestRemoteExecutor_RunDetailed_StatusPins drives the REAL session
// path: exit code arrives from the server's exit-status request, stdout
// and stderr are separated, and a non-zero exit is a completed command
// (Err nil).
func TestRemoteExecutor_RunDetailed_StatusPins(t *testing.T) {
	if testing.Short() {
		t.Skip("network + processes")
	}
	addr := startTestSSHServer(t, func(cmd string, ch gossh.Channel) int {
		if cmd == "failcode" {
			fmt.Fprint(ch, "partial out")
			fmt.Fprint(ch.Stderr(), "the error")
			return 42
		}
		fmt.Fprint(ch, "hello")
		return 0
	})
	exec := connectTestServer(t, addr, nil)

	res := RunDetailed(context.Background(), exec, "ok")
	if res.ExitCode != 0 || res.Err != nil || string(res.Stdout) != "hello" {
		t.Fatalf("success shape: %+v %q", res, res.Stdout)
	}

	res = RunDetailed(context.Background(), exec, "failcode")
	if res.ExitCode != 42 || res.Err != nil {
		t.Fatalf("failure shape: %+v (exit code must arrive, Err must be nil)", res)
	}
	if string(res.Stdout) != "partial out" || string(res.Stderr) != "the error" {
		t.Fatalf("streams crossed: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestRemoteExecutor_CommandTimeoutBoundsHungCommand pins C08's bounded
// subprocess lifetime on the remote path: with CommandTimeout set, a
// command that never finishes dies at the deadline — RunDetailed
// returns with TimedOut set instead of hanging the CLI forever.
func TestRemoteExecutor_CommandTimeoutBoundsHungCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("network + processes")
	}
	addr := startTestSSHServer(t, func(cmd string, ch gossh.Channel) int {
		time.Sleep(30 * time.Second) // hung past any test patience
		return 0
	})
	exec := connectTestServer(t, addr, func(c *ConnectConfig) { c.CommandTimeout = 500 * time.Millisecond })

	start := time.Now()
	res := RunDetailed(context.Background(), exec, "hang")
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Fatalf("deadline expiry must set TimedOut: %+v", res)
	}
	if res.Err == nil {
		t.Fatal("timeout must surface an error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("hung command outlived its deadline: %s", elapsed)
	}

	// The connection remains usable after a timed-out command.
	res2 := RunDetailed(context.Background(), exec, "hang")
	if !res2.TimedOut {
		t.Fatalf("second hung command must also be bounded: %+v", res2)
	}
}

// TestRemoteExecutor_CancelMidRun pins remote cancellation: canceling
// the context mid-command returns promptly with Canceled set and the
// session torn down (no hidden work continues on the connection's
// session; the server-side process is the server's to manage, but the
// client session is closed after SIGTERM as before).
func TestRemoteExecutor_CancelMidRun(t *testing.T) {
	if testing.Short() {
		t.Skip("network + processes")
	}
	addr := startTestSSHServer(t, func(cmd string, ch gossh.Channel) int {
		time.Sleep(10 * time.Second)
		return 0
	})
	exec := connectTestServer(t, addr, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res := RunDetailed(ctx, exec, "hang")
	cancel()
	if !res.Canceled || res.TimedOut {
		t.Fatalf("explicit cancel: Canceled=%v TimedOut=%v, want true/false", res.Canceled, res.TimedOut)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancellation did not return promptly: %s", time.Since(start))
	}
}
