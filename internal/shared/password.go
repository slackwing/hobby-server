// Argon2id password hashing for the shared auth system.
//
// Uses the OWASP-recommended argon2id parameters (19 MiB, t=2, p=1) and
// the standard PHC string format, so hashes are self-describing and the
// parameters can be raised later without invalidating existing hashes.
// Unlike bcrypt there is no 72-byte truncation — long master-style
// passphrases are hashed in full.
package shared

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime      = 2
	argonMemoryKiB = 19456 // 19 MiB
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32

	// MinPasswordLen is the only password rule. No composition
	// requirements — length is what matters.
	MinPasswordLen = 8
)

// dummyPHC is a valid argon2id hash of an unguessable throwaway value,
// used to keep failed-login timing constant when the username doesn't
// exist or has no password set.
var dummyPHC = func() string {
	h, err := HashPassword("dummy-timing-equalizer")
	if err != nil {
		panic(err)
	}
	return h
}()

func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func VerifyPassword(password, phc string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t, p uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// VerifyDummy burns the same work as a real verification; call it on
// the failure paths that would otherwise return early.
func VerifyDummy(password string) {
	VerifyPassword(password, dummyPHC)
}

func ValidatePassword(password string) error {
	if len(password) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	return nil
}
