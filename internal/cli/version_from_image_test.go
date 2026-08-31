package cli

import "testing"

// The bug this guards: `teploy deploy --image repo/app:462d7a7` labelled the
// deploy with the local git HEAD (fe82bf0), so the container name, the deploy
// log and the operator's screen all named a commit whose code was not running.
func TestVersionFromImage(t *testing.T) {
	cases := []struct {
		name  string
		image string
		want  string
	}{
		{
			// The production case. The registry port contains a colon, so a
			// naive "text after the last colon" split returns the wrong thing
			// for a registry-qualified reference without a tag.
			name:  "registry with port and tag",
			image: "100.108.123.49:49152/tyler/fylun-web:462d7a7",
			want:  "462d7a7",
		},
		{name: "simple tag", image: "nginx:1.25", want: "1.25"},
		{name: "namespaced tag", image: "black-forest-labs/flux:v2", want: "v2"},

		// A registry port must not be mistaken for a tag.
		{name: "registry port, no tag", image: "100.108.123.49:49152/tyler/app", want: ""},
		{name: "untagged", image: "nginx", want: ""},

		// Reusing a floating tag as the version gives every deploy the same
		// container name and leaves CurrentHash == PreviousHash, which disables
		// rollback. The caller falls back to a timestamp instead.
		{name: "latest is refused", image: "nginx:latest", want: ""},

		// A digest is content-addressed and the most truthful label available.
		// Colons are invalid in container names, hence the dashed prefix.
		{
			name:  "digest",
			image: "repo/app@sha256:" + "ab12cd34ef56" + "0000000000000000000000000000000000000000000000000000",
			want:  "sha256-ab12cd34ef56",
		},
		{
			// Docker resolves the digest and ignores the tag, so labelling this
			// "1.2.3" would be the same lie this function exists to remove.
			name:  "digest wins over an accompanying tag",
			image: "repo/app:1.2.3@sha256:" + "ab12cd34ef56" + "0000000000000000000000000000000000000000000000000000",
			want:  "sha256-ab12cd34ef56",
		},
		{name: "malformed digest", image: "repo/app@sha256:tooshort", want: ""},

		// Versions are interpolated UNQUOTED into remote docker commands via
		// ContainerName, and an image reference is a less-trusted source than
		// git output. Anything outside Docker's tag grammar is refused.
		{name: "shell metacharacters refused", image: "repo/app:v1;rm -rf /", want: ""},
		{name: "backtick refused", image: "repo/app:`whoami`", want: ""},
		{name: "leading dot refused", image: "repo/app:.hidden", want: ""},
		{name: "empty", image: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionFromImage(tc.image); got != tc.want {
				t.Errorf("versionFromImage(%q) = %q, want %q", tc.image, got, tc.want)
			}
		})
	}
}

// Every accepted version must be safe to concatenate into a container name and
// into the remote shell commands that manipulate it.
func TestVersionFromImage_OutputIsContainerNameSafe(t *testing.T) {
	images := []string{
		"100.108.123.49:49152/tyler/fylun-web:462d7a7",
		"nginx:1.25",
		"repo/app@sha256:ab12cd34ef560000000000000000000000000000000000000000000000000000",
	}
	for _, image := range images {
		v := versionFromImage(image)
		if v == "" {
			t.Fatalf("expected a version for %q", image)
		}
		for _, r := range v {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
			if !ok {
				t.Errorf("versionFromImage(%q) = %q contains %q, unsafe in a container name", image, v, r)
			}
		}
	}
}
