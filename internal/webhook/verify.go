// Package webhook receives Gitea webhooks, verifies their HMAC signature,
// and normalizes the payload into a model.WebhookEvent.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Verify reports whether signatureHex is a valid HMAC-SHA256 of body keyed by
// secret. The comparison is constant-time via hmac.Equal. An empty or
// malformed signature returns false.
func Verify(body []byte, signatureHex, secret string) bool {
	if signatureHex == "" {
		return false
	}
	want, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := mac.Sum(nil)
	return hmac.Equal(got, want)
}
