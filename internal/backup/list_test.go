package backup

import (
	"testing"
)

// The recursive listing parser maps keys to rows and skips strays.
func TestClassifyListedKey(t *testing.T) {
	row, ok := classifyListedKey("myapp", "myapp/volumes/20261020-031704-1f2e3d4c5b6a7988.tar.gz")
	if !ok || row.Kind != KindVolumes || row.ID != "20261020-031704-1f2e3d4c5b6a7988" || !row.Restorable {
		t.Fatalf("volumes row: %+v ok=%v", row, ok)
	}
	if row.CreatedAt != "2026-10-20T03:17:04Z" {
		t.Fatalf("created_at = %q", row.CreatedAt)
	}
	if row, ok := classifyListedKey("myapp", "myapp/accessories/db/20261019-030001.sql.gz"); !ok || row.Kind != KindAccessory || row.Accessory != "db" {
		t.Fatalf("accessory row: %+v ok=%v", row, ok)
	}
	if row, ok := classifyListedKey("myapp", "myapp/teploy/20261019-030001.tar.gz"); !ok || row.Kind != KindTeployData || row.Restorable {
		t.Fatalf("teploy row: %+v ok=%v", row, ok)
	}
	for _, stray := range []string{
		"myapp/volumes/notabackup.tar.gz",
		"myapp/volumes/20261020-031704.tar.gz.tmp",
		"otherapp/volumes/20261020-031704.tar.gz",
		"myapp/unknown/20261020-031704.tar.gz",
		"myapp/accessories/20261020-031704.tar.gz",
		"myapp/volumes/nested/20261020-031704.tar.gz",
	} {
		if row, ok := classifyListedKey("myapp", stray); ok {
			t.Errorf("stray classified: %s -> %+v", stray, row)
		}
	}
}
