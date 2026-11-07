package smb

import (
	"bytes"
	"crypto/aes"
	"sync"
	"testing"

	"github.com/jfjallid/go-smb/smb/crypto/ccm"
)

// TestConcurrentCCMUse covers the contract the server relies on: one session
// encrypts outbound PDUs from the receive loop and from concurrent read
// workers, so one AEAD value must be safe for concurrent use.
func TestConcurrentCCMUse(t *testing.T) {
	ciph, err := aes.NewCipher([]byte("YELLOW SUBMARINE"))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("a PDU that two goroutines encrypt at the same time")

	const goroutines, rounds = 8, 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			nonce := make([]byte, aead.NonceSize())
			for n := 0; n < rounds; n++ {
				sealed := aead.Seal(nil, nonce, plaintext, nil)
				opened, err := aead.Open(nil, nonce, sealed, nil)
				if err != nil {
					t.Errorf("Open: %v", err)
					return
				}
				if !bytes.Equal(opened, plaintext) {
					t.Errorf("round trip = %q, want %q", opened, plaintext)
					return
				}
			}
		}()
	}
	wg.Wait()
}
