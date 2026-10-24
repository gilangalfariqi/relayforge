package secrets

import (
	"bytes"
	"testing"
)

func TestSealOpenAndAADBinding(t *testing.T) {
	kek := bytes.Repeat([]byte{7}, 32)
	box, err := New(kek, 3)
	if err != nil {
		t.Fatal(err)
	}
	aad := AAD("t1", "e1", "s1", 3)
	ct, nonce, err := box.Seal([]byte("whsec_example"), aad)
	if err != nil {
		t.Fatal(err)
	}
	got, err := box.Open(ct, nonce, aad)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "whsec_example" {
		t.Fatalf("plaintext = %q", got)
	}
	if _, err := box.Open(ct, nonce, AAD("t2", "e1", "s1", 3)); err == nil {
		t.Fatal("ciphertext opened with different AAD")
	}
	if _, err := box.Open(ct, nonce, aad[:len(aad)-1]); err == nil {
		t.Fatal("ciphertext opened with truncated AAD")
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New(make([]byte, 31), 1); err == nil {
		t.Fatal("accepted 31-byte key")
	}
	if _, err := New(make([]byte, 32), 0); err == nil {
		t.Fatal("accepted version 0")
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	box, _ := New(bytes.Repeat([]byte{1}, 32), 1)
	ad := AAD("t", "e", "s", 1)
	ct1, n1, err := box.Seal([]byte("same"), ad)
	if err != nil {
		t.Fatal(err)
	}
	ct2, n2, err := box.Seal([]byte("same"), ad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(n1, n2) || bytes.Equal(ct1, ct2) {
		t.Fatal("nonce/ciphertext reused")
	}
}
