package main

import "testing"

// The kernel's socket table, as /proc/net/tcp prints it: the daemon (uid
// 1027) listening on 127.0.0.1:9807 (0x264F), an established connection to
// it from another user, and an unrelated listener.
const tcpTable = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:264F 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1027        0 23456 1 0000000000000000 100 0 0 10 0
   1: 0100007F:264F 0100007F:D3A2 01 00000000:00000000 00:00000000 00000000  1000        0 23999 1 0000000000000000 20 4 30 10 -1
   2: 00000000:1388 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11111 1 0000000000000000 100 0 0 10 0
`

func TestListenersOnThePortAreFoundWithTheirOwner(t *testing.T) {
	got := parseListenUIDs(tcpTable, 9807)
	if len(got) != 1 || got[0] != 1027 {
		t.Fatalf("listeners on 9807: want [1027], got %v", got)
	}
	if got := parseListenUIDs(tcpTable, 5000); len(got) != 1 || got[0] != 0 {
		t.Fatalf("listeners on 5000: want [0], got %v", got)
	}
	if got := parseListenUIDs(tcpTable, 9808); len(got) != 0 {
		t.Fatalf("no listener on 9808: got %v", got)
	}
}

// The gateway hands the administrator's DSM session and the daemon token
// only to a listener owned by its own user, the package user the daemon
// runs as. Anything else on the daemon's port — a process that took it
// while the daemon was down — gets nothing.
func TestTheSessionGoesOnlyToTheDaemonsOwnUser(t *testing.T) {
	for _, tc := range []struct {
		name  string
		uids  []int
		known bool
		want  bool
	}{
		{"the daemon itself", []int{1027}, true, true},
		{"another user on the port", []int{1000}, true, false},
		{"another user beside the daemon", []int{1027, 1000}, true, false},
		{"nobody listening: the proxy reports the daemon is down", nil, true, true},
		{"no socket table to read (off Linux)", nil, false, true},
	} {
		if got := listenerTrusted(tc.uids, tc.known, 1027); got != tc.want {
			t.Errorf("%s: trusted=%v, want %v", tc.name, got, tc.want)
		}
	}
}
