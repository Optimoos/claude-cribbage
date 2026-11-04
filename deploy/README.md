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
| `cribserver.conf` | `/etc/cribbage/cribserver.conf` (the server's settings) |
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

# Settings, then the service. Edit cribserver.conf first (your domain and
# mail account) and create the secrets: see "Signing in and email" below.
# The service does not start without them.
install -d -m 0755 /etc/cribbage
install -m 0644 cribserver.conf /etc/cribbage/
cp cribbage.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now cribbage
journalctl -u cribbage -f          # "email enabled", "web client" and "database backed up" lines

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
set `public-ip = <elastic/static IP>` in `cribserver.conf`, and open the same
ports in the cloud's security group.

Leave ports 8080 and 7777 closed: they listen on localhost only (the
default). Port 7777 is for headless bots (`cmd/botclient`), which run on
the server itself; its protocol is not encrypted.

`rtc-public-only` makes the server ignore WebRTC addresses a client
offers on private, loopback or link-local networks, so nobody can make it
send packets into its own network. Leave it off to play on one machine or
a LAN.

## Signing in and email

There are no passwords. Players sign in with an 8-digit code the server
emails them (the same email has a link that signs in the browser it is
opened in), with a passkey in the web client, or, in the desktop app, by
approving the code it shows from a device that is already signed in
("Sign in with browser"). An account needs an email address; family
members can share one, and then the email has a code for each account.
Sessions last 90 days from the last time the device was used.

Codes work for 15 minutes. Five wrong codes end a sign-in, and ten wrong
codes for an account, from anywhere, stop its codes working for a while
(one more an hour); the link still works. Each account gets 10 emails,
then 1 every 5 minutes, and each address 20, then 1 every 5 minutes. The
reply to "email me a code" is the same whether or not the account exists.

So email must work before anyone can play. You need an SMTP account that
may send from your domain, for example Postmark, Resend, Amazon SES, or
your mailbox provider's SMTP login. Set up SPF and DKIM for the sender
domain as the provider describes, or the codes land in spam. Then:

```sh
install -m 0600 /dev/stdin /etc/cribbage/smtp-password <<< 'the-smtp-password'
openssl rand -hex 32 | install -m 0600 /dev/stdin /etc/cribbage/email-key
```

Email addresses are stored encrypted with that key (AES-256-GCM), so the
database and its backups reveal none without it. **Back the key up
somewhere safe, apart from the database backups.** Without it nobody can
sign in by email until you set each account's address again (below), and
the server refuses to start with a different key than the one the
database was first used with.

Put your values in `/etc/cribbage/cribserver.conf` (`public-url`, `smtp`,
`smtp-user`, `mail-from`), then run
`systemctl daemon-reload && systemctl restart cribbage`. The log says
`email enabled`. Port 465 uses TLS from the start; other ports must offer
STARTTLS, and the server refuses to send without it. `public-url` must be
the HTTPS address players use: the links point there, and passkeys are
tied to its host name, so changing the domain later makes existing
passkeys stop working (players sign in by email and add new ones).

Without `smtp`, the server writes every email, codes included, to its log
instead. That is only for testing on your own machine: nobody else can
sign in.

**Changing an address** takes two links: one sent to the current address
(so someone using a device left signed in cannot take the account), then
one to the new address. **Account → Sign out other devices** ends every
other session.

**When someone cannot sign in** (they lost access to their email, or
someone else changed it), check that it is really them, then on the
server:

```sh
sudo cribserver admin -socket /run/cribbage/admin.sock set-email <username> <address>
```

Codes then go to the new address, every device is signed out, and the
account's passkeys are removed, since whoever took the account may have
added one; the old address is told. The command talks to the running
server through `/run/cribbage/admin.sock`, which only root and the
`cribbage` user can open (`/opt/cribbage/cribserver` if it is not on your
PATH).

**Bots.** Headless clients (`cmd/botclient`) cannot read email. They sign
in to bot accounts, which have no address, with a shared secret: create
`/etc/cribbage/bot-key` (`openssl rand -hex 16`), uncomment `bot-key-file`
in `cribserver.conf` and its `LoadCredential=` line in `cribbage.service`,
and run `botclient -key-file`. Without a bot key, bots cannot sign in.
The bot key cannot sign in to a person's account.

## Deleting accounts

Players delete their own accounts under **Account**. A link is mailed to
their address (it works for an hour) and the account goes only when they
open it and confirm. (A bot account, which has no address, is deleted
when its username is typed again.) Deleting ends their unfinished games (resigning those
in progress), removes the account, its address and invites, and frees the
username. Their finished games stay in their opponents' history as
"Deleted player". To delete an account yourself:

```sh
sudo cribserver admin -socket /run/cribbage/admin.sock delete-account <username>
```

It asks you to type the username again (`-yes` skips that). A deleted
account stays in database backups until they rotate out (a week by
default), with its address encrypted.

## Privacy and bug reports

The web client links to a privacy page (`/privacy.html`, also linked from
the Godot client) saying what the server keeps. It is written for this
server as configured here; if you change what is kept (access logs in
Caddy, a third-party STUN or TURN server, longer backups), update
`server/web/static/privacy.html`. Both clients have a "Report a bug" link,
which goes to `report-url` in `cribserver.conf`.

## Updating

Clients carry a protocol version, and the server refuses clients built for
another one with a message to update. The web client is served by the
server, so it reloads itself; Godot builds must be replaced. Bump
`PROTOCOL_VERSION_CURRENT` in `proto/cribbage/v1/cribbage.proto` (and
`PROTOCOL_VERSION` in `app.js`) when a change would break older clients.

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

or with your provider's volume snapshots. Keep them private, although
there is little in them to use: there are no passwords, addresses are
encrypted, and session tokens are stored hashed, so a backup cannot be
used to sign in as anyone.

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

**Signing in.** Remote clients get 20 sign-in attempts (codes, links,
passkeys), then 10 a minute; 10 emailed codes per address, then 1 a
minute; and 5 registrations, then 5 an hour. Wrong codes and emails are
also limited per account and per email address (see "Signing in and
email"). IPv6 addresses count by their /64. The address is the one
WebRTC packets come from, so it is the player's real address even though
signaling passes through Caddy. Connections from localhost (bots on the
server) are not limited.

**Connections.** At most 20 per address and 5,000 in all; one that has
not signed in within 30 seconds is closed, or within 20 minutes once it
has asked for a code (time to read the email). Messages over 64 KB are
refused. WebRTC connection setups: 10 at once per address and 200 in all,
each with at most 32 network addresses. HTTP requests time out after 30
seconds.

**Requests and CPU.** Once signed in, a player may make 60 requests at
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
