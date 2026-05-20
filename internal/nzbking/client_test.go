package nzbking

import "testing"

// Pins the nzbking row parser against a realistic page snippet so a future
// HTML refactor on nzbking.com shows up here before showing up in the
// queue as "no NZB results".
func TestParseResults_TwoRows(t *testing.T) {
	body := `<table>
<tr><td>
<input type="checkbox" name="cb0" />
Daniel.Schinhofen-Foundation.mp3.nzb
<a href="/nzb:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/">NZB</a>
<a href="/details:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/">Details</a>
parts: 100/100 size: 256 MB <a href="/poster/somebody">somebody</a> a.b.audio 12d
</td></tr>
<tr><td>
<input type="checkbox" name="cb1" />
Alistair.MacLean-Partisans.mp3.nzb
<a href="/nzb:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/">NZB</a>
<a href="/details:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/">Details</a>
parts: 50/50 size: 128 MB <a href="/poster/other">other</a> a.b.boneless 30d
PASSWORD PROTECTED
</td></tr>
Query time: 0.1s
</table>`
	got := parseResults(body)
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2; got %+v", len(got), got)
	}
	if got[0].NZBURL != "https://nzbking.com/nzb:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/" {
		t.Errorf("row 0 NZBURL = %q", got[0].NZBURL)
	}
	if got[0].Size != "256 MB" {
		t.Errorf("row 0 Size = %q", got[0].Size)
	}
	if got[0].Group != "a.b.audio" {
		t.Errorf("row 0 Group = %q", got[0].Group)
	}
	if got[0].Age != "12d" {
		t.Errorf("row 0 Age = %q", got[0].Age)
	}
	if got[0].PasswordProtected {
		t.Errorf("row 0 should not be PW-protected")
	}
	if !got[1].PasswordProtected {
		t.Errorf("row 1 should be PW-protected")
	}
	if got[1].Group != "a.b.boneless" {
		t.Errorf("row 1 Group = %q", got[1].Group)
	}
}

func TestParseResults_EmptyBody(t *testing.T) {
	if got := parseResults(`<html><body>No results.</body></html>`); len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
