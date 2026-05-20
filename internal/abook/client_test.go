package abook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractCSRFNonce_FromLoginPage(t *testing.T) {
	// Mirrors the live abook.link login HTML shape: hidden field with two
	// hex strings sits inside the SMF login form.
	body := `<html><body><form name="frmLogin" id="frmLogin" method="post">
<input type="text" name="user" /><input type="password" name="passwrd" />
<input type="hidden" name="e96b64aef521" value="641aeb649a0a2f63847b2894811799fc" />
</form></body></html>`
	name, value, ok := extractCSRFNonce(body)
	if !ok {
		t.Fatal("CSRF nonce not extracted")
	}
	if name != "e96b64aef521" {
		t.Errorf("nonce name = %q, want e96b64aef521", name)
	}
	if value != "641aeb649a0a2f63847b2894811799fc" {
		t.Errorf("nonce value = %q, want the 32-hex value", value)
	}
}

func TestExtractCSRFNonce_NoLoginForm(t *testing.T) {
	if _, _, ok := extractCSRFNonce(`<html>no login form here</html>`); ok {
		t.Fatal("expected ok=false when login form is absent")
	}
}

func TestLogin_HappyPath(t *testing.T) {
	var got struct {
		user, passwrd, nonce, nonceValue string
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.php", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "login":
			_, _ = w.Write([]byte(`<form name="frmLogin" id="frmLogin">
<input type="hidden" name="abc123def456" value="cafef00ddeadbeefcafef00ddeadbeef" />
</form>`))
		case "login2":
			_ = r.ParseForm()
			got.user = r.FormValue("user")
			got.passwrd = r.FormValue("passwrd")
			got.nonce = "abc123def456"
			got.nonceValue = r.FormValue("abc123def456")
			http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "post-login-session", Path: "/"})
			_, _ = w.Write([]byte("you are now successfully logged in"))
		default:
			// Logged-in marker: not the guest welcome string.
			_, _ = w.Write([]byte("Welcome, <strong>alice</strong>."))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(context.Background(), "alice@example.com", "hunter2"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got.user != "alice@example.com" || got.passwrd != "hunter2" {
		t.Errorf("submitted credentials: user=%q passwrd=%q", got.user, got.passwrd)
	}
	if got.nonceValue != "cafef00ddeadbeefcafef00ddeadbeef" {
		t.Errorf("CSRF nonce was not echoed back; got %q", got.nonceValue)
	}
	if !strings.Contains(c.CookieHeader(), "PHPSESSID=post-login-session") {
		t.Errorf("jar missing post-login cookie; CookieHeader=%q", c.CookieHeader())
	}
}

func TestLogin_BadCredentialsSurfacesError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/index.php", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "login":
			_, _ = w.Write([]byte(`<form name="frmLogin" id="frmLogin">
<input type="hidden" name="abc123def456" value="cafef00d" />
</form>`))
		case "login2":
			_, _ = w.Write([]byte("That username or password is incorrect."))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := New(srv.URL)
	err := c.Login(context.Background(), "alice@example.com", "wrong")
	if err == nil || !strings.Contains(err.Error(), "credentials rejected") {
		t.Fatalf("login should fail on bad creds; got %v", err)
	}
}

func TestSearch_ParsesAbookForumResults(t *testing.T) {
	// Two-result page in the format the live forum returns from
	// search_abook.php — closely mirrors the .ts reference scraper test
	// case so the regex is pinned against a representative shape.
	page := `<a href='./index.php?topic=12345&r=10' target='_blank'>Foundation - Isaac Asimov</a><br/>
       alice - <span class='boardname'>Sci-Fi</span> - <span class='posttime'>May 20</span><br/><br/>
<a href='./index.php?topic=67890&r=10' target='_blank'>Project Hail Mary</a><br/>
       bob - <span class='boardname'>Sci-Fi</span> - <span class='posttime'>Apr 03</span><br/><br/>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	hits, err := c.Search(context.Background(), "foundation")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].TopicID != 12345 || hits[0].Title != "Foundation - Isaac Asimov" || hits[0].Board != "Sci-Fi" {
		t.Errorf("first hit unexpected: %+v", hits[0])
	}
}

func TestFetchTopic_ExtractsSearchCodeAndPassword(t *testing.T) {
	// Topic that's already unlocked (no thank dance needed) and emits the
	// canonical "Hidden content:" block with two <code> sections — first
	// is the Usenet search code, second is the unpack password.
	page := `<html><body>
<b>Author:</b> Isaac Asimov<br>
<b>Read By:</b> Scott Brick<br>
<b>Copyright:</b> 1951<br>
<b>Total Duration:</b> 7:24:00<br>
<b>Total Size:</b> 256 MB<br>
<h6>Hidden content:</h6>
<p>Search:</p>
<code class="bbc_code">PW - foundation-abc123</code>
<p>Password:</p>
<code class="bbc_code">unpack-password-here</code>
<div class="moderatorbar">end</div>
</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	tc, err := c.FetchTopic(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if tc.Search != "PW - foundation-abc123" {
		t.Errorf("Search = %q", tc.Search)
	}
	if tc.Password != "unpack-password-here" {
		t.Errorf("Password = %q", tc.Password)
	}
	if tc.Author != "Isaac Asimov" || tc.Narrator != "Scott Brick" || tc.Year != "1951" {
		t.Errorf("metadata unexpected: %+v", tc)
	}
}

func TestFetchTopic_AutoThanks_ThenRefetches(t *testing.T) {
	// Topic returns the "must thank" marker on the first GET, then the
	// unlocked page after a thank-action GET. Asserts the second fetch
	// actually parses the hidden block.
	var thanked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery + " " + r.URL.Path
		if strings.Contains(raw, "action=thank") {
			thanked++
			return
		}
		if thanked == 0 {
			_, _ = w.Write([]byte(`<html><body>
You must thank this post to see the content
<a href="index.php?action=thank;topic=42">Thanks</a>
</body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<html><body>
<h6>Hidden content:</h6>
<code class="bbc_code">unlocked-search-code</code>
<div class="moderatorbar">end</div>
</body></html>`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	tc, err := c.FetchTopic(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if thanked == 0 {
		t.Fatal("auto-thank did not fire")
	}
	if tc.Search != "unlocked-search-code" {
		t.Errorf("Search = %q after thank", tc.Search)
	}
}
