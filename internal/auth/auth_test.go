// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadOrCreateTokenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ui-token")
	token, created, err := LoadOrCreateToken(path)
	if err != nil || !created || len(token) < minTokenLength {
		t.Fatalf("first load = %q, %v, %v", token, created, err)
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	again, created, err := LoadOrCreateToken(path)
	if err != nil || created || again != token {
		t.Fatalf("second load = %q, %v, %v; want %q, false, nil", again, created, err, token)
	}
}

func TestLoadOrCreateTokenRejectsUnsafeFiles(t *testing.T) {
	tests := []struct {
		name, content string
		mode          os.FileMode
		want          string
	}{
		{"short token", "tooshort\n", 0o600, "shorter than"},
		{"readable by others", strings.Repeat("x", minTokenLength), 0o644, "accessible to other users"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tt.mode != 0o600 {
				t.Skip("file modes are not enforced on Windows")
			}
			path := filepath.Join(t.TempDir(), "ui-token")
			if err := os.WriteFile(path, []byte(tt.content), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadOrCreateToken(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

const testToken = "abcdefghijklmnopqrstuvwxyz234567"

func guarded() http.Handler {
	return Require(testToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
}

func TestRequireRejectsMissingOrWrongCredentials(t *testing.T) {
	tests := []struct {
		name, path, header string
		wantStatus         int
		wantLocation       string
	}{
		{"page without cookie", "/", "", http.StatusSeeOther, "/login"},
		{"events without cookie", "/events", "", http.StatusUnauthorized, ""},
		{"wrong bearer", "/events", "Bearer nope", http.StatusUnauthorized, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			guarded().ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus || rec.Header().Get("Location") != tt.wantLocation {
				t.Fatalf("got %d %q, want %d %q", rec.Code, rec.Header().Get("Location"), tt.wantStatus, tt.wantLocation)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Fatal("guarded content leaked")
			}
		})
	}
}

func TestRequireAcceptsBearer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	guarded().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "secret" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestLoginSetsCookieThatGrantsAccess(t *testing.T) {
	logins := map[string]*http.Request{
		"link": httptest.NewRequest(http.MethodGet, "/login?token="+testToken, nil),
		"form": func() *http.Request {
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"token": {testToken}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return req
		}(),
	}
	for name, req := range logins {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			guarded().ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
				t.Fatalf("login = %d %q", rec.Code, rec.Header().Get("Location"))
			}
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
				t.Fatalf("cookies = %+v", cookies)
			}
			page := httptest.NewRequest(http.MethodGet, "/", nil)
			page.AddCookie(cookies[0])
			rec = httptest.NewRecorder()
			guarded().ServeHTTP(rec, page)
			if rec.Code != http.StatusOK || rec.Body.String() != "secret" {
				t.Fatalf("page = %d %q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestLoginRejectsWrongToken(t *testing.T) {
	rec := httptest.NewRecorder()
	guarded().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login?token=wrong", nil))
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("got %d with cookies %v", rec.Code, rec.Result().Cookies())
	}
	rec = httptest.NewRecorder()
	guarded().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<form") {
		t.Fatalf("login page = %d", rec.Code)
	}
}

func TestRequireRefusesCrossOriginWrites(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/approvals/x", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: CookieName, Value: testToken})
	req.Header.Set("Sec-Fetch-Site", "same-site")
	rec := httptest.NewRecorder()
	guarded().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}
