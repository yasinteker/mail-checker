package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Vendor hata raporu (A101, gunde iki kez): satici kaynakli veri hatalari (urun tipi, eksik ozellik,
// kategori eslemesi...) bilinen kaliplardir ve sessiz gecer. Bunlarin disindaki her hata (or. "Timeout
// exceeded when trying to acquire the lock", "Transaction count after EXECUTE...") sistemsel kabul
// edilir ve acil bildirim uretir. Yeni bir zararsiz veri hatasi cikarsa VENDOR_REPORT_KNOWN'a eklenir.

const defaultVendorReportSubject = "Vendor Hata Raporu"

const defaultVendorReportKnown = "ürün tipi hatali,ürün bulunamadi,bilgileri bos olamaz,missing attributes," +
	"ürün aktarımına uygun değil,price not found"

type reportRow struct {
	User  string
	Error string
	Count int
}

var multiSpace = regexp.MustCompile(`\s{2,}`)

// parseReportRows: metin govdesindeki "Username<TAB>ErrorMessage<TAB>HataAdedi" satirlarini okur.
// Son alani sayi olmayan satirlar (baslik, selamlama) atlanir.
func parseReportRows(body string) []reportRow {
	var rows []reportRow
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r", ""), "\n") {
		var fields []string
		if strings.Contains(line, "\t") {
			fields = strings.Split(line, "\t")
		} else {
			fields = multiSpace.Split(line, -1)
		}
		var f []string
		for _, s := range fields {
			if s = strings.TrimSpace(s); s != "" {
				f = append(f, s)
			}
		}
		if len(f) < 3 {
			continue
		}
		n, err := strconv.Atoi(f[len(f)-1])
		if err != nil {
			continue
		}
		rows = append(rows, reportRow{User: f[0], Error: strings.Join(f[1:len(f)-1], " "), Count: n})
	}
	return rows
}

func (r rules) isVendorReport(m message) bool {
	return r.ReportSubject != "" && strings.Contains(lowerTR(m.Subject), lowerTR(r.ReportSubject))
}

func (r rules) knownReportError(msg string) bool {
	l := lowerTR(msg)
	for _, k := range r.ReportKnown {
		if strings.Contains(l, lowerTR(k)) {
			return true
		}
	}
	return false
}

// vendorReportDecision: rapordaki bilinmeyen (sistemsel) hatalari toplar.
func (r rules) vendorReportDecision(m message) decision {
	rows := parseReportRows(m.Body.Content)
	if len(rows) == 0 {
		return decision{Level: levelImportant, Source: "kural:rapor (okunamadı)",
			Reason: "Vendor hata raporunun tablosu okunamadı; biçim değişmiş olabilir, elle bakın."}
	}
	total := map[string]int{}
	var order []string
	for _, row := range rows {
		if r.knownReportError(row.Error) {
			continue
		}
		if _, ok := total[row.Error]; !ok {
			order = append(order, row.Error)
		}
		total[row.Error] += row.Count
	}
	if len(order) == 0 {
		return decision{Level: levelNone, MarkRead: true, Source: "kural:rapor (yalnızca veri hataları)"}
	}
	var parts []string
	for i, e := range order {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d hata türü daha", len(order)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", truncateRunes(e, 90), total[e]))
	}
	return decision{Level: levelUrgent, Source: "kural:rapor (sistemsel)",
		Reason: "Sistemsel hata: " + strings.Join(parts, "; ")}
}

// runReportCheck: son n vendor raporunu yeni kuralla degerlendirip yazar; hicbir sey degistirmez.
// Calisan konteynerde (ayni ortam degiskenleri ve kayitli giris anahtariyla) calistirilir.
func runReportCheck(n int) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := newStore(ctx, cfg.DatabaseURL, cfg.EncKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "veritabani:", err)
		return 1
	}
	g := newGraph(cfg.TenantID, cfg.ClientID, graphScope(cfg.MarkRead), st)
	q := url.Values{
		"$search": {`"subject:` + cfg.Rules.ReportSubject + `"`},
		"$top":    {strconv.Itoa(n)},
		"$select": {"id,subject,from,receivedDateTime,body"},
	}
	var r struct{ Value []message }
	if err := g.do(ctx, http.MethodGet, "/me/messages?"+q.Encode(), nil, &r,
		map[string]string{"Prefer": `outlook.body-content-type="text"`}); err != nil {
		fmt.Fprintln(os.Stderr, "liste:", err)
		return 1
	}
	for _, m := range r.Value {
		if !cfg.Rules.isVendorReport(m) {
			continue
		}
		d := cfg.Rules.vendorReportDecision(m)
		fmt.Printf("%s  %-9s satır=%-3d %s\n", m.ReceivedDateTime.In(cfg.Location).Format("02.01 15:04"), d.Level,
			len(parseReportRows(m.Body.Content)), d.Reason)
	}
	return 0
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
