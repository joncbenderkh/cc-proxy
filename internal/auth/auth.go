// SPDX-License-Identifier: GPL-3.0-or-later

// Package auth guards the web UI with a shared secret token, kept in a
// file readable only by its owner and exchanged once for a cookie.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CookieName holds the token in the browser after a login.
const CookieName = "cc_proxy_token"

// minTokenLength is the length of a generated token (128 bits, base32);
// shorter tokens read from a file are rejected.
const minTokenLength = 26

// cookieMaxAge is the longest lifetime browsers accept (400 days).
const cookieMaxAge = 400 * 24 * 60 * 60

// DefaultTokenFile is where the token lives unless --ui-token-file says
// otherwise.
func DefaultTokenFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cc-proxy", "ui-token"), nil
}

// LoadOrCreateToken reads the token at path, generating and storing a new
// one when the file does not exist yet.
func LoadOrCreateToken(path string) (token string, created bool, err error) {
	token, err = readToken(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return token, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", false, err
	}
	token = rand.Text()
	if _, err := file.WriteString(token + "\n"); err != nil {
		file.Close()
		return "", false, err
	}
	return token, true, file.Close()
}

func readToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("token file %s is accessible to other users (mode %v); run chmod 600 on it", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if len(token) < minTokenLength {
		return "", fmt.Errorf("token file %s holds a token shorter than %d characters", path, minTokenLength)
	}
	return token, nil
}

//go:embed login.html
var loginHTML []byte

// Require serves a login page at /login and passes every other request to
// next only when it carries the token, as the login cookie or as an
// Authorization bearer token. Unauthenticated page loads are sent to the
// login page; anything else gets 401. Cross-origin browser requests that
// change state are refused, since SameSite=Lax still sends the cookie from
// other ports of the same host.
func Require(token string, next http.Handler) http.Handler {
	valid := func(candidate string) bool {
		return subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1
	}
	login := func(w http.ResponseWriter, r *http.Request, candidate string) {
		if candidate == "" || !valid(candidate) {
			status := http.StatusOK
			if candidate != "" {
				status = http.StatusUnauthorized
			}
			serveLogin(w, status)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     CookieName,
			Value:    token,
			Path:     "/",
			MaxAge:   cookieMaxAge,
			HttpOnly: true,
			Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
			SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		login(w, r, r.URL.Query().Get("token"))
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		login(w, r, strings.TrimSpace(r.PostFormValue("token")))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if valid(bearerOrCookie(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="cc-proxy"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	return http.NewCrossOriginProtection().Handler(mux)
}

func bearerOrCookie(r *http.Request) string {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(bearer)
	}
	if cookie, err := r.Cookie(CookieName); err == nil {
		return cookie.Value
	}
	return ""
}

func serveLogin(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	w.Write(loginHTML)
}
