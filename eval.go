package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// runEval: son n gelen kutusu mailini degerlendirir ve sonuclari yazar; hicbir sey degistirmez,
// bildirim gondermez. Kural/istem ayari icin: MS_ACCESS_TOKEN ve ANTHROPIC_API_KEY gerekir.
func runEval(n int) int {
	ctx := context.Background()
	g := &graph{http: &http.Client{Timeout: 30 * time.Second}, apiBase: graphBase,
		access: os.Getenv("MS_ACCESS_TOKEN"), exp: time.Now().Add(time.Hour)}
	me, err := g.me(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "me:", err)
		return 1
	}
	q := url.Values{"$top": {fmt.Sprint(n)}, "$orderby": {"receivedDateTime desc"},
		"$select": {"id,subject,from,toRecipients,ccRecipients,receivedDateTime,importance,isRead,webLink,body"}}
	var r struct{ Value []message }
	if err := g.do(ctx, http.MethodGet, "/me/mailFolders/inbox/messages?"+q.Encode(), nil, &r,
		map[string]string{"Prefer": `outlook.body-content-type="text"`}); err != nil {
		fmt.Fprintln(os.Stderr, "liste:", err)
		return 1
	}
	cfg := rules{VIP: splitList(os.Getenv("VIP_SENDERS")), Ignore: splitList(os.Getenv("IGNORE_SENDERS")),
		Keywords: splitList(envOr("URGENT_KEYWORDS", defaultKeywords))}
	ai := newClassifier(firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("CLAUDE_API")), envOr("CLAUDE_MODEL", "claude-haiku-5-5"), me)
	for _, m := range r.Value {
		start := time.Now()
		v, aiErr := ai.classify(ctx, m, ruleHints(m, cfg))
		d := decide(m, me, cfg, v, aiErr)
		fmt.Printf("%-9s mark=%-5v rel=%-5v %4dms | %s | %s\n    -> %s %s\n", d.Level, d.MarkRead, v.Relevant,
			time.Since(start).Milliseconds(), firstNonEmpty(m.From.EmailAddress.Name, m.From.EmailAddress.Address),
			strings.TrimSpace(m.Subject), d.Reason, errString(aiErr))
	}
	return 0
}
