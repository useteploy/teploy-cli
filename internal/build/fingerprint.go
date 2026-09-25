package build

import (
	"crypto/sha256"
	"io"
	"os"
	"runtime"
)

func hashFile(path string) ([32]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return [32]byte{}, 0, err
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, size, nil
}

// EffectiveLocalPlatform returns the platform a LOCAL Dockerfile build
// targets for the given configured platform: an explicit platform wins;
// otherwise building on Apple silicon targets linux/amd64 (the deploy
// default since the first local-build support — an arm64 macOS host
// otherwise produces images the typical amd64 server cannot run); anywhere
// else the daemon's native default applies (empty). Shared by the build
// itself and the C04 provenance record so both name the same platform.
func EffectiveLocalPlatform(platform string) string {
	if platform != "" {
		return platform
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "linux/amd64"
	}
	return ""
}
