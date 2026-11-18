#!/bin/bash
# The front door: spk/ui/api.cgi, the gate every request from DSM passes
# before it reaches the daemon. Run off the NAS against stand-ins for what
# DSM provides — authenticate.cgi (which user a session or SynoToken
# names), `id` and `getent` (which groups that user is in) — and a stand-in
# for the app binary that records being reached. Proves that only a signed-in
# administrator gets through, and that the gate fails closed everywhere else:
#
#   no session                      401, nothing reached
#   a signed-in non-administrator   403, nothing reached
#   an administrator                reached, by token (query or header) or,
#                                   with no token at all, by session cookie
#   administrators under another GID  reached (the group's own GID is used)
#   a group merely NAMED like it    403
#   a token that does not validate  401, never falling back to the cookie
#   a helper that prints a name and then fails   401
#   an empty, duplicated or smuggled SynoToken   400
#   no authenticate.cgi             403
#   /debug                          administrators only
#
# Requires: a POSIX sh, readlink -f. No network, no Go. check.sh runs it.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(cd .. && pwd)"
W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

# DSM installs the package at <volume>/@appstore/<id>; the gate derives the
# id from its own resolved path.
CGI="$W/appstore/duplicatefinder/ui/api.cgi"
mkdir -p "$(dirname "$CGI")" "$W/modules" "$W/stubs" "$W/packages/duplicatefinder/target/bin" "$W/packages/duplicatefinder/var"
cp "$ROOT/spk/ui/api.cgi" "$CGI"
chmod 755 "$CGI"

cat > "$W/modules/authenticate.cgi" <<'EOF'
#!/bin/sh
# Stand-in for DSM's authenticate.cgi: prints the user a SynoToken or the
# session cookie names, exits non-zero for anything else.
echo "$QUERY_STRING" >> "${0%/*}/../auth.log"
tok=""
case "$QUERY_STRING" in SynoToken=*) tok="${QUERY_STRING#SynoToken=}" ;; esac
[ -n "$tok" ] || tok="${HTTP_X_SYNO_TOKEN:-}"
if [ -n "$tok" ]; then
	# Like DSM's, a token is checked against the session: no cookie, no user.
	case "${HTTP_COOKIE:-}" in *id=*) ;; *) exit 1 ;; esac
	case "$tok" in
		tok-alice) echo alice ;;
		tok-bob) echo bob ;;
		tok-carol) echo carol ;;
		tok-dave) echo dave ;;
		tok-broken) echo alice; exit 1 ;;
		*) exit 1 ;;
	esac
	exit 0
fi
case "${HTTP_COOKIE:-}" in
	*id=sess-alice*) echo alice ;;
	*id=sess-bob*) echo bob ;;
	*) exit 1 ;;
esac
EOF
# alice: in GID 101, DSM's administrators. carol: in the administrators group
# under a GID other than 101. bob: users only. dave: in a group NAMED
# "backup administrators" (GID 2000), which is not the administrators group.
cat > "$W/stubs/id" <<'EOF'
#!/bin/sh
case "$1" in
	-G) case "${3:-}" in
		alice) echo "100 101" ;;
		bob) echo "100" ;;
		carol) echo "100 1234" ;;
		dave) echo "100 2000" ;;
		*) exit 1 ;;
	esac ;;
	-Gn) case "${3:-}" in
		alice|carol) echo "users administrators" ;;
		bob) echo "users" ;;
		dave) echo "users backup administrators" ;;
		*) exit 1 ;;
	esac ;;
	-un) echo http ;;
	*) exit 1 ;;
esac
EOF
cat > "$W/stubs/getent" <<'EOF'
#!/bin/sh
[ "$1" = group ] && [ "$2" = administrators ] && echo "administrators:x:1234:carol"
exit 0
EOF
cat > "$W/packages/duplicatefinder/target/bin/dupfinder" <<EOF
#!/bin/sh
# Stand-in for the app binary: records that the gate let a request through.
echo "\$PATH_INFO" >> "$W/reached"
printf 'Content-Type: application/json\r\n\r\n{"reached":true,"var":"%s"}' "\$DUPFINDER_VAR"
EOF
chmod 755 "$W/modules/authenticate.cgi" "$W/stubs/id" "$W/stubs/getent" "$W/packages/duplicatefinder/target/bin/dupfinder"

# gate <path> <query> <cookie> <x-syno-token> — runs the CGI as DSM's web
# server would, with a clean environment, and prints its full answer.
MODULES="$W/modules"
gate() {
	rm -f "$W/reached"
	env -i PATH="$W/stubs:/usr/bin:/bin:/usr/sbin:/sbin" \
		DUPFINDER_MODULES="$MODULES" DUPFINDER_PACKAGES="$W/packages" \
		REQUEST_METHOD=GET PATH_INFO="$1" QUERY_STRING="$2" HTTP_COOKIE="$3" HTTP_X_SYNO_TOKEN="$4" \
		sh "$CGI" </dev/null
}
reached() { [ -f "$W/reached" ]; }
# expect <label> <status-substring|reached> <path> <query> <cookie> <x-token>
expect() {
	local label="$1" want="$2" out
	out="$(gate "$3" "$4" "$5" "$6")"
	if [ "$want" = reached ]; then
		reached || fail "$label: the request was not let through: $out"
		echo "$out" | grep -q '"reached":true' || fail "$label: the binary's answer did not come back: $out"
	else
		reached && fail "$label: the request reached the app"
		echo "$out" | head -1 | grep -q "Status: $want" || fail "$label: want $want, got: $out"
	fi
	pass "$label"
}

echo "== gateway"
expect "no session is refused"                                     401 /state "" "" ""
expect "a signed-in non-administrator is refused"                  403 /state "" "id=sess-bob" ""
expect "an administrator's session cookie gets through when no token is sent" reached /state "" "id=sess-alice" ""
expect "an administrator's SynoToken in the query gets through"    reached /state "SynoToken=tok-alice" "id=sess-any" ""
expect "an administrator's SynoToken in the header gets through"   reached /state "" "id=sess-any" "tok-alice"
expect "a non-administrator's token is refused"                    403 /state "SynoToken=tok-bob" "id=sess-any" ""
expect "an administrator whose group has another GID gets through" reached /state "SynoToken=tok-carol" "id=sess-any" ""
expect "a group merely named like administrators is refused"       403 /state "SynoToken=tok-dave" "id=sess-any" ""
# The stand-in cannot tell a cookie-only call from a token call (both see the
# request's own variables), so the fall-back is caught by counting: after a
# token fails, authenticate.cgi must not be asked a second time.
: > "$W/auth.log"
expect "a token that does not validate never falls back to the cookie" 401 /state "SynoToken=tok-forged" "id=sess-alice" ""
[ "$(wc -l < "$W/auth.log" | tr -d ' ')" = 1 ] || fail "after a token failed to validate, authenticate.cgi was asked again: $(cat "$W/auth.log")"
: > "$W/auth.log"
expect "a forged token in the header never falls back to the cookie"    401 /state "" "id=sess-alice" "tok-forged"
[ "$(wc -l < "$W/auth.log" | tr -d ' ')" = 1 ] || fail "after a header token failed to validate, authenticate.cgi was asked again: $(cat "$W/auth.log")"
expect "a helper that prints a name and then fails is refused"     401 /state "SynoToken=tok-broken" "id=sess-any" ""
expect "an empty SynoToken is refused, never sliding to the cookie" 400 /state "SynoToken=" "id=sess-alice" ""
expect "a token without the session cookie is refused"              401 /state "SynoToken=tok-alice" "" ""
expect "a valid token followed by an empty one is refused"          400 /state "SynoToken=tok-alice&SynoToken=" "id=sess-any" ""
expect "two SynoTokens are refused"                                400 /state "SynoToken=tok-alice&SynoToken=tok-bob" "id=sess-any" ""
expect "a second parameter smuggled inside the token is refused"   400 /state "SynoToken=tok-alice%26SynoToken%3Dx" "id=sess-any" ""
expect "the diagnostics page is refused to a non-administrator"    403 /debug "SynoToken=tok-bob" "id=sess-any" ""
out="$(gate /debug "SynoToken=tok-alice" "id=sess-any" "")"
echo "$out" | grep -q '"isAdmin":true' || fail "the diagnostics page did not answer an administrator: $out"
reached && fail "the diagnostics page ran the app binary"
pass "the diagnostics page answers an administrator, from the gate itself"
out="$(gate /state "SynoToken=tok-alice" "id=sess-any" "")"
echo "$out" | grep -q "\"var\":\"$W/packages/duplicatefinder/var\"" || fail "the app was not told this package's own data folder: $out"
pass "the app is handed this package's own data folder"
grep -q 'SynoToken=tok-alice%26' "$W/auth.log" && fail "a smuggled parameter reached authenticate.cgi"
pass "authenticate.cgi is only ever asked about a single, plain token"

MODULES="$W/no-modules"
expect "no authenticate.cgi refuses every request"                 403 /state "SynoToken=tok-alice" "id=sess-any" ""
MODULES="$W/modules"
rm "$W/packages/duplicatefinder/target/bin/dupfinder"
expect "a package that is not installed is refused"                500 /state "SynoToken=tok-alice" "id=sess-any" ""

echo
echo "GATEWAY CHECKS PASSED"
