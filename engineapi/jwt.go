package engineapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// jwt iat drift tolerance mandated by the Engine API authentication spec.
const jwtMaxDrift = 60 * time.Second

// JWTHandler enforces the Engine API's HS256 bearer-token authentication on
// every request before delegating to next.
func JWTHandler(secret []byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || !verifyJWT(secret, token, time.Now()) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func verifyJWT(secret []byte, token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if !decodeSegment(parts[0], &header) || header.Alg != "HS256" {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	var claims struct {
		Iat int64 `json:"iat"`
	}
	if !decodeSegment(parts[1], &claims) {
		return false
	}
	drift := now.Sub(time.Unix(claims.Iat, 0))
	if drift < 0 {
		drift = -drift
	}
	return drift <= jwtMaxDrift
}

func decodeSegment(seg string, v any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}
