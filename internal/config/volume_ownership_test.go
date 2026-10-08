package config

import "testing"

func TestVolumeOwnershipAdmission(t *testing.T) {
	base := "app: observe\ningress: host\nimage: observe:test\nport: 5080\nvolumes:\n  observe-state: /var/lib/observe\n"
	for _, tail := range []string{
		"volume_ownership:\n  observe-state: {uid: 10001, gid: 10001, mode: \"0700\"}\n",
		"accessories:\n  db:\n    image: postgres:17\n    volumes: {data: /data}\n    volume_ownership:\n      data: {uid: 999, gid: 999, mode: \"0750\"}\n",
	} {
		if _, err := ParseAppBytes([]byte(base + tail)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tail := range []string{
		"volume_ownership:\n  observe-state: {uid: -1, gid: 10001, mode: \"0700\"}\n",
		"volume_ownership:\n  observe-state: {uid: 4294967295, gid: 10001, mode: \"0700\"}\n",
		"volume_ownership:\n  observe-state: {gid: 10001, mode: \"0700\"}\n",
		"volume_ownership:\n  observe-state: {uid: 10001, gid: 10001, mode: \"777\"}\n",
		"volume_ownership:\n  observe-state: {uid: 10001, gid: 10001, mode: \"4777\"}\n",
		"volume_ownership:\n  observe-state: {uid: 10001, gid: 10001, mode: \"0777\"}\n",
		"volume_ownership:\n  missing: {uid: 10001, gid: 10001, mode: \"0700\"}\n",
		"volume_ownership:\n  /tmp/outside: {uid: 10001, gid: 10001, mode: \"0700\"}\n",
	} {
		if _, err := ParseAppBytes([]byte(base + tail)); err == nil {
			t.Fatalf("accepted invalid ownership: %s", tail)
		}
	}
	if _, err := ParseAppBytes([]byte(base)); err != nil {
		t.Fatalf("legacy volumes broken: %v", err)
	}
}
