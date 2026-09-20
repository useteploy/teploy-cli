package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)


// TestExtractBinary_BoundedAndSingleRegular is the T48 regression: archive
// members are read under a decompressed-size bound, non-regular and
// duplicate matches are refused, and the entry count is capped — the old
// bare io.ReadAll let a small compressed member expand without limit.
func TestExtractBinary_BoundedAndSingleRegular(t *testing.T) {
	// A legitimate archive with the binary plus metadata extracts fine.
	var tarBuf bytes.Buffer
	gz := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gz)
	payload := []byte("binary-bytes")
	tw.WriteHeader(&tar.Header{Name: "teploy", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg})
	tw.Write(payload)
	tw.WriteHeader(&tar.Header{Name: "checksums.txt", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	tw.Write([]byte("abcd"))
	tw.Close()
	gz.Close()
	got, err := extractBinary(tarBuf.Bytes(), "tar.gz", "teploy")
	if err != nil || string(got) != "binary-bytes" {
		t.Fatalf("extractBinary: (%q, %v)", got, err)
	}

	// A member whose declared size exceeds the bound is refused WITHOUT
	// reading it.
	var big bytes.Buffer
	gz2 := gzip.NewWriter(&big)
	tw2 := tar.NewWriter(gz2)
	tw2.WriteHeader(&tar.Header{Name: "teploy", Size: maxUpdateBinarySize + 1, Typeflag: tar.TypeReg})
	tw2.Close()
	gz2.Close()
	if _, err := extractBinary(big.Bytes(), "tar.gz", "teploy"); err == nil {
		t.Error("oversized declared member accepted")
	}

	// Duplicate names are refused.
	var dup bytes.Buffer
	gz3 := gzip.NewWriter(&dup)
	tw3 := tar.NewWriter(gz3)
	for i := 0; i < 2; i++ {
		tw3.WriteHeader(&tar.Header{Name: "teploy", Size: 1, Typeflag: tar.TypeReg})
		tw3.Write([]byte("x"))
	}
	tw3.Close()
	gz3.Close()
	if _, err := extractBinary(dup.Bytes(), "tar.gz", "teploy"); err == nil {
		t.Error("duplicate binary accepted")
	}

	// A directory masquerading as the binary is refused.
	var dir bytes.Buffer
	gz4 := gzip.NewWriter(&dir)
	tw4 := tar.NewWriter(gz4)
	tw4.WriteHeader(&tar.Header{Name: "teploy", Typeflag: tar.TypeDir})
	tw4.Close()
	gz4.Close()
	if _, err := extractBinary(dir.Bytes(), "tar.gz", "teploy"); err == nil {
		t.Error("directory entry accepted as the binary")
	}
}
