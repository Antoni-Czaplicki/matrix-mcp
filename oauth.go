package main

// Minimal single-user OAuth 2.1 authorization server, so the MCP can be added as a
// custom connector in claude.ai, ChatGPT and other clients that only speak OAuth.
// Supports dynamic client registration (RFC 7591), authorization code + PKCE (S256)
// and rotating refresh tokens. The consent page signs in with the Matrix account itself:
// the username and password are checked against the homeserver.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/dbutil"
)

const (
	accessTokenTTL  = time.Hour
	refreshTokenTTL = 60 * 24 * time.Hour
	authCodeTTL     = 5 * time.Minute
)

type OAuth struct {
	db      *dbutil.Database
	issuer  string // public base URL, no trailing slash
	account string // Matrix user ID that may sign in
	verify  LoginVerifier

	mu       sync.Mutex
	codes    map[string]*authCode
	failures int
	lastFail time.Time
}

type authCode struct {
	clientID, redirectURI, challenge, resource string
	expires                                    time.Time
}

type oauthClient struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
}

// LoginVerifier checks Matrix credentials. It returns false (and no error) for wrong credentials.
type LoginVerifier func(ctx context.Context, user, password string) (bool, error)

func NewOAuth(ctx context.Context, db *dbutil.Database, issuer, account string, verify LoginVerifier) (*OAuth, error) {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS mcp_oauth_clients (client_id TEXT PRIMARY KEY, name TEXT NOT NULL, redirect_uris TEXT NOT NULL, created INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS mcp_oauth_tokens (hash TEXT PRIMARY KEY, kind TEXT NOT NULL, client_id TEXT NOT NULL, expires INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			return nil, err
		}
	}
	return &OAuth{db: db, issuer: strings.TrimRight(issuer, "/"), account: account, verify: verify, codes: map[string]*authCode{}}, nil
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func (o *OAuth) resourceMetadataURL() string {
	return o.issuer + "/.well-known/oauth-protected-resource"
}

func (o *OAuth) Register(mux *http.ServeMux) {
	resourceMeta := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":                 o.issuer + "/mcp",
			"authorization_servers":    []string{o.issuer},
			"bearer_methods_supported": []string{"header"},
			"resource_name":            "Matrix MCP",
		})
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", resourceMeta)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", resourceMeta)
	asMeta := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                o.issuer,
			"authorization_endpoint":                o.issuer + "/authorize",
			"token_endpoint":                        o.issuer + "/token",
			"registration_endpoint":                 o.issuer + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"scopes_supported":                      []string{"matrix"},
		})
	}
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", asMeta)
	mux.HandleFunc("GET /.well-known/openid-configuration", asMeta)
	mux.HandleFunc("POST /register", o.handleRegister)
	mux.HandleFunc("GET /authorize", o.handleAuthorizeForm)
	mux.HandleFunc("POST /authorize", o.handleAuthorizeSubmit)
	mux.HandleFunc("POST /token", o.handleToken)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

func (o *OAuth) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "1 to 10 redirect_uris required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirectURI(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be https (or http on localhost)")
			return
		}
	}
	if len(req.Name) > 200 {
		req.Name = req.Name[:200]
	}
	c := oauthClient{ID: randomToken(), Name: req.Name, RedirectURIs: req.RedirectURIs}
	uris, _ := json.Marshal(c.RedirectURIs)
	if _, err := o.db.Exec(r.Context(), `INSERT INTO mcp_oauth_clients (client_id, name, redirect_uris, created) VALUES ($1, $2, $3, $4)`,
		c.ID, c.Name, string(uris), time.Now().Unix()); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not store client")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"client_id_issued_at":        time.Now().Unix(),
	})
}

func (o *OAuth) getClient(ctx context.Context, clientID string) (*oauthClient, error) {
	var c oauthClient
	var uris string
	err := o.db.QueryRow(ctx, `SELECT client_id, name, redirect_uris FROM mcp_oauth_clients WHERE client_id = $1`, clientID).Scan(&c.ID, &c.Name, &uris)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(uris), &c.RedirectURIs)
	return &c, nil
}

// authRequest holds the validated parameters of an authorization request.
type authRequest struct {
	Client                                                   *oauthClient
	ClientID, RedirectURI, State, Challenge, Resource, Scope string
	RedirectHost                                             string
	Account                                                  string
	Error                                                    string
}

func (o *OAuth) parseAuthRequest(r *http.Request) (*authRequest, error) {
	v := r.Form
	ar := &authRequest{
		ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), State: v.Get("state"),
		Challenge: v.Get("code_challenge"), Resource: v.Get("resource"), Scope: v.Get("scope"),
	}
	c, err := o.getClient(r.Context(), ar.ClientID)
	if err != nil {
		return nil, errors.New("unknown client_id; the client must register first")
	}
	ar.Client = c
	ar.Account = o.account
	matched := false
	for _, u := range c.RedirectURIs {
		if u == ar.RedirectURI {
			matched = true
		}
	}
	if !matched {
		return nil, errors.New("redirect_uri does not match the registered client")
	}
	if u, err := url.Parse(ar.RedirectURI); err == nil {
		ar.RedirectHost = u.Host
	}
	return ar, nil
}

func redirectWithError(w http.ResponseWriter, r *http.Request, ar *authRequest, code, desc string) {
	u, _ := url.Parse(ar.RedirectURI)
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if ar.State != "" {
		q.Set("state", ar.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (o *OAuth) handleAuthorizeForm(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ar, err := o.parseAuthRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Form.Get("response_type") != "code" {
		redirectWithError(w, r, ar, "unsupported_response_type", "only code is supported")
		return
	}
	if ar.Challenge == "" || r.Form.Get("code_challenge_method") != "S256" {
		redirectWithError(w, r, ar, "invalid_request", "PKCE with S256 is required")
		return
	}
	renderConsent(w, ar)
}

// checkLogin verifies credentials with a global backoff after repeated failures.
func (o *OAuth) checkLogin(ctx context.Context, user, pw string) (bool, string) {
	o.mu.Lock()
	if o.failures >= 5 && time.Since(o.lastFail) < 10*time.Second {
		o.mu.Unlock()
		return false, "Too many failed attempts, wait a few seconds."
	}
	o.mu.Unlock()
	if strings.TrimSpace(user) == "" || pw == "" {
		return false, "Enter your Matrix username and password."
	}
	ok, err := o.verify(ctx, user, pw)
	o.mu.Lock()
	defer o.mu.Unlock()
	if ok {
		o.failures = 0
		return true, ""
	}
	o.failures++
	o.lastFail = time.Now()
	if err != nil {
		return false, "Could not check the login with the homeserver: " + err.Error()
	}
	return false, "Wrong username or password."
}

func (o *OAuth) handleAuthorizeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ar, err := o.parseAuthRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ar.Challenge == "" {
		redirectWithError(w, r, ar, "invalid_request", "PKCE with S256 is required")
		return
	}
	if r.PostForm.Get("action") == "deny" {
		redirectWithError(w, r, ar, "access_denied", "the user denied access")
		return
	}
	if ok, msg := o.checkLogin(r.Context(), r.PostForm.Get("username"), r.PostForm.Get("password")); !ok {
		time.Sleep(time.Second)
		ar.Error = msg
		w.WriteHeader(http.StatusUnauthorized)
		renderConsent(w, ar)
		return
	}
	code := randomToken()
	o.mu.Lock()
	now := time.Now()
	for k, c := range o.codes {
		if now.After(c.expires) {
			delete(o.codes, k)
		}
	}
	o.codes[hashToken(code)] = &authCode{clientID: ar.ClientID, redirectURI: ar.RedirectURI, challenge: ar.Challenge, resource: ar.Resource, expires: now.Add(authCodeTTL)}
	o.mu.Unlock()
	u, _ := url.Parse(ar.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	if ar.State != "" {
		q.Set("state", ar.State)
	}
	q.Set("iss", o.issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (o *OAuth) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
		return
	}
	f := r.PostForm
	clientID := f.Get("client_id")
	if clientID == "" {
		clientID, _, _ = r.BasicAuth()
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		o.mu.Lock()
		key := hashToken(f.Get("code"))
		c := o.codes[key]
		delete(o.codes, key) // single use
		o.mu.Unlock()
		if c == nil || time.Now().After(c.expires) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
			return
		}
		if c.clientID != clientID || c.redirectURI != f.Get("redirect_uri") {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri mismatch")
			return
		}
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(c.challenge)) != 1 {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		o.issueTokens(w, r.Context(), clientID)
	case "refresh_token":
		hash := hashToken(f.Get("refresh_token"))
		var owner string
		var expires int64
		err := o.db.QueryRow(r.Context(), `SELECT client_id, expires FROM mcp_oauth_tokens WHERE hash = $1 AND kind = 'refresh'`, hash).Scan(&owner, &expires)
		if err != nil || owner != clientID || time.Now().Unix() > expires {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
			return
		}
		_, _ = o.db.Exec(r.Context(), `DELETE FROM mcp_oauth_tokens WHERE hash = $1`, hash) // rotate
		o.issueTokens(w, r.Context(), clientID)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (o *OAuth) issueTokens(w http.ResponseWriter, ctx context.Context, clientID string) {
	access, refresh := randomToken(), randomToken()
	now := time.Now()
	err := o.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		if _, err := o.db.Exec(ctx, `DELETE FROM mcp_oauth_tokens WHERE expires < $1`, now.Unix()); err != nil {
			return err
		}
		if _, err := o.db.Exec(ctx, `INSERT INTO mcp_oauth_tokens (hash, kind, client_id, expires) VALUES ($1, 'access', $2, $3)`, hashToken(access), clientID, now.Add(accessTokenTTL).Unix()); err != nil {
			return err
		}
		_, err := o.db.Exec(ctx, `INSERT INTO mcp_oauth_tokens (hash, kind, client_id, expires) VALUES ($1, 'refresh', $2, $3)`, hashToken(refresh), clientID, now.Add(refreshTokenTTL).Unix())
		return err
	})
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         "matrix",
	})
}

// ValidAccessToken reports whether token is an unexpired OAuth access token.
func (o *OAuth) ValidAccessToken(ctx context.Context, token string) bool {
	var expires int64
	err := o.db.QueryRow(ctx, `SELECT expires FROM mcp_oauth_tokens WHERE hash = $1 AND kind = 'access'`, hashToken(token)).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return false
	}
	return time.Now().Unix() <= expires
}

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize Matrix MCP</title>
<style>
:root{color-scheme:light dark;--bg:#f6f7f9;--card:#fff;--fg:#111;--muted:#666;--accent:#0b7a5a;--border:#ddd}
@media (prefers-color-scheme:dark){:root{--bg:#101214;--card:#1a1d21;--fg:#eee;--muted:#9aa;--accent:#2fb486;--border:#333}}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{width:min(420px,calc(100% - 32px));background:var(--card);border:1px solid var(--border);border-radius:14px;padding:28px}
h1{font-size:20px;margin:0 0 8px}p{margin:0 0 16px;color:var(--muted)}b{color:var(--fg)}
input{margin-bottom:10px;width:100%;box-sizing:border-box;padding:10px 12px;border:1px solid var(--border);border-radius:8px;background:transparent;color:var(--fg);font:inherit}
.row{display:flex;flex-direction:row-reverse;gap:10px;margin-top:16px}button{flex:1;padding:10px;border-radius:8px;border:1px solid var(--border);font:inherit;cursor:pointer;background:transparent;color:var(--fg)}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}.err{color:#d33;margin:0 0 12px}
</style></head><body><main>
<h1>Connect to Matrix MCP</h1>
<p><b>{{if .Client.Name}}{{.Client.Name}}{{else}}An app{{end}}</b> wants full access to your Matrix chats, including sending messages. After approving you will be sent to <b>{{.RedirectHost}}</b>.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/authorize">
<input type="hidden" name="client_id" value="{{.ClientID}}"><input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="state" value="{{.State}}"><input type="hidden" name="code_challenge" value="{{.Challenge}}">
<input type="hidden" name="resource" value="{{.Resource}}"><input type="hidden" name="scope" value="{{.Scope}}">
<p>Sign in with the Matrix account <b>{{.Account}}</b>.</p>
<input name="username" placeholder="Username" value="{{.Account}}" autocomplete="username" autocapitalize="none" spellcheck="false">
<input type="password" name="password" placeholder="Matrix password" autocomplete="current-password" autofocus>
<div class="row"><button class="primary" name="action" value="allow">Allow</button><button name="action" value="deny">Deny</button></div>
</form></main></body></html>`))

func renderConsent(w http.ResponseWriter, ar *authRequest) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https: http://localhost:* http://127.0.0.1:*")
	_ = consentTmpl.Execute(w, ar)
}
