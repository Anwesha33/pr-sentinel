package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestParsePullRequestURL(t *testing.T) {
	cases := []struct {
		in     string
		owner  string
		repo   string
		number int
		wantOK bool
	}{
		{"https://github.com/Anwesha33/pr-sentinel/pull/12", "Anwesha33", "pr-sentinel", 12, true},
		{"http://github.com/a/b/pull/1", "a", "b", 1, true},
		{"github.com/a/b/pull/7", "a", "b", 7, true},
		{"a/b/pull/9", "a", "b", 9, true},
		// The fragment a browser leaves on when you copy from a comment.
		{"https://github.com/a/b/pull/3#discussion_r1", "a", "b", 3, true},
		{"https://github.com/a/b/issues/3", "", "", 0, false},
		{"https://github.com/a/b", "", "", 0, false},
		{"not a url", "", "", 0, false},
	}
	for _, c := range cases {
		owner, repo, number, err := ParsePullRequestURL(c.in)
		if c.wantOK != (err == nil) {
			t.Fatalf("%q: err = %v, wantOK = %v", c.in, err, c.wantOK)
		}
		if err == nil && (owner != c.owner || repo != c.repo || number != c.number) {
			t.Fatalf("%q -> %s/%s#%d, want %s/%s#%d", c.in, owner, repo, number, c.owner, c.repo, c.number)
		}
	}
}

func TestValidSignature(t *testing.T) {
	const secret = "s3cret"
	body := []byte(`{"action":"opened"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !ValidSignature(secret, body, good) {
		t.Fatal("a correct signature must verify")
	}
	if ValidSignature("wrong-secret", body, good) {
		t.Fatal("a signature from a different secret must not verify")
	}
	if ValidSignature(secret, []byte(`{"action":"closed"}`), good) {
		t.Fatal("a tampered body must not verify")
	}
	for _, bad := range []string{"", "sha1=abcd", good[:len(good)-2], "sha256=nothex"} {
		if ValidSignature(secret, body, bad) {
			t.Fatalf("malformed header %q must not verify", bad)
		}
	}
}
