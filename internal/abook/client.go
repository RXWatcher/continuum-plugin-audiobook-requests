// Package abook scrapes abook.link (a Simple Machines Forum running
// audiobook NZB posts) to discover the Usenet "search code" + unpack
// password that a downstream nzbking.com lookup needs.
//
// This package is a Go port of /opt/librarymanager/scripts/abook-search.ts.
// Compared to the script:
//   - the cookie is minted from email+password via the SMF login flow
//     (the script took the cookie as a pre-baked env var);
//   - the SMF CSRF token (a randomly named hidden field that rotates with
//     PHPSESSID) is fetched per login attempt, not assumed;
//   - HTTP is bounded by short context-derived timeouts so a hung forum
//     can't pin the consumer goroutine.
package abook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultTimeout  = 15 * time.Second
	maxResponseSize = 8 << 20
)

// SMF embeds a randomly named hidden field in the login form ("nonce") whose
// presence is required by /action=login2. The name + value rotate per
// PHPSESSID so we have to fetch the login page first and parse them out.
// The field's name and value are both 12-hex-char strings.
var smfHiddenFieldRe = regexp.MustCompile(`name="([0-9a-f]{6,16})"\s+value="([0-9a-f]{8,64})"`)

// extractCSRFNonce pulls the first SMF hidden-name pair out of the login form
// HTML. Returns ("", "", false) if the page doesn't look like SMF's login
// page — caller should treat that as a fatal "login form changed".
func extractCSRFNonce(body string) (name, value string, ok bool) {
	// Narrow to the login form so we don't grab unrelated 8-hex strings
	// from elsewhere on the page.
	formStart := strings.Index(body, `name="frmLogin"`)
	if formStart < 0 {
		return "", "", false
	}
	formEnd := strings.Index(body[formStart:], "</form>")
	if formEnd < 0 {
		return "", "", false
	}
	form := body[formStart : formStart+formEnd]
	m := smfHiddenFieldRe.FindStringSubmatch(form)
	if len(m) != 3 {
		return "", "", false
	}
	return m[1], m[2], true
}

// Client wraps a per-session cookie jar + HTTP client tuned for abook.link.
// One Client maps to one logged-in session. The plugin holds one Client at
// a time; re-Login replaces the cookies in place.
type Client struct {
	baseURL string
	jar     *cookiejar.Jar
	hc      *http.Client
}

// New builds an empty (logged-out) Client. baseURL should point at the
// forum root, e.g. https://abook.link/book — same convention the script
// uses.
func New(baseURL string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("abook: base url is required")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("abook: cookie jar: %w", err)
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		jar:     jar,
		hc: &http.Client{
			Jar:     jar,
			Timeout: defaultTimeout,
		},
	}, nil
}

// CookieHeader serialises the current jar into a single Cookie header value
// suitable for re-hydrating a Client later (or for the operator to paste
// somewhere). Returns "" if the jar is empty.
func (c *Client) CookieHeader() string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return ""
	}
	cookies := c.jar.Cookies(u)
	if len(cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

// SetCookieHeader restores a cookie string into the jar. Used to rehydrate
// the Client from a previously-stored cookie without re-running Login.
// Format is the standard "name=value; name=value" header form.
func (c *Client) SetCookieHeader(header string) error {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return err
	}
	if header == "" {
		return nil
	}
	cookies := []*http.Cookie{}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:  strings.TrimSpace(part[:eq]),
			Value: strings.TrimSpace(part[eq+1:]),
			Path:  "/",
		})
	}
	c.jar.SetCookies(u, cookies)
	return nil
}

// ErrLoginFailed signals SMF rejected the credentials (or the login form
// shape changed in a way we couldn't recover from). Operator-visible.
var ErrLoginFailed = errors.New("abook: login failed")

// LoggedIn reports whether the most recent request from this session
// observed a logged-in marker. Cheap probe used by the admin Test button
// after a Login round-trip.
func (c *Client) LoggedIn(ctx context.Context) (bool, error) {
	body, err := c.get(ctx, c.baseURL+"/index.php")
	if err != nil {
		return false, err
	}
	// SMF renders "Welcome, <strong>Guest</strong>" for logged-out, replaces
	// "Guest" with the actual username post-login. Cheap + version-stable
	// across the SMF themes I've seen on abook.link.
	if strings.Contains(body, "Welcome, <strong>Guest</strong>") {
		return false, nil
	}
	return true, nil
}

// Login performs the SMF login dance: GET the login page to capture the
// rotating CSRF hidden field name + PHPSESSID, then POST credentials.
// On success the session cookie is stored in the jar; CookieHeader() returns
// the new value.
func (c *Client) Login(ctx context.Context, email, password string) error {
	if email == "" || password == "" {
		return fmt.Errorf("%w: email and password are required", ErrLoginFailed)
	}
	loginPage, err := c.get(ctx, c.baseURL+"/index.php?action=login")
	if err != nil {
		return fmt.Errorf("fetch login page: %w", err)
	}
	nonceName, nonceValue, ok := extractCSRFNonce(loginPage)
	if !ok {
		return fmt.Errorf("%w: SMF login form shape changed (no CSRF nonce found)", ErrLoginFailed)
	}
	form := url.Values{
		"user":         {email},
		"passwrd":      {password}, // SMF accepts plaintext server-side; the JS hash is optional client-side hardening
		"cookielength": {"-1"},     // SMF: -1 = forever (until logout)
		nonceName:      {nonceValue},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/index.php?action=login2", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("login2 POST: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read login2 body: %w", err)
	}
	// SMF login responses are 200 with a body whose content varies:
	//   - success page contains "you are now successfully logged in"
	//     or redirects to the forum root rendering the logged-in shell;
	//   - failure page contains "incorrect" / "wrong" / "couldn't find"
	//     somewhere in an inline error block.
	bodyLower := strings.ToLower(string(body))
	if strings.Contains(bodyLower, "incorrect") || strings.Contains(bodyLower, "password is incorrect") {
		return fmt.Errorf("%w: credentials rejected", ErrLoginFailed)
	}
	// Independent verification: hit the forum root and look for the "Guest"
	// marker. Defends against false positives where the login page rendered
	// without an explicit error but the session didn't actually upgrade.
	in, err := c.LoggedIn(ctx)
	if err != nil {
		return fmt.Errorf("verify login: %w", err)
	}
	if !in {
		return fmt.Errorf("%w: session did not upgrade after login2", ErrLoginFailed)
	}
	return nil
}

func (c *Client) get(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("abook %s: HTTP %d", rawURL, resp.StatusCode)
	}
	return string(body), nil
}
