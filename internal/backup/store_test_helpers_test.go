package backup

import "github.com/useteploy/teploy/internal/ssh"

// newStoreMock is ssh.NewMockExecutor with the verified-upload path answered:
// the local measurement and the stored object agree, so a test about something
// else does not have to spell the upload's checks out. A test about the upload
// itself uses ssh.NewMockExecutor.
func newStoreMock(host string, cmds ...ssh.MockCommand) *ssh.MockExecutor {
	return ssh.NewMockExecutor(host, append(append([]ssh.MockCommand{}, storeStubs...), cmds...)...)
}

var storeStubs = []ssh.MockCommand{
	{Match: "sha256sum", Output: "abc  f\n4096\n"},
	{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
	{Match: "{ ", Output: "abc  -\n"},
	{Match: "printf '%s\\n'", Output: ""},
	{Match: "for p in /etc/teploy/age-key", Output: ""},
	{Match: "aws s3api head-object", Output: `{"ContentLength":4096,"Metadata":{"sha256":"abc"}}`},
}

// Valid X25519 age recipients (fixed values: public keys, no matching identity is kept).
const (
	testOwnerRecipient  = "age1lggyhqrw2nlhcxprm67z43rta597azn8gknawjehu9d9dl0jq3yqqvfafg"
	testServerRecipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
)
