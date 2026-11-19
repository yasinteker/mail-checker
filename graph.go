package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const graphBase = "https://graph.microsoft.com/v1.0"

// errNeedLogin: kayitli yenileme anahtari yok ya da Microsoft reddetti; cihaz koduyla yeniden giris gerekir.
var errNeedLogin = errors.New("Microsoft girisi gerekli")

type graph struct {
	tenant, clientID, scope string
	st                      *store
	http                    *http.Client
	loginBase               string // test icin degistirilebilir
	apiBase                 string

	mu     sync.Mutex
	access string
	exp    time.Time
}

func newGraph(tenant, clientID, scope string, st *store) *graph {
	return &graph{
		tenant: tenant, clientID: clientID, scope: scope, st: st,
		http:      &http.Client{Timeout: 30 * time.Second},
		loginBase: "https://login.microsoftonline.com",
		apiBase:   graphBase,
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (g *graph) tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	var tr tokenResponse
	form.Set("client_id", g.clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/oauth2/v2.0/token", g.loginBase, g.tenant), strings.NewReader(form.Encode()))
	if err != nil {
		return tr, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.http.Do(req)
	if err != nil {
		return tr, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return tr, fmt.Errorf("token yaniti (%d): %w", resp.StatusCode, err)
	}
	return tr, nil
}

func (g *graph) accept(ctx context.Context, tr tokenResponse) error {
	if tr.RefreshToken != "" {
		if err := g.st.saveRefresh(ctx, tr.RefreshToken, tr.Scope); err != nil {
			return err
		}
	}
	g.access = tr.AccessToken
	g.exp = time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - 2*time.Minute)
	return nil
}

// token returns a valid access token, refreshing it when needed.
func (g *graph) token(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.access != "" && time.Now().Before(g.exp) {
		return g.access, nil
	}
	rt, _, err := g.st.loadRefresh(ctx)
	if err != nil {
		return "", err
	}
	if rt == "" {
		return "", errNeedLogin
	}
	tr, err := g.tokenRequest(ctx, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {rt}, "scope": {g.scope},
	})
	if err != nil {
		return "", err
	}
	switch tr.Error {
	case "":
	case "invalid_grant", "interaction_required", "consent_required":
		slog.Warn("yenileme reddedildi", "error", tr.Error, "desc", firstLine(tr.ErrorDesc))
		return "", errNeedLogin
	default:
		return "", fmt.Errorf("token: %s: %s", tr.Error, firstLine(tr.ErrorDesc))
	}
	return g.access, g.accept(ctx, tr)
}

// grantedScope reports the scope string of the stored token (e.g. to see whether Mail.ReadWrite was granted).
func (g *graph) grantedScope(ctx context.Context) string {
	_, scope, _ := g.st.loadRefresh(ctx)
	return scope
}

type deviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

// deviceLogin runs the device-code flow. prompt is called with the code the user must enter.
func (g *graph) deviceLogin(ctx context.Context, prompt func(code, uri string)) error {
	form := url.Values{"client_id": {g.clientID}, "scope": {g.scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/oauth2/v2.0/devicecode", g.loginBase, g.tenant), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	var dc deviceCode
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&dc)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if dc.Error != "" {
		return fmt.Errorf("devicecode: %s: %s", dc.Error, firstLine(dc.ErrorDesc))
	}
	prompt(dc.UserCode, dc.VerificationURI)

	interval := time.Duration(max(dc.Interval, 5)) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		tr, err := g.tokenRequest(ctx, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {dc.DeviceCode},
		})
		if err != nil {
			return err
		}
		switch tr.Error {
		case "":
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.accept(ctx, tr)
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return fmt.Errorf("giris: %s: %s", tr.Error, firstLine(tr.ErrorDesc))
		}
	}
	return errors.New("giris kodu suresi doldu")
}

func (g *graph) do(ctx context.Context, method, path string, body any, out any, headers map[string]string) error {
	tok, err := g.token(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := path
	if !strings.HasPrefix(u, "https://") {
		u = g.apiBase + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		g.mu.Lock()
		g.access = ""
		g.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &graphError{Status: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

type graphError struct {
	Status int
	Body   string
}

func (e *graphError) Error() string { return fmt.Sprintf("graph %d: %s", e.Status, firstLine(e.Body)) }

type emailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}
type recipient struct {
	EmailAddress emailAddress `json:"emailAddress"`
}

type message struct {
	ID               string      `json:"id"`
	Subject          string      `json:"subject"`
	From             recipient   `json:"from"`
	ToRecipients     []recipient `json:"toRecipients"`
	CcRecipients     []recipient `json:"ccRecipients"`
	ReceivedDateTime time.Time   `json:"receivedDateTime"`
	Importance       string      `json:"importance"`
	IsRead           bool        `json:"isRead"`
	WebLink          string      `json:"webLink"`
	Body             struct {
		Content string `json:"content"`
	} `json:"body"`
}

type identity struct{ Name, Email string }

func (g *graph) me(ctx context.Context) (identity, error) {
	var r struct {
		DisplayName       string `json:"displayName"`
		Mail              string `json:"mail"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	if err := g.do(ctx, http.MethodGet, "/me?$select=displayName,mail,userPrincipalName", nil, &r, nil); err != nil {
		return identity{}, err
	}
	email := r.Mail
	if email == "" {
		email = r.UserPrincipalName
	}
	return identity{Name: r.DisplayName, Email: strings.ToLower(email)}, nil
}

// inboxSince lists inbox messages received at or after since, oldest first, bodies as plain text.
func (g *graph) inboxSince(ctx context.Context, since time.Time) ([]message, error) {
	q := url.Values{
		"$filter":  {"receivedDateTime ge " + since.UTC().Format(time.RFC3339)},
		"$orderby": {"receivedDateTime asc"},
		"$top":     {"50"},
		"$select":  {"id,subject,from,toRecipients,ccRecipients,receivedDateTime,importance,isRead,webLink,body"},
	}
	next := "/me/mailFolders/inbox/messages?" + q.Encode()
	hdr := map[string]string{"Prefer": `outlook.body-content-type="text"`}
	var all []message
	for page := 0; next != "" && page < 20; page++ {
		var r struct {
			Value    []message `json:"value"`
			NextLink string    `json:"@odata.nextLink"`
		}
		if err := g.do(ctx, http.MethodGet, next, nil, &r, hdr); err != nil {
			return nil, err
		}
		all = append(all, r.Value...)
		next = r.NextLink
	}
	return all, nil
}

func (g *graph) markRead(ctx context.Context, id string) error {
	return g.do(ctx, http.MethodPatch, "/me/messages/"+url.PathEscape(id), map[string]bool{"isRead": true}, nil, nil)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
