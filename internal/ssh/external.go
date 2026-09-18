package ssh

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ExternalSSHArgs builds the argv options for shelling out to the system
// `ssh` binary (image transfer, rsync transport). Every transfer channel
// previously ran with `-o StrictHostKeyChecking=no`, which disabled host-key
// verification entirely for those connections — a securely verified Go
// control connection does not authenticate a separate ssh(1) process, so a
// MITM'd transfer channel could feed a tampered image to the server even
// though the control channel was clean.
//
// acceptNew mirrors the control connection's policy: strict verification by
// default; accept-new (enroll unknown hosts, reject changes) only when the
// control connection itself was created with --accept-new — by transfer time
// it has already verified and recorded the host key in ~/.ssh/known_hosts, so
// strict mode adds no first-run friction.
//
// host may carry a :port suffix (and may be a bare IPv6 literal, which is
// bracketed for rsync target syntax by RsyncTarget). BatchMode=yes keeps a
// missing key from hanging on a passphrase prompt.
func ExternalSSHArgs(host, keyPath string, acceptNew bool) []string {
	policy := "yes"
	if acceptNew {
		policy = "accept-new"
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=" + policy,
	}
	if _, port, err := SplitHostPort(host); err == nil && port != "" && port != "22" {
		args = append(args, "-p", port)
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	return args
}

// ExternalSSHCommand renders ExternalSSHArgs as the single shell string
// rsync's -e option expects. rsync hands the -e value to a shell, so a
// bare strings.Join of the argv breaks the moment an argument contains a
// space (an identity path like ~/My Keys/id_ed25519); each element is
// quoted exactly once for that re-parse (TCL-52).
func ExternalSSHCommand(host, keyPath string, acceptNew bool) string {
	args := append([]string{"ssh"}, ExternalSSHArgs(host, keyPath, acceptNew)...)
	for i, a := range args {
		args[i] = ShellQuote(a)
	}
	return strings.Join(args, " ")
}

// RsyncTarget renders user@host:path for an rsync destination, bracketing a
// bare IPv6 host (rsync's colon syntax cannot carry an unbracketed IPv6
// literal) and moving ANY host:port suffix out of the target — rsync has no
// port syntax; the port belongs in the -e ssh args (see ExternalSSHArgs).
// The host is re-derived from the parsed endpoint, so an explicit :22 is
// stripped just like any other port (it used to be left in place and then
// bracketed as if it were part of an IPv6 literal), and an already-bracketed
// IPv6 address with a port is never bracketed twice (TCL-52).
func RsyncTarget(user, host, remotePath string) string {
	rsyncHost := host
	if h, port, err := SplitHostPort(host); err == nil && port != "" {
		rsyncHost = strings.Trim(h, "[]")
	}
	if strings.Contains(rsyncHost, ":") {
		rsyncHost = "[" + rsyncHost + "]"
	}
	return user + "@" + rsyncHost + ":" + remotePath
}

// SplitHostPort splits an endpoint that may be "host", "host:port", a
// bracketed IPv6 "[host]:port", or a bare IPv6 literal. Returns
// ("", "", error) for anything else.
func SplitHostPort(endpoint string) (host, port string, err error) {
	if endpoint == "" {
		return "", "", fmt.Errorf("empty endpoint")
	}
	if h, p, splitErr := net.SplitHostPort(endpoint); splitErr == nil && h != "" && p != "" {
		if _, convErr := strconv.Atoi(p); convErr == nil {
			return h, p, nil
		}
	}
	if ip := net.ParseIP(strings.Trim(endpoint, "[]")); ip != nil {
		return ip.String(), "", nil
	}
	// Plain hostname without port.
	if !strings.Contains(endpoint, ":") && !strings.ContainsAny(endpoint, "/@ ") {
		return endpoint, "", nil
	}
	return "", "", fmt.Errorf("invalid SSH endpoint %q", endpoint)
}
