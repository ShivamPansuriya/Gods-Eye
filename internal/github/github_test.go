package github

import "testing"

func TestSignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sig := Sign("s3cret", body)
	if !VerifySignature("s3cret", body, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature("other", body, sig) || VerifySignature("s3cret", []byte(`{}`), sig) || VerifySignature("", body, sig) || VerifySignature("s3cret", body, "sha1=abc") {
		t.Fatal("bad signature accepted")
	}
}
