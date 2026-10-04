package pg

/*
MIT License

Copyright (c) 2026 Shane

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
*/

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// scramIterations matches the server's default scram_iterations.
const scramIterations = 4096

// ScramVerifier returns the SCRAM-SHA-256 verifier PostgreSQL stores for a
// password. Sending the verifier instead of the password keeps the plain text
// out of the server's statement log.
func ScramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return scramVerifier(password, salt, scramIterations)
}

func scramVerifier(password string, salt []byte, iter int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	enc := base64.StdEncoding
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iter, enc.EncodeToString(salt), enc.EncodeToString(stored[:]), enc.EncodeToString(serverKey)), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// ScramMatches reports whether a stored verifier was made from password, so
// the agent rewrites a role's password only when it really changed.
func ScramMatches(verifier, password string) bool {
	rest, ok := strings.CutPrefix(verifier, "SCRAM-SHA-256$")
	if !ok {
		return false
	}
	iterSalt, _, ok := strings.Cut(rest, "$")
	if !ok {
		return false
	}
	iterS, saltB64, ok := strings.Cut(iterSalt, ":")
	if !ok {
		return false
	}
	iter, err := strconv.Atoi(iterS)
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return false
	}
	want, err := scramVerifier(password, salt, iter)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(verifier)) == 1
}
