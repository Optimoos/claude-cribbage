#!/bin/sh
# Builds the web client for hosting apart from the game server, on
# Cloudflare Pages (see deploy/README.md): copies static/ to dist/, points
# the page at the server, and writes the headers the server would
# otherwise send (_headers, which Pages reads).
#
# CRIBBAGE_SERVER is the game server's address, e.g.
# https://api.cribbage.example.com. Locally:
#   CRIBBAGE_SERVER=https://api.cribbage.example.com sh pages-build.sh
set -eu
cd "$(dirname "$0")"

server=${CRIBBAGE_SERVER:-}
server=${server%/}
case $server in
https://*) ;;
*)
	echo "pages-build.sh: set CRIBBAGE_SERVER to the game server's https:// address, e.g. https://api.cribbage.example.com" >&2
	exit 1
	;;
esac
host=${server#https://}
case $host in
"" | */* | *[!A-Za-z0-9.:-]*)
	echo "pages-build.sh: CRIBBAGE_SERVER must be https://<host name>, with no path: $server" >&2
	exit 1
	;;
esac

rm -rf dist
cp -R static dist
sed 's|<meta name="cribbage-server" content="">|<meta name="cribbage-server" content="'"$server"'/">|' static/index.html >dist/index.html
grep -q "content=\"$server/\"" dist/index.html || {
	echo "pages-build.sh: no cribbage-server meta tag in static/index.html" >&2
	exit 1
}

# The Content-Security-Policy is the server's (csp in web.go), except that
# the page talks to the game server rather than to its own host.
cat >dist/_headers <<EOF
/*
  Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src $server wss://$host; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
  Cache-Control: no-cache
  Strict-Transport-Security: max-age=31536000
  X-Content-Type-Options: nosniff
  Referrer-Policy: no-referrer
EOF

echo "built dist/ for $server"
