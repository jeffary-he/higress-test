package whitelist

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Java/Go contract: remove one exact "Bearer " prefix; never trim or alter case.
func Digest(authorization string) (string, bool) {
	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), true
}

func ValidDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
