# Deploying the cribbage server

One small Linux VPS runs the `cribserver` binary (game server, WebRTC),
which serves HTTPS itself with a Let's Encrypt certificate. The web page is
static files on [Cloudflare Pages](https://pages.cloudflare.com), so it
loads even while the server is down, and says so.

```
browser ──HTTPS──────────────────────► Cloudflare Pages  cribbage.example.com
browser / Godot ──HTTPS, WSS (443)───► cribserver        api.cribbage.example.com
                ──WebRTC (UDP 7778)──► cribserver
```

Why this shape:
- **One instance only.** Games live in memory in one process, and SQLite
  is opened exclusively. Do not run replicas or put it behind an
  autoscaler.
- **UDP 7778 must reach the server directly.** App platforms that only
  route HTTP will not work.
- **Persistent disk** for the database in `/var/lib/cribbage`.

Any small machine with a public IPv4 address will do (Hetzner Cloud,
DigitalOcean, Linode, Vultr, AWS). The server uses about 50 MB idle, so
1 GB of memory is enough for family and friends; 2 vCPU / 2–4 GB is room
for many games at once. The expert bot's search is the main CPU cost, in
short bursts that burstable instances handle well. For AWS, see "AWS: a
Spot instance" below.

## Files

| File | Install as |
|---|---|
| `cribserver.conf` | `/etc/cribbage/cribserver.conf` (the server's settings) |
| `cribbage.service` | `/etc/systemd/system/cribbage.service` |
| `cribbage-s3-backup` | `/opt/cribbage/cribbage-s3-backup` (optional: copies backups to S3) |
| `cribbage-s3-backup.service`, `.timer` | `/etc/systemd/system/` (with the above) |
| `cribbage-dns` | `/opt/cribbage/cribbage-dns` (optional: keeps the DNS record on the address, with Cloudflare) |
| `cribbage-dns.service`, `.timer` | `/etc/systemd/system/` (with the above) |

## First install

On your machine, build a static Linux binary (pure Go, no cgo; it embeds
a copy of the web client too, for testing on a LAN):

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

# Firewall: SSH, HTTPS, WebRTC.
ufw allow OpenSSH
ufw allow 443/tcp
ufw allow 7778/udp
ufw enable
```

Point `api.cribbage.example.com` (`tls-domain` in `cribserver.conf`) at
the server with an A record before starting it. On Cloudflare DNS, make it
**DNS only** (grey cloud), not proxied: the server gets its own
certificate on port 443, which a proxy would intercept. The first request
to `https://api.cribbage.example.com/api/config` fetches the certificate,
taking a few seconds; it is kept in `/var/lib/cribbage/certs` and renewed
by itself. Then set up the web page: see "The web page on Cloudflare
Pages" below.

On AWS, GCP or Azure the public IP is NATed rather than on the interface:
set `public-ip = <elastic/static IP>` in `cribserver.conf`, and open the same
ports in the cloud's security group.

Leave port 7777 closed: it listens on localhost only (the default). Port 7777 is for headless bots (`cmd/botclient`), which run on
the server itself; its protocol is not encrypted.

`rtc-public-only` makes the server ignore WebRTC addresses a client
offers on private, loopback or link-local networks, so nobody can make it
send packets into its own network. Leave it off to play on one machine or
a LAN.

## The web page on Cloudflare Pages

The web client is static files in `server/web/static`. Pages builds it
straight from this repository with `server/web/pages-build.sh`, which
points the page at the server (`CRIBBAGE_SERVER`) and writes its
Content-Security-Policy and other headers to `_headers`.

In the Cloudflare dashboard, Workers & Pages → Create → Pages → connect
to Git, choose the repository, and set:

| Setting | Value |
|---|---|
| Production branch | `pages` (see "Updating") |
| Build command | `sh pages-build.sh` |
| Build output directory | `dist` |
| Root directory | `server/web` |
| Environment variable | `CRIBBAGE_SERVER` = `https://api.cribbage.example.com` |

Create the `pages` branch first (`git push origin main:pages`). After
the first deploy, under the project's Settings → Builds, set preview
branches to **None**, so pushes to other branches do not publish clients
that may not match the server. Then add `cribbage.example.com` under
Custom domains. That is the address players use, and `public-url` in
`cribserver.conf`: emailed links open there, and passkeys belong to it.

The server still serves its embedded copy of the page at
`https://api.cribbage.example.com`, but players should not use it:
passkeys do not work there, and it goes down with the server.

To check a build on your machine:

```sh
cd server/web && CRIBBAGE_SERVER=https://api.cribbage.example.com sh pages-build.sh
```

When the page cannot reach the server for about six seconds, it shows
"Can't reach the server" over whatever was on screen, and keeps trying;
it carries on when the server is back.

## Signing in and email

There are no passwords. Players sign in with an 8-digit code the server
emails them (the same email has a link that signs in the browser it is
opened in), with a passkey in the web client, or, in the desktop app, by
approving the code it shows from a device that is already signed in
("Sign in with browser"). There is no separate sign-up: someone who
enters an address with no account gets a code that creates one, and
chooses a username once it is entered. An account needs an email address; family
members can share one, and then the email has a code for each account.
Sessions last 90 days from the last time the device was used.

Codes work for 15 minutes. Five wrong codes end a sign-in, and ten wrong
codes for an account, from anywhere, stop its codes working for a while
(one more an hour); the link still works. Each account gets 10 emails,
then 1 every 5 minutes, and each address 20, then 1 every 5 minutes;
addresses that differ only by a `+tag` (and, at Gmail, by dots) count as
one, so they cannot be used to flood an inbox. A new-account code goes
to an address nobody has yet shown is theirs, so the server sends at
most 20 emails with only a new-account code, then 1 every 15 minutes,
however many places the requests come from (an address with accounts
gets a new-account code in its sign-in email, which costs nothing
extra). The reply to "email me a code" is the same whether or not the
account exists, so the page cannot be used to find out who plays.

**Stopping email.** Every email ends with a link (and a one-click
`List-Unsubscribe` header, which mail apps show as "Unsubscribe") that
stops all email to its address and its `+tag` variants: for someone who
got codes they did not ask for. It deletes nothing and signs nobody out;
an account whose address is stopped can still sign in with a passkey or a
signed-in device. The server keeps a keyed hash of the address, not the
address. Only you can undo it, once you know the address is theirs:

```sh
sudo cribserver admin -socket /run/cribbage/admin.sock allow-email <address>
```

**Bounces and complaints.** With Amazon SES, turn on the account-level
suppression list, so SES itself stops sending to addresses that bounce or
mark an email as spam (from CloudShell, in the SES region):

```sh
aws sesv2 put-account-suppression-attributes --suppressed-reasons BOUNCE COMPLAINT
aws sesv2 get-account --query SuppressionAttributes   # check
```

**The log** says `sign-in code asked for` with how many accounts the name
matched (never the name), `email sent` with the kind of email, and why an
email was not sent (`email not sent: ...`); a failed send logs the mail
server's error. `journalctl -u cribbage | grep -E "sign-in|email"`
shows them.

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
server as configured here, the page on Cloudflare Pages included; if you
change what is kept (access logs, a third-party STUN or TURN server,
longer backups), update
`server/web/static/privacy.html`. Both clients have a "Report a bug" link,
which goes to `report-url` in `cribserver.conf`.

## Updating

Clients carry a protocol version, and the server refuses clients built for
another one with a message to update. The web client reloads itself once
when that happens, so update the server first and the page right after;
Godot builds must be replaced. Bump
`PROTOCOL_VERSION_CURRENT` in `proto/cribbage/v1/cribbage.proto` (and
`PROTOCOL_VERSION` in `app.js`) when a change would break older clients.

Games survive restarts: every move is committed before anyone sees it, and
clients reconnect and resume by themselves within a few seconds.

```sh
scp cribserver root@SERVER:/tmp/
ssh root@SERVER 'install -m 0755 /tmp/cribserver /opt/cribbage/ && systemctl restart cribbage'
git push origin main:pages      # then the web page (Pages builds it in a minute or so)
```

Pages deploys only what is pushed to `pages`, so commits on `main` reach
players when you deploy, not before.

Pages left open notice a new page (each build writes its commit to
`version.json`; open pages check it every 10 minutes, when shown again,
and after reconnecting) and offer "Reload", reloading by themselves only
in a tab nobody is looking at and with no sign-in under way. Games are on
the server, so reloading loses nothing. A page the server refuses as too
old says it is updating, then reloads.

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

with your provider's volume snapshots, or on AWS with the S3 job in "AWS:
a Spot instance" below. Keep them private, although
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

## AWS: a Spot instance

A `t4g.micro` (2 vCPU, 1 GB, ARM) costs a fraction of its on-demand price
as a Spot instance. AWS can take Spot capacity back with two minutes'
warning. Set up as below, it then shuts down cleanly and starts again when
capacity returns, so the cost is a gap in service, usually minutes and
rarely hours, not lost data.

The server is built for that. It saves every move before anyone sees it,
and on shutdown it finishes what it is doing and closes the database;
`Restart=always` brings it back after a crash. A `kill -9` in the middle of
20 live games lost none: all came back, and clients reconnected by
themselves. An interruption loses only what is held in memory: sign-in
codes and device approvals not yet used (players ask for another) and
connections (clients reconnect, or players reload the page). Accounts,
sessions, passkeys, invites and games are on disk.

What decides whether data survives is the disk, so all of the settings
below matter.

### 1. Build for ARM

Graviton (`t4g`) instances are ARM, so build with `GOARCH=arm64` instead
of `amd64` in "First install".

### 2. Bucket and role (for the S3 backup job)

The server's backups are on its own disk; copy them to S3 so that losing
the disk loses nothing. Run this once from your machine with the AWS CLI
(replace the bucket name, which must be unique across all of S3, and the
region):

```sh
B=cribbage-backups-SOMETHING-UNIQUE R=us-east-1
aws s3api create-bucket --bucket $B --region $R \
  --create-bucket-configuration LocationConstraint=$R   # omit this line for us-east-1
aws s3api put-bucket-versioning --bucket $B --versioning-configuration Status=Enabled
aws s3api put-bucket-lifecycle-configuration --bucket $B --lifecycle-configuration '{"Rules":[{
  "ID":"expire","Status":"Enabled","Filter":{"Prefix":"backups/"},
  "Expiration":{"Days":7},"NoncurrentVersionExpiration":{"NoncurrentDays":1},
  "AbortIncompleteMultipartUpload":{"DaysAfterInitiation":1}}]}'

# A role the instance runs as, which may only add files under backups/.
aws iam create-role --role-name cribbage-server --assume-role-policy-document \
  '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
aws iam put-role-policy --role-name cribbage-server --policy-name s3-backup --policy-document \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":[\"s3:PutObject\",\"s3:AbortMultipartUpload\"],\"Resource\":\"arn:aws:s3:::$B/backups/*\"}]}"
aws iam create-instance-profile --instance-profile-name cribbage-server
aws iam add-role-to-instance-profile --instance-profile-name cribbage-server --role-name cribbage-server
```

The server can add backups but cannot list, read or delete them. If it is
broken into, the copies in S3 stay out of reach, and versioning keeps the
original if someone overwrites one. New buckets block public access and
encrypt at rest by default.

S3 keeps each backup for 7 days, matching what the privacy page promises
about deleted accounts. If you keep them longer, change the sentence about
backups in `server/web/static/privacy.html` too. A week of backups of a
small database costs well under a cent a month.

### 3. Launch the instance

In the EC2 console, under **Advanced details**:

- **Purchasing option:** Spot instances, then Customize. Set **Request
  type** to *Persistent* and **Interruption behavior** to *Stop*.
  *Terminate* (the default) deletes the instance and its disk, database
  included. *Stop* keeps the disk, and with a persistent request AWS starts
  the instance again when capacity returns.
- **IAM instance profile:** `cribbage-server`.
- **Metadata version:** V2 only.

Under **Storage**, set the root volume to 8 GB gp3 and **Delete on
termination** to *No*. The disk then survives even if the instance is
terminated by mistake.

Use a Debian 13 or Ubuntu 24.04 arm64 image. The security group should
allow TCP 22 (from your IP only), TCP 443, and UDP 7778.

The same with the CLI (find the image ID and root device name for your
region in the console; Debian's is `/dev/xvda`, Ubuntu's `/dev/sda1`):

```sh
aws ec2 run-instances --instance-type t4g.micro --image-id ami-... \
  --key-name YOUR-KEY --security-group-ids sg-... \
  --iam-instance-profile Name=cribbage-server \
  --metadata-options HttpTokens=required \
  --instance-market-options 'MarketType=spot,SpotOptions={SpotInstanceType=persistent,InstanceInterruptionBehavior=stop}' \
  --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=8,VolumeType=gp3,DeleteOnTermination=false}'
```

### 4. A fixed address

The public IP changes every time an instance stops and starts, which would
break DNS and WebRTC. Allocate an **Elastic IP** and associate it with the
instance; it stays through stops. Point `api.cribbage.example.com` at it and set
`public-ip = <the Elastic IP>` in `cribserver.conf`. (Every public IPv4
address on AWS is billed hourly, about $3.60 a month, and an Elastic IP is
billed while the instance is stopped too.)

**Checking it (Cloudflare DNS).** `cribbage-dns` checks, at boot (before
the game server starts) and hourly, that the A record for
`api.cribbage.example.com` points at the instance's public address (from
the EC2 metadata service) and is DNS only, and that `public-ip` in
`cribserver.conf` matches; it fixes whichever is wrong, restarting the
game server if `public-ip` changed. With an Elastic IP it should only ever
confirm; it catches a record edited by mistake, or an address that
changed because the Elastic IP was released or moved.

It needs a Cloudflare API token that can edit DNS in that zone and
nothing else: in the Cloudflare dashboard, My Profile → API Tokens →
Create Token → the "Edit zone DNS" template, with Zone Resources set to
Include → Specific zone → your domain. Copy the token, and the zone's ID
(the domain's Overview page, "Zone ID", lower right). On the server:

```sh
install -m 0600 /dev/stdin /etc/cribbage/cloudflare-token <<< 'the-token'
install -m 0755 cribbage-dns /opt/cribbage/
cp cribbage-dns.service cribbage-dns.timer /etc/systemd/system/
# Edit CLOUDFLARE_ZONE_ID and CRIBBAGE_DNS_NAME in the .service file, then:
systemctl daemon-reload
systemctl enable cribbage-dns.service cribbage-dns.timer
systemctl start cribbage-dns.timer
systemctl start cribbage-dns      # once now, to check it
journalctl -u cribbage-dns        # "api... already points at 203.0.113.10"
```

If it cannot reach Cloudflare at boot, the game server starts anyway, and
the next hourly run tries again.

### 5. Install

Follow "First install" and "Signing in and email". AWS images do not
allow root to log in: SSH as `admin` (Debian) or `ubuntu` (Ubuntu), copy
files to its home instead of `root@SERVER:/tmp/`, and run the commands
with `sudo`. Amazon SES works as the mail account. Then:

**Fit the server to 1 GB.** `cribbage.service` is sized for 2 GB. Override
it with `systemctl edit cribbage` and these lines:

```ini
[Service]
Environment=GOMEMLIMIT=550MiB
MemoryHigh=600M
MemoryMax=700M
```

Then add swap as a cushion; cloud images come without it:

```sh
fallocate -l 1G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab
```

**Turn on the S3 job:**

```sh
apt install awscli    # Debian 13; on Ubuntu, AWS's installer:
                      # https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html
install -m 0755 cribbage-s3-backup /opt/cribbage/
cp cribbage-s3-backup.service cribbage-s3-backup.timer /etc/systemd/system/
# Edit CRIBBAGE_S3_URL and AWS_REGION in the .service file, then:
systemctl daemon-reload
systemctl enable --now cribbage-s3-backup.timer
systemctl start cribbage-s3-backup      # once now, to check it
journalctl -u cribbage-s3-backup        # "uploaded cribbage-....db to s3://..."
```

The job checks hourly and uploads each new backup once. It remembers what
it uploaded in `/var/lib/cribbage-s3-backup`. Credentials come from the
instance's role, so there are no keys on the machine.

**Keep the email key out of S3.** Store `/etc/cribbage/email-key` in a
password manager. A backup without its key cannot be read, so the two
should not be lost together, or stolen together.

### Restoring onto a new instance

Launch and install as above, with the same email key. Then copy a backup
down from your machine (the instance's role cannot read the bucket):

```sh
aws s3 ls s3://$B/backups/
aws s3 cp s3://$B/backups/cribbage-YYYYMMDD-HHMMSS.db .
scp cribbage-YYYYMMDD-HHMMSS.db admin@SERVER:   # then sudo mv it into /var/lib/cribbage/backups/
```

and follow "To restore" under Backups. Move the Elastic IP to the new
instance, and DNS needs no change.

### Housekeeping

- **To shut it down for good**, cancel the Spot request first (EC2 →
  Spot Requests → Cancel), then terminate the instance. Otherwise the
  persistent request launches a fresh, empty instance to replace it.
  Then delete the kept volume, and release the Elastic IP, which costs
  money while it is not in use.
- **You can stop and start it yourself** like any instance, for example to
  change its type.
- **Set a budget alert** (Billing → Budgets, for example $10 a month) to
  catch anything left running.

## Limits

All built in; the numbers are in `internal/server` (`DefaultAuthLimits`,
`DefaultGameLimits`).

**Signing in.** Remote clients get 20 sign-in attempts (codes, links,
passkeys), then 10 a minute; 5 emailed codes per address, then 1 every 5
minutes, so one address cannot try many accounts; and 3 registrations,
then 3 an hour. Wrong codes and emails are also limited per account and
per email address, and new-account emails from everyone together (see
"Signing in and email"). Stop links are limited to 10, then 10 a minute. IPv6 addresses count by their /64. The address is the one
WebRTC packets come from, so it is the player's real address even if
signaling passes through a proxy. Connections from localhost (bots on the
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

**Web page.** The page has a Content-Security-Policy allowing only its
own script and stylesheet and its server, so injected markup could not
run script. Cloudflare Pages sends it from `_headers`; the server sends
the same for its embedded copy.

## TURN (optional)

A server with a public IP is reachable from nearly every home network, so
start without TURN. If players on networks that block UDP (some offices
and schools) cannot connect, run [coturn](https://github.com/coturn/coturn)
on this machine on port 3478 (and 5349 for TLS; 443 belongs to cribserver), open
those ports, and pass `-ice-servers turn:api.cribbage.example.com:3478` (with
credentials as coturn is configured); browsers get the list from
`/api/config`.

## Desktop clients

GitHub Actions builds the Linux and Windows clients and publishes them
on the repository's Releases page (`.github/workflows/desktop-clients.yml`).
Once, set the repository variable `CRIBBAGE_SERVER` (Settings → Secrets
and variables → Actions → Variables) to the server's address as the web
page uses it, `https://api.cribbage.example.com`; release builds then
connect to `wss://api.cribbage.example.com/rtc` without players typing it.

To release, tag the commit and push the tag:

```sh
git tag client-v1.0.0 && git push origin client-v1.0.0
```

The workflow downloads Godot and its export templates (checked against
their published hashes, and cached between runs), builds both clients and
creates the release `client-v1.0.0` with
`Cribbage-1.0.0-linux-x86_64.zip` and `Cribbage-1.0.0-windows-x86_64.zip`.
To try a build without releasing, run the workflow by hand (Actions →
Desktop clients → Run workflow); its zips are kept with the run for a
week. When a protocol change makes the server refuse old clients, deploy
the server, then release: the client tells players to choose "Updates",
which opens the latest release.

The Windows build is not signed, so Windows warns before the first run
("More info", then "Run anyway"); the README in each zip says so. There
is no macOS build.

`client-godot/export.sh` builds the same zips on your machine, with the
editor and templates that `client-godot/ci/install-godot.sh` installs.
