package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// SessionTTL is how long a login session lasts.
const SessionTTL = 7 * 24 * time.Hour

// HashPassword hashes with bcrypt (cost 10 is plenty for a small panel).
func HashPassword(pw string) (string, error) {
	if len(pw) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}
	if len(pw) > 128 {
		return "", fmt.Errorf("password too long")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), 10)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword compares a plaintext password against a bcrypt hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// NewToken returns a fresh 256-bit random session token (hex).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashToken is the storage form of a session token.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// ConstantTimeEqual is a small wrapper for token comparisons.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
