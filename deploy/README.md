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

Leave ports 8080 and 7777 closed: they listen on localhost only. Port 7777
is for headless bots (`cmd/botclient`), which run on the server itself;
its protocol is not encrypted.

## Updating

Games survive restarts: every move is committed before anyone sees it, and
clients reconnect and resume by themselves within a few seconds.

```sh
scp cribserver root@SERVER:/tmp/
ssh root@SERVER 'install -m 0755 /tmp/cribserver /opt/cribbage/ && systemctl restart cribbage'
```

## Backups

The server writes a compacted copy of the database to
`/var/lib/cribbage/backups/cribbage-<UTC time>.db` at startup and every
24 hours, keeping the newest 14 (`-backup-dir`, `-backup-every`,
`-backup-keep`). Writes pause while a backup runs; for a database of tens
of MB that is well under a second.

Backups on the same disk do not survive losing the machine. Copy them off
it, for example with a nightly cron job on another machine:

```sh
rsync -a root@SERVER:/var/lib/cribbage/backups/ ~/cribbage-backups/
```

or with your provider's volume snapshots. The files contain password
hashes and session tokens; keep them private.

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

## Login limits

Remote clients get 20 login or registration attempts, then 10 a minute;
5 registrations per address, then 5 an hour; and 10 failed logins per
username, then 1 a minute. Over the limit, the server answers "too many
attempts; try again later". The address is the one WebRTC packets come
from, so it is the player's real address even though signaling passes
through Caddy. Connections from localhost (bots on the server) are not
limited. At most one bcrypt hash per CPU runs at a time, so a flood of
logins cannot slow the games.

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
