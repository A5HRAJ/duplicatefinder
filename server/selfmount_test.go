package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A network mount whose server is this NAS — by an address of its own, by
// loopback, or by a name that resolves to either — is a self-mount; a mount
// of another server, and every local filesystem, is not.
func TestFindSelfMounts(t *testing.T) {
	dir := t.TempDir()
	mf := filepath.Join(dir, "mounts")
	os.WriteFile(mf, []byte(`/dev/md0 / ext4 rw 0 0
/dev/vg1000/lv /volume1 btrfs rw 0 0
//192.168.1.243.nip.io/Test/DF-staying /volume1/Backups/DF-selfmount cifs rw,vers=3.0,addr=192.168.1.243,soft 0 0
//other.lan/Media /volume1/Backups/other cifs rw,addr=192.168.1.50 0 0
//127.0.0.1/Test /volume1/Backups/loop cifs rw,addr=127.0.0.1 0 0
ds916.lan:/volume1/Test /volume1/Backups/nfs\040self nfs4 rw,clientaddr=192.168.1.243,addr=192.168.1.243 0 0
nas2:/volume1/x /volume1/Backups/nfs-other nfs rw,clientaddr=192.168.1.243,addr=192.168.1.60 0 0
//byname/Test /volume1/Backups/byname cifs rw 0 0
//elsewhere/Test /volume1/Backups/elsewhere cifs rw 0 0
//unresolvable/Test /volume1/Backups/unknown cifs rw 0 0
`), 0o644)
	defer func(f string, a func() []net.IP, l func(string) ([]net.IP, error)) {
		mountsFile, localAddrs, lookupHost = f, a, l
	}(mountsFile, localAddrs, lookupHost)
	mountsFile = mf
	localAddrs = func() []net.IP { return []net.IP{net.ParseIP("192.168.1.243"), net.ParseIP("10.0.0.2")} }
	lookupHost = func(h string) ([]net.IP, error) {
		switch h {
		case "byname":
			return []net.IP{net.ParseIP("10.0.0.2")}, nil
		case "elsewhere":
			return []net.IP{net.ParseIP("192.168.1.77")}, nil
		}
		return nil, errors.New("no such host")
	}
	got := map[string]bool{}
	for _, m := range findSelfMounts() {
		got[m.point] = true
	}
	want := map[string]bool{
		"/volume1/Backups/DF-selfmount": true,
		"/volume1/Backups/loop":         true,
		"/volume1/Backups/nfs self":     true, // \040 unescaped; clientaddr= is not the server
		"/volume1/Backups/byname":       true,
		"/volume1/Backups/unknown":      true, // a server that cannot be established counts as this NAS
	}
	for p := range want {
		if !got[p] {
			t.Errorf("%s: not recognised as a mount of this NAS", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("%s: wrongly taken for a mount of this NAS", p)
		}
	}

	set := findSelfMounts()
	if _, in := set.under("/volume1/Backups/DF-selfmount/a/b.jpg"); !in {
		t.Error("a file inside the mount must be under it")
	}
	if _, in := set.under("/volume1/Backups/DF-selfmount"); !in {
		t.Error("the mount point itself must be under it")
	}
	if _, in := set.under("/volume1/Backups/DF-selfmountX/b.jpg"); in {
		t.Error("a sibling sharing the mount point's prefix is not under it")
	}
}

func TestMountHost(t *testing.T) {
	for src, want := range map[string]string{
		"//host/share/sub": "host",
		"//[fe80::1]/s":    "fe80::1",
		"nas:/volume1/x":   "nas",
		"[fd00::5]:/v/x":   "fd00::5",
		"none":             "",
	} {
		if got := mountHost(src); got != want {
			t.Errorf("mountHost(%q) = %q, want %q", src, got, want)
		}
	}
}
