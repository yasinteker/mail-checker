// mail-checker: Outlook (Microsoft 365) gelen kutusunu izler, her yeni maili kurallar + Claude ile
// degerlendirir; acil/onemli olanlari fcm-bridge uzerinden telefona bildirir, kullaniciyla ilgisiz
// onemsiz mailleri (istenirse) okundu isaretler. Mail icerigi saklanmaz.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata"
)

var version = "dev"

const defaultKeywords = "acil,urgent,kesinti,çöktü,outage,down,canlıda hata,prod hata,ödeme alınamıyor,sipariş düşmüyor,p1,kritik"

type config struct {
	DatabaseURL  string
	ListenAddr   string
	TenantID     string
	ClientID     string
	AnthropicKey string
	Model        string
	NotifyURL    string
	NotifyKey    string
	EncKey       []byte
	Interval     time.Duration
	MarkRead     bool
	DryRun       bool
	Rules        rules
	BootstrapRT  string
	DigestHour   int
	Location     *time.Location
}

func loadConfig() (config, error) {
	c := config{
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		ListenAddr:   envOr("LISTEN_ADDR", ":8080"),
		TenantID:     os.Getenv("MS_TENANT_ID"),
		ClientID:     os.Getenv("MS_CLIENT_ID"),
		AnthropicKey: firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("CLAUDE_API")),
		Model:        envOr("CLAUDE_MODEL", "claude-haiku-5-5"),
		NotifyURL:    os.Getenv("FCM_NOTIFY_URL"),
		NotifyKey:    os.Getenv("FCM_BRIDGE_API_KEY"),
		MarkRead:     os.Getenv("MARK_READ_IRRELEVANT") == "true",
		DryRun:       os.Getenv("DRY_RUN") == "true",
		BootstrapRT:  os.Getenv("MS_BOOTSTRAP_REFRESH_TOKEN"),
		Rules: rules{
			VIP:             splitList(os.Getenv("VIP_SENDERS")),
			Ignore:          splitList(os.Getenv("IGNORE_SENDERS")),
			Keywords:        splitList(envOr("URGENT_KEYWORDS", defaultKeywords)),
			AddressedUrgent: splitList(os.Getenv("ADDRESSED_URGENT_SENDERS")),
		},
	}
	var missing []string
	for k, v := range map[string]string{"DATABASE_URL": c.DatabaseURL, "MS_TENANT_ID": c.TenantID, "MS_CLIENT_ID": c.ClientID,
		"ANTHROPIC_API_KEY": c.AnthropicKey, "FCM_NOTIFY_URL": c.NotifyURL, "FCM_BRIDGE_API_KEY": c.NotifyKey, "TOKEN_ENC_KEY": os.Getenv("TOKEN_ENC_KEY")} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("eksik ortam degiskenleri: %s", strings.Join(missing, ", "))
	}
	var err error
	if c.EncKey, err = base64.StdEncoding.DecodeString(os.Getenv("TOKEN_ENC_KEY")); err != nil {
		return c, fmt.Errorf("TOKEN_ENC_KEY: %w", err)
	}
	if c.Interval, err = time.ParseDuration(envOr("POLL_INTERVAL", "60s")); err != nil || c.Interval < 15*time.Second {
		return c, errors.New("POLL_INTERVAL en az 15s olmali")
	}
	if c.Location, err = time.LoadLocation(envOr("TZ_NAME", "Europe/Istanbul")); err != nil {
		return c, err
	}
	if _, err := fmt.Sscanf(envOr("DIGEST_HOUR", "18"), "%d", &c.DigestHour); err != nil || c.DigestHour < -1 || c.DigestHour > 23 {
		return c, errors.New("DIGEST_HOUR 0-23 ya da -1 (kapali) olmali")
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

type app struct {
	cfg     config
	st      *store
	g       *graph
	ai      *classifier
	me      identity
	notify  notifier
	lastOK  atomic.Int64 // unix saniye, son basarili tarama
	waiting atomic.Bool  // kullanicinin girisi bekleniyor (saglik ucu "ok" doner)

	canMarkRead   bool
	excluded      map[string]bool      // izlenmeyen klasor kimlikleri
	lastNotified  map[string]time.Time // tekrar eden bildirim bastirma: gonderen|konu -> son bildirim
	aiFailingFrom time.Time
	aiAlerted     bool
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "calisan servisin /health ucunu yokla ve cik")
	eval := flag.Int("eval", 0, "son N maili degerlendir ve yazdir (degisiklik yapmaz)")
	flag.Parse()
	if *eval > 0 {
		os.Exit(runEval(*eval))
	}
	if *healthcheck {
		os.Exit(runHealthcheck(envOr("LISTEN_ADDR", ":8080")))
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("yapilandirma", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := newStore(ctx, cfg.DatabaseURL, cfg.EncKey)
	if err != nil {
		slog.Error("veritabani", "err", err)
		os.Exit(1)
	}
	a := &app{cfg: cfg, st: st, notify: notifier{url: cfg.NotifyURL, key: cfg.NotifyKey, dry: cfg.DryRun}}
	a.g = newGraph(cfg.TenantID, cfg.ClientID, graphScope(cfg.MarkRead), st)

	// Saglik ucu girisi beklerken de yanit vermeli; yoksa Swarm konteyneri yeniden baslatir.
	a.lastOK.Store(time.Now().Unix())
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http", "err", err)
			stop()
		}
	}()

	if err := a.bootstrap(ctx); err != nil {
		slog.Error("baslangic", "err", err)
		os.Exit(1)
	}
	slog.Info("basladi", "version", version, "interval", cfg.Interval.String(), "model", cfg.Model,
		"mark_read", cfg.MarkRead, "dry_run", cfg.DryRun, "vip", len(cfg.Rules.VIP), "ignore", len(cfg.Rules.Ignore), "addressed_urgent", len(cfg.Rules.AddressedUrgent))

	a.run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func graphScope(write bool) string {
	perm := "Mail.Read"
	if write {
		perm = "Mail.ReadWrite"
	}
	return "offline_access https://graph.microsoft.com/User.Read https://graph.microsoft.com/" + perm
}

// bootstrap: tek seferlik yenileme anahtarini al, gerekirse cihaz koduyla giris yap, kullaniciyi tani.
func (a *app) bootstrap(ctx context.Context) error {
	rt, _, err := a.st.loadRefresh(ctx)
	if err != nil {
		return err
	}
	if rt == "" && a.cfg.BootstrapRT != "" {
		if err := a.st.saveRefresh(ctx, a.cfg.BootstrapRT, ""); err != nil {
			return err
		}
		slog.Info("baslangic yenileme anahtari iceri alindi; MS_BOOTSTRAP_REFRESH_TOKEN artik silinebilir")
	}
	for {
		a.me, err = a.g.me(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, errNeedLogin) {
			slog.Warn("kullanici bilgisi alinamadi, tekrar denenecek", "err", err)
			if !sleepCtx(ctx, 30*time.Second) {
				return ctx.Err()
			}
			continue
		}
		if err := a.login(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("giris tamamlanmadi; 1 saat sonra yeniden denenecek", "err", err)
			a.waiting.Store(true)
			ok := sleepCtx(ctx, time.Hour)
			a.waiting.Store(false)
			if !ok {
				return ctx.Err()
			}
		}
	}
	a.ai = newClassifier(a.cfg.AnthropicKey, a.cfg.Model, a.me)
	a.lastNotified = map[string]time.Time{}
	for {
		if a.excluded, err = a.g.excludedFolders(ctx); err == nil {
			break
		}
		slog.Warn("klasorler alinamadi, tekrar denenecek", "err", err)
		if !sleepCtx(ctx, 30*time.Second) {
			return ctx.Err()
		}
	}
	a.canMarkRead = a.cfg.MarkRead && strings.Contains(strings.ToLower(a.g.grantedScope(ctx)), "mail.readwrite")
	if a.cfg.MarkRead && !a.canMarkRead {
		slog.Warn("okundu isaretleme acik ama Mail.ReadWrite izni yok; yeniden giris istenecek")
		if err := a.login(ctx); err == nil {
			a.canMarkRead = strings.Contains(strings.ToLower(a.g.grantedScope(ctx)), "mail.readwrite")
		}
	}
	slog.Info("kullanici", "domain", domainOf(a.me.Email), "can_mark_read", a.canMarkRead)
	return nil
}

func (a *app) login(ctx context.Context) error {
	a.waiting.Store(true)
	defer func() { a.waiting.Store(false); a.lastOK.Store(time.Now().Unix()) }()
	return a.g.deviceLogin(ctx, func(code, uri string) {
		slog.Info("cihaz kodu ile giris bekleniyor", "uri", uri)
		a.notify.send(ctx, "critical", "Outlook girişi gerekli",
			fmt.Sprintf("%s adresine girip %s kodunu yazın ve şirket hesabınızla giriş yapın (15 dk geçerli).", uri, code), nil)
	})
}

func (a *app) run(ctx context.Context) {
	for {
		if err := a.poll(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("tarama", "err", err)
			if errors.Is(err, errNeedLogin) {
				if err := a.login(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("giris tamamlanmadi", "err", err)
					a.waiting.Store(true) // bekleme suresince saglikli say
					sleepCtx(ctx, time.Hour)
					a.waiting.Store(false)
				}
			}
		}
		a.maybeDigest(ctx)
		if !sleepCtx(ctx, a.cfg.Interval) {
			return
		}
	}
}

const overlap = 10 * time.Minute

func (a *app) poll(ctx context.Context) error {
	wm, err := a.st.getState(ctx, "watermark")
	if err != nil {
		return err
	}
	since := time.Now().UTC()
	if wm == "" {
		// Ilk calisma: gecmisi taramadan bugunden itibaren izle.
		if err := a.st.setState(ctx, "watermark", since.Format(time.RFC3339)); err != nil {
			return err
		}
	} else if since, err = time.Parse(time.RFC3339, wm); err != nil {
		return err
	}
	msgs, err := a.g.messagesSince(ctx, since.Add(-overlap))
	if err != nil {
		return err
	}
	newest := since
	for _, m := range msgs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.ReceivedDateTime.After(newest) {
			newest = m.ReceivedDateTime
		}
		if a.excluded[m.ParentFolderID] {
			continue
		}
		done, err := a.st.isProcessed(ctx, m.ID)
		if err != nil {
			return err
		}
		if !done {
			if err := a.handle(ctx, m); err != nil {
				return err // isaretlenmeden birakilir, sonraki taramada tekrar denenir
			}
		}
	}
	if newest.After(since) {
		if err := a.st.setState(ctx, "watermark", newest.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if err := a.st.forgetDetails(ctx, detailRetention); err != nil {
		slog.Warn("ayrinti temizligi", "err", err)
	}
	a.lastOK.Store(time.Now().Unix())
	return nil
}

// repeatWindow: ayni gonderenden ayni (rakamlardan arindirilmis) konuyla gelen mailler icin bu sure
// icinde yalnizca bir bildirim gider. Siklikla tekrarlanan alarm maillerinde bildirim yagmurunu onler.
const repeatWindow = time.Hour

var digits = regexp.MustCompile(`[0-9]+`)
var replyPrefix = regexp.MustCompile(`(?i)^\s*((re|fw|fwd|ynt|ilt|yanıt|ilet)\s*:\s*)+`)

func repeatKey(from, subject string) string {
	s := replyPrefix.ReplaceAllString(lowerTR(subject), "")
	return strings.ToLower(from) + "|" + strings.TrimSpace(digits.ReplaceAllString(s, "#"))
}

// lateThreshold: mail bundan daha gec bildiriliyorsa (or. sunucu kapaliyken gelen birikmis mailler)
// govdeye gercek gelis saati yazilir; telefon bildirimin ulastigi ani gosterdigi icin yaniltmasin.
const lateThreshold = 10 * time.Minute

func receivedLine(received, now time.Time, loc *time.Location) string {
	if received.IsZero() || now.Sub(received) < lateThreshold {
		return ""
	}
	r, n := received.In(loc), now.In(loc)
	if r.Year() == n.Year() && r.YearDay() == n.YearDay() {
		return "Geliş: " + r.Format("15:04")
	}
	return "Geliş: " + r.Format("02.01 15:04")
}

// detailRetention: gonderen/konu/gerekce bu sure sonunda silinir.
const detailRetention = 7 * 24 * time.Hour

func (a *app) handle(ctx context.Context, m message) error {
	from := strings.ToLower(m.From.EmailAddress.Address)
	if from == a.me.Email || m.IsRead { // kendi maillerim ya da zaten okunmuslar degerlendirilmez
		return a.st.recordProcessed(ctx, record{ID: m.ID, Received: m.ReceivedDateTime, Level: levelNone, Source: "atlandi"})
	}
	var v verdict
	var aiErr error
	if !matchAddr(from, a.cfg.Rules.Ignore) { // yoksayilan gondericiler icin yapay zekaya sorulmaz
		v, aiErr = a.ai.classify(ctx, m, ruleHints(m, a.cfg.Rules))
		a.trackAI(ctx, aiErr)
	}
	d := decide(m, a.me, a.cfg.Rules, v, aiErr)

	notified := false
	key := repeatKey(from, m.Subject)
	if d.Level != levelNone && !m.IsRead {
		if last, ok := a.lastNotified[key]; ok && time.Since(last) < repeatWindow {
			d.Source += " (tekrar: bildirim bastırıldı)"
			goto after
		}
		sev, label := "info", "Önemli"
		if d.Level == levelUrgent {
			sev, label = "critical", "Acil"
		}
		sender := firstNonEmpty(m.From.EmailAddress.Name, from)
		body := m.Subject
		if d.Reason != "" {
			body += "\n" + d.Reason
		}
		if line := receivedLine(m.ReceivedDateTime, time.Now(), a.cfg.Location); line != "" {
			body += "\n" + line
		}
		notified = a.notify.sendAt(ctx, m.ReceivedDateTime, sev, fmt.Sprintf("%s mail: %s", label, sender), body,
			map[string]string{"url": m.WebLink, "kind": "mail"})
		if notified {
			a.lastNotified[key] = time.Now()
		}
	}
after:
	marked := false
	if d.MarkRead && a.canMarkRead && !m.IsRead && !a.cfg.DryRun {
		if err := a.g.markRead(ctx, m.ID); err != nil {
			slog.Warn("okundu isaretlenemedi", "err", err)
			var ge *graphError
			if errors.As(err, &ge) && ge.Status == http.StatusForbidden {
				a.canMarkRead = false
			}
		} else {
			marked = true
		}
	}
	slog.Info("mail", "level", d.Level, "notified", notified, "marked_read", marked, "would_mark_read", d.MarkRead,
		"from_domain", domainOf(from), "ai_error", errString(aiErr))
	return a.st.recordProcessed(ctx, record{ID: m.ID, Received: m.ReceivedDateTime, Level: d.Level, Notified: notified,
		MarkedRead: marked, Sender: from, Subject: m.Subject, Reason: d.Reason, Source: d.Source})
}

// trackAI: model 30 dakikadan uzun sure yanit vermezse bir kez uyar.
func (a *app) trackAI(ctx context.Context, err error) {
	if err == nil {
		if a.aiAlerted {
			a.notify.send(ctx, "info", "Mail izleyici", "Yapay zekâ değerlendirmesi yeniden çalışıyor.", nil)
		}
		a.aiFailingFrom, a.aiAlerted = time.Time{}, false
		return
	}
	slog.Warn("yapay zeka", "err", err)
	if a.aiFailingFrom.IsZero() {
		a.aiFailingFrom = time.Now()
	}
	if !a.aiAlerted && time.Since(a.aiFailingFrom) > 30*time.Minute {
		a.aiAlerted = true
		a.notify.send(ctx, "warning", "Mail izleyici", "Yapay zekâ 30 dakikadır yanıt vermiyor; yalnızca kurallarla (VIP, anahtar kelime) bildirim yapılıyor.", nil)
	}
}

// maybeDigest: her gun DIGEST_HOUR'da kisa bir ozet bildirimi.
func (a *app) maybeDigest(ctx context.Context) {
	if a.cfg.DigestHour < 0 {
		return
	}
	now := time.Now().In(a.cfg.Location)
	today := now.Format("2006-01-02")
	if now.Hour() < a.cfg.DigestHour {
		return
	}
	last, err := a.st.getState(ctx, "last_digest")
	if err != nil || last == today {
		return
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, a.cfg.Location)
	s, err := a.st.statsSince(ctx, start)
	if err != nil {
		return
	}
	body := fmt.Sprintf("Bugün %d mail değerlendirildi: %d acil, %d önemli.", s.Total, s.Urgent, s.Important)
	if a.canMarkRead {
		body += fmt.Sprintf(" %d ilgisiz mail okundu işaretlendi.", s.MarkedRead)
	}
	if a.notify.send(ctx, "info", "Günlük mail özeti", body, nil) {
		_ = a.st.setState(ctx, "last_digest", today)
		_ = a.st.purge(ctx, 30*24*time.Hour)
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		last := time.Unix(a.lastOK.Load(), 0)
		ok := a.waiting.Load() || time.Since(last) < 5*a.cfg.Interval+time.Minute
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[bool]string{true: "ok", false: "stale"}[ok], "last_poll": last.UTC().Format(time.RFC3339)})
	})
	return mux
}

type notifier struct {
	url, key string
	dry      bool
	client   http.Client
}

func (n *notifier) send(ctx context.Context, severity, title, body string, meta map[string]string) bool {
	return n.sendAt(ctx, time.Time{}, severity, title, body, meta)
}

// sendAt: at sifir degilse bildirimin olay zamani olarak gonderilir (fcm-bridge "timestamp");
// aksi halde fcm-bridge gonderim anini kullanir.
func (n *notifier) sendAt(ctx context.Context, at time.Time, severity, title, body string, meta map[string]string) bool {
	if n.dry {
		slog.Info("DRY_RUN bildirim", "severity", severity)
		return true
	}
	payload := map[string]any{"source": "mail-checker", "severity": severity, "title": title, "body": body}
	if !at.IsZero() {
		payload["timestamp"] = at.UTC().Format(time.RFC3339)
	}
	if len(meta) > 0 {
		payload["meta"] = meta
	}
	b, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(b))
	if err != nil {
		slog.Warn("bildirim", "err", err)
		return false
	}
	req.Header.Set("X-API-Key", n.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		slog.Warn("bildirim", "err", err)
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		slog.Warn("bildirim", "status", resp.StatusCode)
		return false
	}
	return true
}

func runHealthcheck(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/health")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func domainOf(addr string) string { return addr[strings.LastIndex(addr, "@")+1:] }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
