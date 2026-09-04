#!/bin/sh
# Duplicate Finder API gateway.
# DSM's web server executes this for /webman/3rdparty/<package>/api.cgi/*.
# It verifies the DSM session, requires an administrator, then hands the
# request to the Go binary in CGI mode, which proxies to the local daemon.
#
# Session validation (DSM 7): authenticate.cgi resolves the logged-in user
# from the request's cookie environment; when CSRF protection is enabled it
# only answers if the session's SynoToken is also supplied (the UI sends it
# on every request via the query string or the X-Syno-Token header).

MODULES="${DUPFINDER_MODULES:-/usr/syno/synoman/webman/modules}"
AUTH="$MODULES/authenticate.cgi"

deny() {
	printf 'Status: %s\r\nContent-Type: application/json\r\n\r\n{"error":"%s"}' "$1" "$2"
	exit 0
}

# --- locate this package ------------------------------------------------
# DSM keys every package path on the package id, and the id differs between
# builds: the hand-built spk installs as DuplicateFinder, the SynoCommunity
# build as duplicatefinder. Nothing here hardcodes one. DSM executes this
# file in place, so its own resolved location names the id: the package
# lives at /volumeN/@appstore/<id>, and both /var/packages/<id>/target and
# the /webman/3rdparty/<id> entry are symlinks into it, so however the web
# server addressed this file it resolves to <id>/ui/api.cgi. SCRIPT_FILENAME
# and SCRIPT_NAME cover a server that invoked it some other way. A request
# that reveals no id is refused, never guessed: the id chooses which binary
# runs and which token file it reads.
pkg_id_from_path() {
	case "$1" in
		*/ui/api.cgi) p="${1%/ui/api.cgi}"; printf '%s' "${p##*/}" ;;
	esac
}
PKG_ID="$(pkg_id_from_path "$(readlink -f "$0" 2>/dev/null)")"
PKG_ID_SOURCE="argv0"
if [ -z "$PKG_ID" ] && [ -n "$SCRIPT_FILENAME" ]; then
	PKG_ID="$(pkg_id_from_path "$(readlink -f "$SCRIPT_FILENAME" 2>/dev/null)")"
	PKG_ID_SOURCE="script_filename"
fi
if [ -z "$PKG_ID" ]; then
	case "$SCRIPT_NAME" in
		/webman/3rdparty/*/api.cgi*)
			p="${SCRIPT_NAME#/webman/3rdparty/}"
			PKG_ID="${p%%/*}"
			PKG_ID_SOURCE="script_name"
			;;
	esac
fi
# The id is spliced into filesystem paths below: only the characters DSM
# allows in a package name may pass, and the package must really be there.
case "$PKG_ID" in
	""|.|..|*[!A-Za-z0-9._-]*) PKG_ID="" ;;
esac
if [ -z "$PKG_ID" ] || [ ! -x "/var/packages/$PKG_ID/target/bin/dupfinder" ]; then
	deny "500 Internal Server Error" "Cannot locate the Duplicate Finder package"
fi
PKG_DEST="/var/packages/$PKG_ID/target"

# --- recover the session's SynoToken -----------------------------------
# The query-string value is URL-encoded; authenticate.cgi expects the raw
# token, so decode %XX escapes (and '+' → space) before validating with it.
urldecode() {
	printf '%b' "$(printf '%s' "$1" | sed -e 's/+/ /g' -e 's/%\([0-9a-fA-F][0-9a-fA-F]\)/\\x\1/g')"
}
TOKEN=""
TOKEN_SENT=0
# The parameter is matched as a whole name at the start of the string or
# after '&' — never as a substring — and the FIRST occurrence wins, so a
# second empty "SynoToken=" cannot blank out the real one.
case "&$QUERY_STRING" in
	*\&SynoToken=*)
		TOKEN_SENT=1
		TOKEN="$(urldecode "$(printf '&%s' "$QUERY_STRING" | sed -n 's/^.*[&]SynoToken=\([^&]*\).*$/\1/p' | head -n 1)")"
		;;
esac
if [ -n "$HTTP_X_SYNO_TOKEN" ]; then
	TOKEN_SENT=1
	[ -z "$TOKEN" ] && TOKEN="$HTTP_X_SYNO_TOKEN"
fi

# --- resolve the logged-in user -----------------------------------------
# Fail closed on the token: when the request carries a SynoToken, it must
# validate — a bad or expired token never falls back to cookie-only auth.
# Cookie-only validation is reserved for requests that carry no token at
# all (DSM configurations with CSRF protection disabled send none).
# A request that NAMED a token but sent an empty one is not a token-less
# request: it fails closed too, rather than sliding into the cookie-only
# branch. Both invocations get /dev/null for stdin — the request body is
# for the daemon, and an authenticate.cgi that peeked at it on a POST would
# leave the proxied request with a truncated body.
USER=""
if [ -x "$AUTH" ]; then
	if [ -n "$TOKEN" ]; then
		USER="$(QUERY_STRING="SynoToken=$TOKEN" REQUEST_METHOD=GET \
			HTTP_X_SYNO_TOKEN="$TOKEN" "$AUTH" 2>/dev/null </dev/null | head -n 1 | tr -d '\r\n')"
	elif [ "$TOKEN_SENT" = 0 ]; then
		USER="$(REQUEST_METHOD=GET CONTENT_LENGTH= "$AUTH" 2>/dev/null </dev/null | head -n 1 | tr -d '\r\n')"
	fi
fi

# DSM's administrators group ("administrators", GID 101) is the platform's
# stable convention; authenticate.cgi yields only the username, so group
# membership is resolved locally. "--" keeps an unusual username from ever
# being parsed as an option.
# Group NAMES are never parsed out of `id -Gn`: DSM allows interior spaces in
# a group name, and `tr ' ' '\n'` cannot tell a separator from one of those —
# so a member of a group merely NAMED "backup administrators" matched the
# orphaned "administrators" line and was granted full access. Both branches
# below compare GIDs, which are unambiguous: the conventional 101 first, then
# whatever GID the administrators group actually has on this DSM.
is_admin() {
	id -G -- "$1" 2>/dev/null | tr ' ' '\n' | grep -qx "101" && return 0
	# getent first so a directory-provided (LDAP/AD) administrators group is
	# resolved too, /etc/group as the fallback when getent is absent.
	agid="$(getent group administrators 2>/dev/null | awk -F: '{print $3; exit}')"
	[ -n "$agid" ] || agid="$(awk -F: '$1=="administrators"{print $3; exit}' /etc/group 2>/dev/null)"
	[ -n "$agid" ] && id -G -- "$1" 2>/dev/null | tr ' ' '\n' | grep -qx "$agid"
}

# --- gate ---------------------------------------------------------------
if [ ! -x "$AUTH" ]; then
	deny "403 Forbidden" "DSM authentication module unavailable"
fi
if [ -z "$USER" ]; then
	deny "401 Unauthorized" "Please sign in to DSM"
fi
if ! is_admin "$USER"; then
	deny "403 Forbidden" "Administrator privileges required"
fi

# --- diagnostics (deliberately below the gate: admins only) --------------
if [ "${PATH_INFO:-}" = "/debug" ]; then
	CGIUSER="$(id -un 2>/dev/null)"
	COOKIE=false; [ -n "$HTTP_COOKIE" ] && COOKIE=true
	SESSION=false; case "$HTTP_COOKIE" in *id=*) SESSION=true;; esac
	AUTH_X=false; [ -x "$AUTH" ] && AUTH_X=true
	TOKEN_OK=false; [ -n "$TOKEN" ] && TOKEN_OK=true
	SAFE_USER="$(printf '%s' "$USER" | tr -cd 'A-Za-z0-9._@ -')"
	printf 'Content-Type: application/json\r\n\r\n'
	printf '{"cgiUser":"%s","cookiePresent":%s,"sessionCookie":%s,"authCgiExec":%s,"tokenFound":%s,"authUser":"%s","isAdmin":true,"pkgId":"%s","pkgIdSource":"%s"}' \
		"$CGIUSER" "$COOKIE" "$SESSION" "$AUTH_X" "$TOKEN_OK" "$SAFE_USER" "$PKG_ID" "$PKG_ID_SOURCE"
	exit 0
fi

# Where the daemon keeps its shared auth token; the CGI proxy reads it and
# attaches it to every request it forwards to the daemon.
DUPFINDER_VAR="/var/packages/$PKG_ID/var"
export DUPFINDER_VAR

# One spk per architecture ships exactly one binary (see build.sh).
exec "$PKG_DEST/bin/dupfinder" -mode cgi
