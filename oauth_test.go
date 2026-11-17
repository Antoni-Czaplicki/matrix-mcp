package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOAuthFlow(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewUnstartedServer(nil)
	issuer := "http://" + srv.Listener.Addr().String()
	verify := func(_ context.Context, user, pw string) (bool, error) {
		return user == "@me:example.com" && pw == "correct horse battery", nil
	}
	o, err := NewOAuth(ctx, testDB(t), issuer, "@me:example.com", verify)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	o.Register(mux)
	protected := requireAuth(strings.Repeat("s", 32), o, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	mux.Handle("/mcp", protected)
	srv.Config.Handler = mux
	srv.Start()
	defer srv.Close()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// Unauthenticated request points at the resource metadata.
	resp, _ := http.Post(srv.URL+"/mcp", "application/json", nil)
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("unauthenticated: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// Discovery.
	var meta map[string]any
	resp, _ = http.Get(srv.URL + "/.well-known/oauth-authorization-server")
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	if meta["token_endpoint"] != issuer+"/token" {
		t.Fatalf("metadata: %v", meta)
	}

	// Dynamic client registration.
	if resp, _ := http.Post(srv.URL+"/register", "application/json", strings.NewReader(`{"redirect_uris":["http://evil.example/cb"]}`)); resp.StatusCode != 400 {
		t.Errorf("non-https redirect should be rejected, got %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/register", "application/json", strings.NewReader(`{"client_name":"Test","redirect_uris":["https://client.example/cb"]}`))
	var reg map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	clientID, _ := reg["client_id"].(string)
	if resp.StatusCode != 201 || clientID == "" {
		t.Fatalf("register: %d %v", resp.StatusCode, reg)
	}

	verifier := "verifier-verifier-verifier-verifier-verifier"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authQ := url.Values{"client_id": {clientID}, "redirect_uri": {"https://client.example/cb"}, "response_type": {"code"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"xyz"}}

	resp, _ = http.Get(srv.URL + "/authorize?" + authQ.Encode())
	if resp.StatusCode != 200 {
		t.Fatalf("consent page: %d", resp.StatusCode)
	}
	bad := url.Values{"client_id": {clientID}, "redirect_uri": {"https://other.example/cb"}, "code_challenge": {challenge}}
	if resp, _ := http.PostForm(srv.URL+"/authorize", bad); resp.StatusCode != 400 {
		t.Errorf("unregistered redirect_uri should be rejected, got %d", resp.StatusCode)
	}

	form := url.Values{"client_id": {clientID}, "redirect_uri": {"https://client.example/cb"}, "code_challenge": {challenge}, "state": {"xyz"}, "action": {"allow"}, "username": {"@me:example.com"}}
	form.Set("password", "wrong")
	if resp, _ := noRedirect.PostForm(srv.URL+"/authorize", form); resp.StatusCode != 401 {
		t.Errorf("wrong password: got %d", resp.StatusCode)
	}
	form.Set("password", "correct horse battery")
	resp, _ = noRedirect.PostForm(srv.URL+"/authorize", form)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	code := loc.Query().Get("code")
	if resp.StatusCode != 302 || code == "" || loc.Query().Get("state") != "xyz" || loc.Host != "client.example" {
		t.Fatalf("authorize: %d %s", resp.StatusCode, loc)
	}

	token := func(v url.Values) (int, map[string]any) {
		resp, _ := http.PostForm(srv.URL+"/token", v)
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"https://client.example/cb"}, "code_verifier": {"wrong-verifier"}}
	if st, _ := token(exchange); st != 400 {
		t.Errorf("wrong PKCE verifier: %d", st)
	}
	// The code is single use, so the failed attempt burned it; get a new one.
	resp, _ = noRedirect.PostForm(srv.URL+"/authorize", form)
	loc, _ = url.Parse(resp.Header.Get("Location"))
	exchange.Set("code", loc.Query().Get("code"))
	exchange.Set("code_verifier", verifier)
	st, tok := token(exchange)
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if st != 200 || access == "" || refresh == "" {
		t.Fatalf("token exchange: %d %v", st, tok)
	}
	if st, _ := token(exchange); st != 400 {
		t.Errorf("code reuse should fail, got %d", st)
	}

	call := func(bearer string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, _ := http.DefaultClient.Do(req)
		return resp.StatusCode
	}
	if c := call(access); c != 200 {
		t.Errorf("OAuth access token rejected: %d", c)
	}
	if c := call(strings.Repeat("s", 32)); c != 200 {
		t.Errorf("static token rejected: %d", c)
	}
	if c := call("nope"); c != 401 {
		t.Errorf("garbage token accepted: %d", c)
	}

	st, tok2 := token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if st != 200 || tok2["access_token"] == "" || tok2["refresh_token"] == refresh {
		t.Fatalf("refresh: %d %v", st, tok2)
	}
	if st, _ := token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); st != 400 {
		t.Errorf("refresh tokens should rotate, old one still worked: %d", st)
	}
}
