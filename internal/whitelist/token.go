package whitelist

import (
	"strconv"
	"strings"
)

func TenantSelector(tenantID string) (string, bool) {
	if !canonicalUint(tenantID) {
		return "", false
	}
	return "tenant:" + tenantID, true
}

func UserSelector(userID string) (string, bool) {
	if !canonicalUint(userID) {
		return "", false
	}
	return "user:" + userID, true
}

func ValidSelector(value string) bool {
	if raw, ok := strings.CutPrefix(value, "tenant:"); ok {
		_, valid := TenantSelector(raw)
		return valid
	}
	if raw, ok := strings.CutPrefix(value, "user:"); ok {
		_, valid := UserSelector(raw)
		return valid
	}
	return false
}

func canonicalUint(value string) bool {
	if value == "" {
		return false
	}
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == value
}
