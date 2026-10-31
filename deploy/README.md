# Deploying the cribbage server

One small Linux VPS runs everything: the `cribserver` binary (game server,
web client, WebRTC) behind [Caddy](https://caddyserver.com) for HTTPS.

```
browser / Godot ──HTTPS, WSS (443)──► Caddy ──► 127.0.0.1:8080 cribserver
                ──WebRTC (UDP 7778)───────────►                cribserver
```

Why this shape:
- **One instance only.** Games live in memory in one process, and SQLite
  is opened exclusively. Do not run replicas or put it behind an
  autoscaler.
- **UDP 7778 must reach the server directly.** App platforms that only
  route HTTP will not work.
- **Persistent disk** for the database in `/var/lib/cribbage`.

A 2 vCPU / 2–4 GB machine with a public IPv4 address (Hetzner Cloud,
DigitalOcean, Linode, Vultr) is plenty; the expert bot's search is the
main CPU cost.

## Files

| File | Install as |
|---|---|
| `cribbage.service` | `/etc/systemd/system/cribbage.service` |
| `Caddyfile` | `/etc/caddy/Caddyfile` (edit the domain) |

## First install

On your machine, build a static Linux binary (pure Go, no cgo; the web
client is embedded):

```sh
cd server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o cribserver ./cmd/cribserver
scp cribserver models-expert/expert.bin root@SERVER:/tmp/
```

On the server (Debian/Ubuntu shown):

```sh
# A user with no login shell, and its directories.
useradd --system --home /var/lib/cribbage --shell /usr/sbin/nologin cribbage
install -d -o cribbage -g cribbage -m 0700 /var/lib/cribbage
install -d -m 0755 /opt/cribbage
install -m 0755 /tmp/cribserver /opt/cribbage/
install -m 0644 /tmp/expert.bin /opt/cribbage/

# The service.
cp cribbage.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now cribbage
journalctl -u cribbage -f          # "web client" and "database backed up" lines

# Caddy (https://caddyserver.com/docs/install), then:
cp Caddyfile /etc/caddy/Caddyfile  # with your domain
systemctl reload caddy

# Firewall: SSH, HTTP (certificate challenges and redirect), HTTPS, WebRTC.
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 7778/udp
ufw enable
```

Point the domain's DNS at the server before reloading Caddy, so it can get
a certificate. Then browse to `https://cribbage.example.com`.

On AWS, GCP or Azure the public IP is NATed rather than on the interface:
add `-public-ip <elastic/static IP>` to `ExecStart`, and open the same
ports in the cloud's security group.

Leave ports 8080 and 7777 closed: they listen on localhost only (the
default). Port 7777 is for headless bots (`cmd/botclient`), which run on
the server itself; its protocol is not encrypted.

`-rtc-public-only` makes the server ignore WebRTC addresses a client
offers on private, loopback or link-local networks, so nobody can make it
send packets into its own network. Leave it off to play on one machine or
a LAN.

## Updating

Games survive restarts: every move is committed before anyone sees it, and
clients reconnect and resume by themselves within a few seconds.

```sh
scp cribserver root@SERVER:/tmp/
ssh root@SERVER 'install -m 0755 /tmp/cribserver /opt/cribbage/ && systemctl restart cribbage'
```

## Backups

The server writes a compacted copy of the database to
`/var/lib/cribbage/backups/cribbage-<UTC time>.db` every 24 hours, keeping
the newest 7 (`-backup-dir`, `-backup-every`, `-backup-keep`). At startup
it writes one only if the newest is a day old, so restarts (or a crash
loop) do not pile them up. Writes pause while a backup runs; for a database of tens
of MB that is well under a second.

Backups on the same disk do not survive losing the machine. Copy them off
it, for example with a nightly cron job on another machine:

```sh
rsync -a root@SERVER:/var/lib/cribbage/backups/ ~/cribbage-backups/
```

or with your provider's volume snapshots. The files contain password
hashes; keep them private. Session tokens are stored hashed, so a backup
cannot be used to sign in as anyone.

You cannot copy the live `cribbage.db` with `sqlite3 .backup` or
Litestream while the server runs: it holds an exclusive lock.

To restore:

```sh
systemctl stop cribbage
cd /var/lib/cribbage
mv cribbage.db cribbage.db.broken; rm -f cribbage.db-wal cribbage.db-shm
cp backups/cribbage-YYYYMMDD-HHMMSS.db cribbage.db
chown cribbage:cribbage cribbage.db
systemctl start cribbage
```

## Limits

All built in; the numbers are in `internal/server` (`DefaultAuthLimits`,
`DefaultGameLimits`).

**Logins.** Remote clients get 20 login or registration attempts, then 10
a minute; 5 registrations per address, then 5 an hour; and 10 failed
logins per username *from one address*, then 1 a minute, so a stranger's
wrong guesses cannot lock a player out. IPv6 addresses count by their /64.
The address is the one WebRTC packets come from, so it is the player's
real address even though signaling passes through Caddy. Connections from
localhost (bots on the server) are not limited. Password hashing uses all
but one CPU at most, so a flood of logins cannot stall the games.

**Connections.** At most 20 per address and 5,000 in all; one that has
not logged in within 30 seconds is closed. Messages over 64 KB are
refused. WebRTC connection setups: 10 at once per address and 200 in all,
each with at most 32 network addresses. HTTP requests time out after 30
seconds.

**Requests and CPU.** Once logged in, a player may make 60 requests at
once, then 10 a second, across all their connections. Bot moves are
computed on all but one CPU at most, so bot games played as fast as a
script can go cannot slow everyone else.

**Sessions.** Logging out ends the session on the server, and expired
sessions are deleted hourly.

**Games and invites, per player.**
- 20 unfinished games, of which 5 against the computer, and 1 open game.
  Waiting "Find a match" requests count as games.
- 5 invites sent and awaiting an answer; 20 waiting for one player's
  answer; one at a time between two players. Invites and open games
  nobody takes up are withdrawn after 48 hours.
- A game between people with no move for 60 days, or against the computer
  for 30, ends unfinished.

**Memory.** A game nobody has touched for 10 minutes goes to sleep: it
leaves memory and is rebuilt from the database the moment anyone opens it
or moves. Slow games, such as a move every few days, cost only their
database rows, so memory follows how many people are playing right now.

**Web page.** The server sends a Content-Security-Policy allowing only the
page's own script, stylesheet and server, so injected markup could not run
script.

## TURN (optional)

A server with a public IP is reachable from nearly every home network, so
start without TURN. If players on networks that block UDP (some offices
and schools) cannot connect, run [coturn](https://github.com/coturn/coturn)
on this machine on port 3478 (and 5349 for TLS; 443 belongs to Caddy), open
those ports, and pass `-ice-servers turn:cribbage.example.com:3478` (with
credentials as coturn is configured); browsers get the list from
`/api/config`.

## Desktop clients

`client-godot/export.sh` builds Linux and Windows clients. Players enter
`wss://cribbage.example.com/rtc` as the server on the login screen (it is
remembered); the default points at a local server.
