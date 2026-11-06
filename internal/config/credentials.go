package config

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// PasswordHash returns the bcrypt hash of the configured password.
//
// The configuration layer owns the credentials and turns the plaintext into a
// hash once, so the authentication service never sees the plaintext and never
// reads a file to obtain it. The result is empty when no password is
// configured.
func (c Config) PasswordHash() ([]byte, error) {
	if c.Password == "" {
		return nil, nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
	if err != nil {
		// The password length is checked by Validate, so this only fails
		// for a password the validator accepted but bcrypt cannot hash.
		return nil, invalid("password", errors.New("could not be hashed"))
	}
	return hash, nil
}
