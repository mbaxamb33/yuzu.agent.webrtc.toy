package auth

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
    "encoding/hex"
    "errors"
    "fmt"
    "strconv"
    "strings"
    "time"
)

var (
    ErrTokenFormat = errors.New("invalid token format")
    ErrTokenSig    = errors.New("invalid token signature")
    ErrTokenExp    = errors.New("token expired or not yet valid")
    ErrTokenSID    = errors.New("session id mismatch")
)

// GenerateWorkerToken builds a token string for a given session and expiry.
// Format: base64url(session_id + "." + exp_unix + "." + hex(hmac_sha256(secret, session_id+"."+exp)))
func GenerateWorkerToken(secret, sessionID string, expUnix int64) (string, error) {
    msg := sessionID + "." + strconv.FormatInt(expUnix, 10)
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(msg))
    sig := hex.EncodeToString(mac.Sum(nil))
    raw := msg + "." + sig
    return base64.RawURLEncoding.EncodeToString([]byte(raw)), nil
}

// ValidateWorkerToken parses and validates the token.
// Returns the embedded sessionID and exp.
func ValidateWorkerToken(secret, token, expectSessionID string, now time.Time, skewSeconds int) (string, int64, error) {
    b, err := base64.RawURLEncoding.DecodeString(token)
    if err != nil {
        return "", 0, ErrTokenFormat
    }
    parts := strings.Split(string(b), ".")
    if len(parts) != 3 {
        return "", 0, ErrTokenFormat
    }
    sid := parts[0]
    expStr := parts[1]
    sigHex := parts[2]
    exp, err := strconv.ParseInt(expStr, 10, 64)
    if err != nil {
        return "", 0, ErrTokenFormat
    }
    if expectSessionID != "" && sid != expectSessionID {
        return "", 0, ErrTokenSID
    }
    msg := sid + "." + expStr
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(msg))
    want := mac.Sum(nil)
    got, err := hex.DecodeString(sigHex)
    if err != nil {
        return "", 0, ErrTokenFormat
    }
    // constant-time compare
    if !hmac.Equal(want, got) {
        return "", 0, ErrTokenSig
    }
    // expiry with skew
    skew := time.Duration(skewSeconds) * time.Second
    n := now.Unix()
    if n < exp-int64(skew.Seconds()) || n > exp+int64(skew.Seconds()) {
        // Allow small skew around exp; typical semantics: token invalid if now > exp+skew
        if n > exp+int64(skew.Seconds()) {
            return "", 0, ErrTokenExp
        }
    }
    return sid, exp, nil
}

// Debug helper (not used in prod path).
func MustToken(secret, sessionID string, expUnix int64) string {
    t, err := GenerateWorkerToken(secret, sessionID, expUnix)
    if err != nil { panic(fmt.Sprintf("token error: %v", err)) }
    return t
}

