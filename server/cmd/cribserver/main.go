// Command cribserver runs the cribbage game server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/ai"
	"cribbage/internal/auth"
	"cribbage/internal/bots"
	"cribbage/internal/expert"
	"cribbage/internal/mail"
	"cribbage/internal/server"
	"cribbage/internal/store"
	"cribbage/internal/transport/rtc"
	"cribbage/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		adminMain(os.Args[2:])
		return
	}
	tcpAddr := flag.String("tcp", "127.0.0.1:7777", "TCP listen address for headless clients (unencrypted and not rate limited from localhost; keep it private)")
	httpAddr := flag.String("http", "127.0.0.1:8080", "HTTP address for the web client and WebRTC signaling (empty disables; :8080 for other machines on the network)")
	webDir := flag.String("web-dir", "", "serve the web client from this directory instead of the embedded copy")
	rtcPort := flag.Int("rtc-port", 7778, "UDP port for all WebRTC traffic (0 picks a port per connection)")
	publicIPs := flag.String("public-ip", "", "comma-separated public IPs to advertise for WebRTC (servers behind 1:1 NAT)")
	rtcPublic := flag.Bool("rtc-public-only", false, "ignore WebRTC candidates on private and loopback addresses (set on an internet server; leave off for one machine or a LAN)")
	iceServers := flag.String("ice-servers", "", "comma-separated STUN/TURN URLs for WebRTC, e.g. stun:stun.l.google.com:19302")
	dbPath := flag.String("db", "cribbage.db", "SQLite database file; empty keeps everything in memory")
	statsEvery := flag.Duration("stats", 10*time.Second, "log statistics at this interval (0 disables)")
	bcryptCost := flag.Int("bcrypt-cost", bcrypt.DefaultCost, "bcrypt cost for password hashes")
	pprofAddr := flag.String("pprof", "", "serve net/http/pprof on this address (e.g. localhost:6060)")
	model := flag.String("model", "", "neural network model to offer as the \"nn\" bot (e.g. models/best.bin)")
	backupDir := flag.String("backup-dir", "", "write database backups here (empty disables)")
	backupEvery := flag.Duration("backup-every", 24*time.Hour, "how often to back up the database (also at startup, unless the last backup is more recent)")
	backupKeep := flag.Int("backup-keep", 7, "how many backups to keep")
	expertPath := flag.String("expert", "", "expert bundle to offer as the \"expert\" bots (e.g. models-expert/expert.bin)")
	adminSocket := flag.String("admin-socket", "", "listen for admin commands (cribserver admin ...) on this Unix socket, e.g. "+defaultAdminSocket+" (empty disables)")
	publicURL := flag.String("public-url", "", "the web client's public address, for links in emails (e.g. https://cribbage.example.com)")
	smtpAddr := flag.String("smtp", "", "mail server host:port for password reset emails (465 for TLS, else STARTTLS); empty disables email")
	smtpUser := flag.String("smtp-user", "", "mail server login")
	smtpPassFile := flag.String("smtp-password-file", "", "file holding the mail server password (or set CRIBBAGE_SMTP_PASSWORD)")
	mailFrom := flag.String("mail-from", "", "sender address for emails, e.g. cribbage@example.com")
	mailFromName := flag.String("mail-from-name", "Cribbage", "sender name for emails")
	emailKeyFile := flag.String("email-key-file", "", "file holding the key (64 hex digits, from openssl rand -hex 32) that encrypts email addresses in the database (or set CRIBBAGE_EMAIL_KEY); needed with -smtp or -mail-log")
	reportURL := flag.String("report-url", "https://github.com/Optimoos", "where the clients' \"Report a bug\" links go (https)")
	mailLog := flag.Bool("mail-log", false, "log emails, links included, instead of sending them (testing only)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
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

	authSvc := &auth.Service{Store: users, BcryptCost: *bcryptCost, BaseURL: *publicURL}
	switch {
	case *smtpAddr != "" && *mailLog:
		fail("email", errors.New("use -smtp or -mail-log, not both"))
	case *smtpAddr != "":
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
	case *mailLog:
		if authSvc.BaseURL == "" {
			authSvc.BaseURL = "http://" + *httpAddr + "/"
		}
		authSvc.Mailer = mail.Log{Logger: log}
		log.Warn("emails are logged, not sent (-mail-log): their links can reset passwords")
	}
	if sq != nil {
		key := os.Getenv("CRIBBAGE_EMAIL_KEY")
		if *emailKeyFile != "" {
			b, err := os.ReadFile(*emailKeyFile)
			if err != nil {
				fail("email key", err)
			}
			key = string(b)
		}
		switch {
		case key != "":
			k, err := store.ParseEmailKey(key)
			if err != nil {
				fail("email key", err)
			}
			if err := sq.SetEmailKey(context.Background(), k); err != nil {
				fail("email key", err)
			}
		case authSvc.Mailer != nil:
			fail("email", errors.New("email addresses are stored encrypted: give -email-key-file (create one with openssl rand -hex 32)"))
		}
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
			Logger:     log,
		})
		if err != nil {
			fail("webrtc", err)
		}
		mux := http.NewServeMux()
		mux.Handle("/rtc", rtcAPI.Handler(func(c *rtc.Conn) { srv.ServeConn(c) }))
		mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			cfg := rtcAPI.ClientConfig()
			cfg["reportUrl"] = *reportURL
			json.NewEncoder(w).Encode(cfg)
		})
		mux.HandleFunc("/api/bots", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(bots.Names())
		})
		mux.Handle("/", web.Handler(*webDir))
		// Requests are small, and /rtc's WebSocket is hijacked, which clears
		// these deadlines.
		httpSrv = &http.Server{
			Addr: *httpAddr, Handler: mux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    16 << 10,
		}
		hl, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			fail("listen http", err)
		}
		log.Info("web client", "url", "http://"+displayAddr(hl.Addr()), "webrtc_udp", *rtcPort)
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
		httpSrv.Close()
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
