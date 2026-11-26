// Command cribserver runs the cribbage game server.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"cribbage/internal/ai"
	"cribbage/internal/auth"
	"cribbage/internal/bots"
	"cribbage/internal/expert"
	"cribbage/internal/mail"
	"cribbage/internal/netkey"
	"cribbage/internal/ratelimit"
	"cribbage/internal/server"
	"cribbage/internal/store"
	"cribbage/internal/transport/rtc"
	"cribbage/web"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		adminMain(os.Args[2:])
		return
	}
	tcpAddr := flag.String("tcp", "127.0.0.1:7777", "TCP listen address for headless clients (unencrypted and not rate limited from localhost; keep it private)")
	httpAddr := flag.String("http", "127.0.0.1:8080", "HTTP address for the web client and WebRTC signaling (empty disables; :8080 for other machines on the network)")
	tlsDomain := flag.String("tls-domain", "", "serve HTTPS on the -http address (which must then be :443) with a Let's Encrypt certificate for this host name, e.g. api.cribbage.example.com; empty serves plain HTTP")
	tlsCache := flag.String("tls-cache", "certs", "directory that keeps the -tls-domain certificate between restarts")
	webDir := flag.String("web-dir", "", "serve the web client from this directory instead of the embedded copy")
	rtcPort := flag.Int("rtc-port", 7778, "UDP port for all WebRTC traffic (0 picks a port per connection)")
	publicIPs := flag.String("public-ip", "", "comma-separated public IPs to advertise for WebRTC (servers behind 1:1 NAT)")
	rtcPublic := flag.Bool("rtc-public-only", false, "ignore WebRTC candidates on private and loopback addresses (set on an internet server; leave off for one machine or a LAN)")
	iceServers := flag.String("ice-servers", "", "comma-separated STUN/TURN URLs for WebRTC, e.g. stun:stun.l.google.com:19302")
	dbPath := flag.String("db", "cribbage.db", "SQLite database file; empty keeps everything in memory")
	statsEvery := flag.Duration("stats", 10*time.Second, "log statistics at this interval (0 disables)")
	pprofAddr := flag.String("pprof", "", "serve net/http/pprof on this address (e.g. localhost:6060)")
	model := flag.String("model", "", "neural network model to offer as the \"nn\" bot (e.g. models/best.bin)")
	backupDir := flag.String("backup-dir", "", "write database backups here (empty disables)")
	backupEvery := flag.Duration("backup-every", 24*time.Hour, "how often to back up the database (also at startup, unless the last backup is more recent)")
	backupKeep := flag.Int("backup-keep", 7, "how many backups to keep")
	lobby := flag.String("lobby", "expert,expert-medium,expert-easy,random", "comma-separated bots the clients offer in the lobby, hardest first; bots this server lacks (nn and expert need -model and -expert) are left out, and the rest stay playable by name")
	expertPath := flag.String("expert", "", "expert bundle to offer as the \"expert\" bots (e.g. models-expert/expert.bin)")
	adminSocket := flag.String("admin-socket", "", "listen for admin commands (cribserver admin ...) on this Unix socket, e.g. "+defaultAdminSocket+" (empty disables)")
	publicURL := flag.String("public-url", "", "the web client's public address, for links in emails and for passkeys (e.g. https://cribbage.example.com); defaults to http://localhost:<http port>. If set, only pages from it (or this server) may open WebRTC signaling")
	smtpAddr := flag.String("smtp", "", "mail server host:port for sign-in codes (465 for TLS, else STARTTLS); empty writes emails to the log instead (testing only: nobody else can sign in)")
	smtpUser := flag.String("smtp-user", "", "mail server login")
	smtpPassFile := flag.String("smtp-password-file", "", "file holding the mail server password (or set CRIBBAGE_SMTP_PASSWORD)")
	mailFrom := flag.String("mail-from", "", "sender address for emails, e.g. cribbage@example.com")
	mailFromName := flag.String("mail-from-name", "Cribbage", "sender name for emails")
	emailKeyFile := flag.String("email-key-file", "", "file holding the key (64 hex digits, from openssl rand -hex 32) that encrypts email addresses in the database (or set CRIBBAGE_EMAIL_KEY); without one, a key is kept beside the database in <db>.email-key")
	reportURL := flag.String("report-url", "https://github.com/Optimoos/claude-cribbage/issues", "where the clients' \"Report a bug\" links go (https)")
	botKeyFile := flag.String("bot-key-file", "", "file holding a secret that headless bots (cmd/botclient) sign in with (or set CRIBBAGE_BOT_KEY); empty disables bot accounts")
	configPath := flag.String("config", "", "read settings from this file (name = value per line, e.g. /etc/cribbage/cribserver.conf); flags on the command line override it")
	printConfig := flag.Bool("print-config", false, "print a config file with every setting, using these flags' values, and exit")
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "cribserver: unexpected argument %q\n", flag.Arg(0))
		os.Exit(2)
	}
	if *configPath != "" {
		if err := applyConfig(flag.CommandLine, *configPath); err != nil {
			fmt.Fprintln(os.Stderr, "cribserver: config:", err)
			os.Exit(2)
		}
	}
	if *printConfig {
		writeConfig(os.Stdout, flag.CommandLine)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
	if *configPath != "" {
		log.Info("config", "path", *configPath)
	}

	if *model != "" {
		meta, err := ai.Register(*model)
		if err != nil {
			fail("load model", err)
		}
		log.Info("loaded model", "path", *model, "bot", ai.BotName, "sizes", meta.Sizes, "games", meta.Games,
			"vs_heuristic", meta.Metrics["vs_heuristic"])
	}

	if *expertPath != "" {
		b, err := expert.LoadBundle(*expertPath)
		if err != nil {
			fail("load expert bundle", err)
		}
		for _, l := range expert.Levels {
			if err := b.Check(l.Options); err != nil {
				fail("expert bundle "+*expertPath+" cannot play level "+l.Name, err)
			}
		}
		expert.Register(b)
		names := make([]string, len(expert.Levels))
		for i, l := range expert.Levels {
			names[i] = l.Name
		}
		log.Info("loaded expert bundle", "path", *expertPath, "saved", b.Meta.Saved, "bots", strings.Join(names, ","),
			"discard_tables", b.Discard != nil, "opponent_model", b.KeepTemp[0] > 0)
	}

	start := time.Now()
	bots.Prepare()
	log.Info("built bot tables", "took", time.Since(start).Round(time.Millisecond))

	var users store.Store
	var games store.GameStore
	var sq *store.SQLite
	if *dbPath == "" {
		m := store.NewMemory()
		users, games = m, m
		log.Warn("no database: users and games are lost on restart")
	} else {
		var err error
		if sq, err = store.OpenSQLite(*dbPath); err != nil {
			fail("open database", err)
		}
		users, games = sq, sq
	}

	authSvc := &auth.Service{Store: users, BaseURL: *publicURL, Log: log}
	if *tlsDomain != "" {
		authSvc.StopURL = "https://" + *tlsDomain + "/api/stop"
	}
	if authSvc.BaseURL == "" && *tlsDomain != "" {
		authSvc.BaseURL = "https://" + *tlsDomain + "/"
	} else if authSvc.BaseURL == "" {
		authSvc.BaseURL = localURL(*httpAddr)
	}
	if *smtpAddr != "" {
		pass := os.Getenv("CRIBBAGE_SMTP_PASSWORD")
		if *smtpPassFile != "" {
			b, err := os.ReadFile(*smtpPassFile)
			if err != nil {
				fail("smtp password", err)
			}
			pass = strings.TrimSpace(string(b))
		}
		m := &mail.SMTP{Addr: *smtpAddr, Username: *smtpUser, Password: pass, From: *mailFrom, FromName: *mailFromName}
		if err := m.Check(); err != nil {
			fail("email", err)
		}
		if err := checkPublicURL(*publicURL); err != nil {
			fail("email", err)
		}
		authSvc.Mailer = m
		log.Info("email enabled", "smtp", *smtpAddr, "from", *mailFrom)
	} else {
		authSvc.Mailer = mail.Log{Logger: log}
		log.Warn("no -smtp: emails are written to this log instead of sent, so only someone reading it can sign in (testing only)")
	}
	if sq != nil {
		// A key that was given but is empty is a mistake (an empty file, an
		// unset variable in a unit file), not a request for a local key.
		key, given := os.LookupEnv("CRIBBAGE_EMAIL_KEY")
		if *emailKeyFile != "" {
			b, err := os.ReadFile(*emailKeyFile)
			if err != nil {
				fail("email key", err)
			}
			key, given = string(b), true
		}
		if given && strings.TrimSpace(key) == "" {
			fail("email key", errors.New("the email key given (-email-key-file or CRIBBAGE_EMAIL_KEY) is empty"))
		}
		if !given {
			path := *dbPath + ".email-key"
			var err error
			if key, err = localEmailKey(path); err != nil {
				fail("email key", err)
			}
			log.Warn("the email key is kept beside the database; for a deployment, keep it elsewhere with -email-key-file", "path", path)
		}
		k, err := store.ParseEmailKey(key)
		if err != nil {
			fail("email key", err)
		}
		if err := sq.SetEmailKey(context.Background(), k); err != nil {
			fail("email key", err)
		}
	}
	if wa, err := auth.NewWebAuthn(authSvc.BaseURL); err != nil {
		fail("passkeys", err)
	} else if ip := net.ParseIP(hostOf(authSvc.BaseURL)); ip != nil {
		log.Warn("passkeys are off: they need the web client's address to be a name, not an IP address (-public-url)")
	} else {
		authSvc.WebAuthn = wa
	}
	authSvc.BotKey = os.Getenv("CRIBBAGE_BOT_KEY")
	if *botKeyFile != "" {
		b, err := os.ReadFile(*botKeyFile)
		if err != nil {
			fail("bot key", err)
		}
		authSvc.BotKey = strings.TrimSpace(string(b))
	}
	if authSvc.BotKey != "" && len(authSvc.BotKey) < 16 {
		fail("bot key", errors.New("the bot key must be at least 16 characters (try openssl rand -hex 16)"))
	}
	if err := checkReportURL(*reportURL); err != nil {
		fail("report url", err)
	}

	srv := server.New(server.Config{
		Auth:   authSvc,
		Games:  games,
		Logger: log,
	})
	start = time.Now()
	n, err := srv.Restore(context.Background())
	if err != nil {
		fail("restore games", err)
	}
	log.Info("restored games", "count", n, "took", time.Since(start).Round(time.Millisecond))

	ln, err := net.Listen("tcp", *tcpAddr)
	if err != nil {
		fail("listen", err)
	}
	log.Info("listening", "tcp", ln.Addr().String(), "db", *dbPath)
	go func() {
		if err := srv.ServeTCP(ln); err != nil {
			fail("serve", err)
		}
	}()

	var httpSrv *http.Server
	var rtcAPI *rtc.API
	if *httpAddr != "" {
		rtcAPI, err = rtc.NewAPI(rtc.Options{
			UDPPort:    *rtcPort,
			PublicIPs:  splitList(*publicIPs),
			ICEServers: splitList(*iceServers),
			PublicOnly: *rtcPublic,
			Origins:    originsOf(*publicURL),
			Logger:     log,
		})
		if err != nil {
			fail("webrtc", err)
		}
		var tlsConfig *tls.Config
		if *tlsDomain != "" {
			if *tlsCache == "" {
				fail("tls", errors.New("-tls-domain needs -tls-cache"))
			}
			m := &autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(*tlsDomain),
				Cache:      autocert.DirCache(*tlsCache),
			}
			// The certificate comes by TLS-ALPN-01, on this listener, so
			// no port 80. HTTP/1.1 only: the signaling WebSocket needs a
			// connection it can take over, which HTTP/2 does not give.
			tlsConfig = m.TLSConfig()
			tlsConfig.NextProtos = []string{"http/1.1", acme.ALPNProto}
		}
		mux := http.NewServeMux()
		mux.Handle("/rtc", rtcAPI.Handler(func(c *rtc.Conn) { srv.ServeConn(c) }))
		// The web client may be hosted elsewhere (deploy/README.md), so
		// any page may read these: they are public and need no sign-in.
		mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			cfg := rtcAPI.ClientConfig()
			cfg["reportUrl"] = *reportURL
			cfg["passkeys"] = authSvc.WebAuthn != nil
			json.NewEncoder(w).Encode(cfg)
		})
		mux.HandleFunc("/api/bots", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			json.NewEncoder(w).Encode(bots.Names())
		})
		// The clients build their lobby and label bots from this, so bots
		// can change without new client builds.
		lobbyJSON, err := json.Marshal(lobbyInfo(*lobby))
		if err != nil {
			fail("lobby", err)
		}
		mux.HandleFunc("/api/lobby", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Write(lobbyJSON)
		})
		// The stop link in every email: the web client POSTs its token here,
		// and so do mail apps' one-click "Unsubscribe" (RFC 8058), which add
		// a form body this ignores. Tokens cannot be guessed; the limit
		// keeps anyone from making the server check them by the million.
		stopLimit := ratelimit.New(10, 6*time.Second)
		mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			if r.Method != http.MethodPost {
				http.Error(w, "POST a stop token", http.StatusMethodNotAllowed)
				return
			}
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if !stopLimit.Take(netkey.Of(host), time.Now()) {
				http.Error(w, "too many requests; try again later", http.StatusTooManyRequests)
				return
			}
			switch err := authSvc.StopEmail(r.Context(), r.URL.Query().Get("t")); {
			case errors.Is(err, auth.ErrBadToken):
				http.Error(w, "that link is not one this server sent", http.StatusNotFound)
			case err != nil:
				log.Error("stop email", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			default:
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"ok":true}` + "\n"))
			}
		})
		mux.Handle("/", web.Handler(*webDir))
		// Requests are small, and /rtc's WebSocket is hijacked, which clears
		// these deadlines.
		httpSrv = &http.Server{
			Addr: *httpAddr, Handler: securityHeaders(mux, tlsConfig != nil),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    16 << 10,
			// On the internet, scanners fail TLS handshakes all day.
			ErrorLog: stdlog.New(dropLines{os.Stderr, "TLS handshake error"}, "", stdlog.LstdFlags),
		}
		hl, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			fail("listen http", err)
		}
		scheme, host := "http://", displayAddr(hl.Addr())
		if tlsConfig != nil {
			scheme, host = "https://", *tlsDomain
			hl = tls.NewListener(hl, tlsConfig)
		}
		log.Info("web client", "url", scheme+host, "webrtc_udp", *rtcPort)
		go func() {
			if err := httpSrv.Serve(hl); err != nil && err != http.ErrServerClosed {
				fail("serve http", err)
			}
		}()
	}

	var adminLn net.Listener
	if *adminSocket != "" {
		if adminLn, err = serveAdmin(log, *adminSocket, srv); err != nil {
			fail("admin socket", err)
		}
		log.Info("admin socket", "path", *adminSocket)
	}

	if *pprofAddr != "" {
		go func() { log.Error("pprof", "err", http.ListenAndServe(*pprofAddr, nil)) }()
	}
	if *statsEvery > 0 {
		go logStats(log, srv, sq, *statsEvery)
	}
	stopBackups := make(chan struct{})
	backupsDone := make(chan struct{})
	if sq != nil && *backupDir != "" {
		go backups(log, sq, *backupDir, *backupEvery, *backupKeep, stopBackups, backupsDone)
	} else {
		close(backupsDone)
		if *backupDir != "" {
			log.Warn("no database: -backup-dir ignored")
		}
	}

	// systemd and docker stop with SIGTERM; a terminal with SIGINT.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")
	ln.Close()
	if adminLn != nil {
		adminLn.Close()
	}
	if httpSrv != nil {
		// Let requests in flight (a stop link writes to the store) finish.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if httpSrv.Shutdown(ctx) != nil {
			httpSrv.Close()
		}
		cancel()
	}
	srv.Shutdown()
	close(stopBackups)
	<-backupsDone
	if rtcAPI != nil {
		rtcAPI.Close()
	}
	if sq != nil {
		if err := sq.Close(); err != nil {
			fail("close database", err)
		}
	}
}

// backups writes a database backup every interval until stop is closed,
// starting now unless a backup was taken less than an interval ago (so a
// crash loop does not write one at every start).
func backups(log *slog.Logger, sq *store.SQLite, dir string, every time.Duration, keep int, stop, done chan struct{}) {
	defer close(done)
	wait := time.Duration(0)
	if last := store.LastBackup(dir); !last.IsZero() {
		wait = max(0, every-time.Since(last))
		if wait > 0 {
			log.Info("recent database backup", "taken", last.Local().Format(time.DateTime), "next_in", wait.Round(time.Minute))
		}
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-stop:
			return
		}
		start := time.Now()
		path, err := sq.BackupTo(context.Background(), dir, keep)
		if err != nil {
			log.Error("database backup failed", "err", err)
		} else {
			log.Info("database backed up", "path", path, "took", time.Since(start).Round(time.Millisecond))
		}
		t.Reset(every)
	}
}

func logStats(log *slog.Logger, srv *server.Server, sq *store.SQLite, every time.Duration) {
	prev := srv.Stats()
	var prevBatches, prevOps int64
	for range time.Tick(every) {
		s := srv.Stats()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		secs := every.Seconds()
		attrs := []any{
			"conns", s.Connections,
			"active_games", s.ActiveGames,
			"finished", s.GamesFinished,
			"games_per_sec", float64(s.GamesFinished-prev.GamesFinished) / secs,
			"actions_per_sec", float64(s.Actions-prev.Actions) / secs,
			"goroutines", runtime.NumGoroutine(),
			"heap_mb", ms.HeapAlloc >> 20,
		}
		if sq != nil {
			b, o := sq.WriteStats()
			if b > prevBatches {
				attrs = append(attrs, "db_tx_per_sec", float64(b-prevBatches)/secs,
					"db_writes_per_tx", float64(o-prevOps)/float64(b-prevBatches))
			}
			prevBatches, prevOps = b, o
		}
		log.Info("stats", attrs...)
		prev = s
	}
}

// dropLines writes what log lines do not contain skip.
type dropLines struct {
	w    io.Writer
	skip string
}

func (d dropLines) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(d.skip)) {
		return len(p), nil
	}
	return d.w.Write(p)
}

// securityHeaders adds the headers a proxy in front would otherwise add.
// The web client's Content-Security-Policy is set in package web.
func securityHeaders(h http.Handler, https bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if https {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// checkPublicURL requires an absolute http(s) URL, https unless the host
// is local.
func checkPublicURL(s string) error {
	if s == "" {
		return errors.New("-public-url is needed for the links in emails")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("-public-url %q must be an http(s) URL", s)
	}
	if h := u.Hostname(); u.Scheme == "http" && h != "localhost" && !net.ParseIP(h).IsLoopback() {
		return fmt.Errorf("-public-url %q must use https", s)
	}
	return nil
}

// checkReportURL accepts an https URL, which clients open in a browser.
func checkReportURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("-report-url %q must be an https URL", s)
	}
	return nil
}

// displayAddr turns ":8080" style listen addresses into a usable URL host.
func displayAddr(a net.Addr) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil || host == "" || host == "::" || host == "0.0.0.0" {
		return "localhost:" + port
	}
	return a.String()
}

// localURL is the web client's address on this machine, for a server
// without -public-url.
func localURL(httpAddr string) string {
	host, port, err := net.SplitHostPort(httpAddr)
	if err != nil {
		return "http://localhost:8080/"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && (ip.IsUnspecified() || ip.IsLoopback())) {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// originsOf is the web client's host, whose pages may open signaling (none
// without -public-url: then any page may).
func originsOf(publicURL string) []string {
	if p, err := url.Parse(publicURL); err == nil && p.Host != "" {
		return []string{p.Host}
	}
	return nil
}

func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Hostname()
}

// localEmailKey reads the key at path, creating it if there is none.
func localEmailKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return string(b), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	s := hex.EncodeToString(key) + "\n"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return "", err
	}
	return s, f.Close()
}
