package credential

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptAndBindAccessKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	ciphertext, nonce, err := Encrypt(key, "SFS-example", "secret-material")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("secret-material")) {
		t.Fatal("ciphertext contains plaintext secret")
	}
	got, err := Decrypt(key, "SFS-example", ciphertext, nonce)
	if err != nil || got != "secret-material" {
		t.Fatalf("Decrypt() = (%q, %v)", got, err)
	}
	if _, err := Decrypt(key, "SFS-other", ciphertext, nonce); err == nil {
		t.Fatal("ciphertext was accepted under a different access key ID")
	}
}
