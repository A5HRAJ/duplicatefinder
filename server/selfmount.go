// Shared folders of this NAS mounted back into itself. File Station's Mount
// Remote Folder refuses this NAS's own addresses and names, but not a name
// that merely resolves to one of them (measured on DSM 7.4: a CIFS mount of
// \\192.168.1.243.nip.io\Test\DF-staying into another share succeeds). Every
// file under such a mount is a second path to a file that already has one,
// so a scan over both pairs each file with itself as a "duplicate", and
// moving one path takes the other with it: the copy keep-one left in place
// goes too. Such a mount is never scanned, moved out of or moved into.
package main

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// selfMount is a network filesystem mounted at point whose server is this
// NAS; source is the mount's source as the system records it.
type selfMount struct {
	point  string
	source string
}

// selfMountSet is the self-mounts in effect, looked up by path.
type selfMountSet []selfMount

// under returns the self-mount p lies at or under, if any.
func (m selfMountSet) under(p string) (selfMount, bool) {
	for _, s := range m {
		if p == s.point || strings.HasPrefix(p, s.point+"/") {
			return s, true
		}
	}
	return selfMount{}, false
}

// why is the sentence that explains a refusal or a skipped folder.
func (s selfMount) why() string {
	return "it is inside " + s.point + ", a remote folder mounted from this same NAS (" + s.source +
		"), so every file in it is a second path to a file that is already here — use the folder itself, not the mount"
}

// mountsFile is the kernel's table of mounts; a variable for the tests.
var mountsFile = "/proc/mounts"

// localAddrs returns this machine's own addresses; a variable for the tests.
var localAddrs = func() []net.IP {
	var out []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out
}

// lookupHost resolves a mount source's server name; a variable for the tests.
var lookupHost = func(host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// netFSTypes are the filesystems Mount Remote Folder creates.
var netFSTypes = map[string]bool{"cifs": true, "smb3": true, "smbfs": true, "nfs": true, "nfs4": true}

// findSelfMounts reads the mount table for network filesystems served by this
// NAS. The server is the address the kernel connected to — the addr= option,
// which DSM's CIFS and NFS mounts both carry — or, failing that, the source's
// host name resolved now. A server that cannot be established at all counts
// as this NAS: skipping a real remote folder is visible and harmless, and
// scanning a mount of this NAS is not.
func findSelfMounts() selfMountSet {
	f, err := os.Open(mountsFile)
	if err != nil {
		return nil
	}
	defer f.Close()
	own := localAddrs()
	isOwn := func(ip net.IP) bool {
		if ip == nil {
			return false
		}
		if ip.IsLoopback() || ip.IsUnspecified() {
			return true
		}
		for _, o := range own {
			if o.Equal(ip) {
				return true
			}
		}
		return false
	}
	var out selfMountSet
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || !netFSTypes[fields[2]] {
			continue
		}
		source, point := unescapeMount(fields[0]), unescapeMount(fields[1])
		var ips []net.IP
		for _, opt := range strings.Split(fields[3], ",") {
			if v, ok := strings.CutPrefix(opt, "addr="); ok {
				if ip := net.ParseIP(v); ip != nil {
					ips = append(ips, ip)
				}
			}
		}
		if len(ips) == 0 {
			if host := mountHost(source); host != "" {
				if ip := net.ParseIP(host); ip != nil {
					ips = []net.IP{ip}
				} else {
					ips, _ = lookupHost(host)
				}
			}
		}
		self := len(ips) == 0
		for _, ip := range ips {
			if isOwn(ip) {
				self = true
			}
		}
		if self {
			point = filepath.Clean(point)
			out = append(out, selfMount{point: point, source: source})
			// The app compares canonical paths; the table holds the path the
			// mount was made at. The parent is resolved rather than the point
			// itself, so the mount is never touched to find out.
			if rp, err := filepath.EvalSymlinks(filepath.Dir(point)); err == nil {
				if alt := filepath.Join(rp, filepath.Base(point)); alt != point {
					out = append(out, selfMount{point: alt, source: source})
				}
			}
		}
	}
	return out
}

// mountHost is the server part of a mount source: "//host/share/…" (CIFS)
// or "host:/path" (NFS), with an IPv6 literal's brackets removed.
func mountHost(source string) string {
	if rest, ok := strings.CutPrefix(source, "//"); ok {
		host, _, _ := strings.Cut(rest, "/")
		return strings.Trim(host, "[]")
	}
	if strings.HasPrefix(source, "[") {
		if i := strings.Index(source, "]:"); i > 0 {
			return source[1:i]
		}
	}
	if i := strings.LastIndex(source, ":/"); i > 0 {
		return source[:i]
	}
	return ""
}

// unescapeMount undoes the mount table's octal escapes (\040 for a space,
// \011, \012, \134).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
