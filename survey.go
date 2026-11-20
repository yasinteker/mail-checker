package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// runIgnoreSurvey: son n gunde IGNORE_SENDERS/MUTE_SENDERS'tan gelen mailleri gonderen + konu kalibina
// gore gruplar; adet, tarih araligi ve ornek icerik yazar. Hicbir sey degistirmez.
func runIgnoreSurvey(days int) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	st, err := newStore(ctx, cfg.DatabaseURL, cfg.EncKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "veritabani:", err)
		return 1
	}
	g := newGraph(cfg.TenantID, cfg.ClientID, graphScope(cfg.MarkRead), st)
	since := time.Now().AddDate(0, 0, -days)
	q := url.Values{
		"$filter":  {"receivedDateTime ge " + since.UTC().Format(time.RFC3339)},
		"$orderby": {"receivedDateTime asc"},
		"$top":     {"500"},
		"$select":  {"subject,from,receivedDateTime,isRead,bodyPreview"},
	}
	type msg struct {
		Subject          string    `json:"subject"`
		From             recipient `json:"from"`
		ReceivedDateTime time.Time `json:"receivedDateTime"`
		IsRead           bool      `json:"isRead"`
		BodyPreview      string    `json:"bodyPreview"`
	}
	type group struct {
		from, subj  string
		n, unread   int
		first, last time.Time
		samples     []string
	}
	digits := regexp.MustCompile(`\d+`)
	groups := map[string]*group{}
	total, scanned := 0, 0
	promo := map[string]int{}
	next := "/me/messages?" + q.Encode()
	for next != "" {
		var r struct {
			Value    []msg  `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := g.do(ctx, http.MethodGet, next, nil, &r, nil); err != nil {
			fmt.Fprintln(os.Stderr, "liste:", err)
			return 1
		}
		for _, m := range r.Value {
			scanned++
			from := strings.ToLower(m.From.EmailAddress.Address)
			if !matchAddr(from, cfg.Rules.Ignore) && !matchAddr(from, cfg.Rules.Mute) {
				continue
			}
			total++
			var pm message
			pm.From = m.From
			pm.Subject = m.Subject
			pm.Body.Content = m.BodyPreview
			if lv, ok := cfg.Rules.promoted(pm); ok {
				promo[string(lv)+"  "+digits.ReplaceAllString(m.Subject, "#")]++
			}
			subj := digits.ReplaceAllString(m.Subject, "#")
			k := from + "|" + subj
			gr := groups[k]
			if gr == nil {
				gr = &group{from: from, subj: subj, first: m.ReceivedDateTime}
				groups[k] = gr
			}
			gr.n++
			if !m.IsRead {
				gr.unread++
			}
			gr.last = m.ReceivedDateTime
			p := truncateRunes(strings.Join(strings.Fields(m.BodyPreview), " "), 220)
			if len(gr.samples) < 3 {
				dup := false
				for _, s := range gr.samples {
					if digits.ReplaceAllString(s, "#") == digits.ReplaceAllString(p, "#") {
						dup = true
					}
				}
				if !dup {
					gr.samples = append(gr.samples, p)
				}
			}
		}
		next = r.NextLink
	}
	list := make([]*group, 0, len(groups))
	for _, gr := range groups {
		list = append(list, gr)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	fmt.Println("istisna kurali ile bildirilecekler:")
	pk := make([]string, 0, len(promo))
	for k := range promo {
		pk = append(pk, k)
	}
	sort.Strings(pk)
	for _, k := range pk {
		fmt.Printf("  %4d  %s\n", promo[k], truncateRunes(k, 110))
	}
	fmt.Println()
	fmt.Printf("taranan=%d eslesen=%d grup=%d (son %d gun)\n\n", scanned, total, len(list), days)
	for _, gr := range list {
		fmt.Printf("== %d adet (%d okunmamis)  %s .. %s\n   %s\n   %s\n", gr.n, gr.unread,
			gr.first.In(cfg.Location).Format("02.01 15:04"), gr.last.In(cfg.Location).Format("02.01 15:04"), gr.from, gr.subj)
		for _, s := range gr.samples {
			fmt.Printf("   > %s\n", s)
		}
		fmt.Println()
	}
	return 0
}
