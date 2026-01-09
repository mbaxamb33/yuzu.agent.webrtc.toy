package auth

import (
    "testing"
    "time"
)

func TestGenerateAndValidateToken(t *testing.T) {
    sec := "secret123"
    sid := "abc"
    exp := time.Now().Add(5 * time.Minute).Unix()

    tok, err := GenerateWorkerToken(sec, sid, exp)
    if err != nil { t.Fatalf("gen: %v", err) }

    gotSID, gotExp, err := ValidateWorkerToken(sec, tok, sid, time.Now(), 60)
    if err != nil { t.Fatalf("validate: %v", err) }
    if gotSID != sid || gotExp != exp {
        t.Fatalf("mismatch: %s/%d", gotSID, gotExp)
    }
}

func TestBadSignature(t *testing.T) {
    sec := "secret123"
    sid := "abc"
    exp := time.Now().Add(5 * time.Minute).Unix()
    tok, _ := GenerateWorkerToken(sec, sid, exp)

    // flip a char
    if tok[0] == 'A' {
        tok = "B" + tok[1:]
    } else {
        tok = "A" + tok[1:]
    }

    _, _, err := ValidateWorkerToken(sec, tok, sid, time.Now(), 60)
    if err == nil {
        t.Fatalf("expected error for bad token")
    }
}

