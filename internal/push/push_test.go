// SPDX-License-Identifier: GPL-3.0-or-later

package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// browser is the receiving side of a subscription.
type browser struct {
	key  *ecdh.PrivateKey
	auth []byte
}

func newBrowser(t *testing.T) browser {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return browser{key: key, auth: []byte("0123456789abcdef")}
}

func (b browser) subscription(endpoint string) Subscription {
	return Subscription{Endpoint: endpoint, Keys: Keys{P256dh: b64.EncodeToString(b.key.PublicKey().Bytes()), Auth: b64.EncodeToString(b.auth)}}
}

// decrypt opens an aes128gcm body the way a browser does (RFC 8291).
func (b browser) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("body is %d bytes", len(body))
	}
	salt, rs, idLen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != recordSize || idLen != 65 {
		t.Fatalf("record size %d, key id length %d", rs, idLen)
	}
	serverPublic, ciphertext := body[21:21+idLen], body[21+idLen:]
	serverKey, err := ecdh.P256().NewPublicKey(serverPublic)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := b.key.ECDH(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	keyInfo := slices.Concat([]byte("WebPush: info\x00"), b.key.PublicKey().Bytes(), serverPublic)
	ikm, _ := hkdf.Key(sha256.New, shared, b.auth, string(keyInfo), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(plaintext) == 0 || plaintext[len(plaintext)-1] != 2 {
		t.Fatalf("plaintext lacks the last-record delimiter: %q", plaintext)
	}
	return plaintext[:len(plaintext)-1]
}

func openService(t *testing.T, dir string) *Service {
	t.Helper()
	s, err := Open(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEncryptRoundTrip(t *testing.T) {
	b := newBrowser(t)
	body, err := encrypt(b.subscription("https://push.example/1"), []byte(`{"title":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.decrypt(t, body); string(got) != `{"title":"hi"}` {
		t.Fatalf("decrypted %q", got)
	}
}

// verifyVAPID checks the Authorization header against the service's key
// and returns the token claims.
func verifyVAPID(t *testing.T, s *Service, header string) map[string]any {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	token, key, ok2 := strings.Cut(rest, ", k=")
	if !ok || !ok2 || key != s.PublicKey() {
		t.Fatalf("Authorization = %q", header)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	raw, _ := b64.DecodeString(key)
	public, err := ecdsa.ParseUncompressedPublicKey(s.key.Curve, raw)
	if err != nil {
		t.Fatal(err)
	}
	signature, _ := b64.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, sig := new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])
	if len(signature) != 64 || !ecdsa.Verify(public, digest[:], r, sig) {
		t.Fatal("VAPID signature does not verify")
	}
	var claims map[string]any
	payload, _ := b64.DecodeString(parts[1])
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestSendDeliversAndForgetsGoneSubscriptions(t *testing.T) {
	b := newBrowser(t)
	var mu sync.Mutex
	var delivered []Message
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusGone)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m Message
		if err := json.Unmarshal(b.decrypt(t, body), &m); err != nil {
			t.Error(err)
		}
		mu.Lock()
		delivered = append(delivered, m)
		mu.Unlock()
		if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "600" || r.Header.Get("Urgency") != "high" {
			t.Errorf("headers = %v", r.Header)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer service.Close()

	dir := t.TempDir()
	s := openService(t, dir)
	s.client = service.Client()
	claims := verifyVAPID(t, s, func() string {
		token, err := s.vapidToken(service.URL, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return "vapid t=" + token + ", k=" + s.PublicKey()
	}())
	if claims["aud"] != service.URL || claims["sub"] != Subject {
		t.Errorf("claims = %v", claims)
	}
	for _, path := range []string{"/live", "/gone"} {
		if err := s.Subscribe(b.subscription(service.URL + path)); err != nil {
			t.Fatal(err)
		}
	}

	s.Send(t.Context(), Message{Title: "Approve Bash?", Body: "ls", Tag: "approval-1", TTL: 10 * time.Minute, Urgent: true})

	if len(delivered) != 1 || delivered[0].Title != "Approve Bash?" || delivered[0].Tag != "approval-1" {
		t.Fatalf("delivered = %+v", delivered)
	}
	reopened := openService(t, dir)
	if len(reopened.subscriptions) != 1 || reopened.subscriptions[0].Endpoint != service.URL+"/live" {
		t.Fatalf("stored subscriptions = %+v", reopened.subscriptions)
	}
	if reopened.PublicKey() != s.PublicKey() {
		t.Fatal("VAPID key changed across Open")
	}
}

func TestSendChecksVAPIDAudience(t *testing.T) {
	b := newBrowser(t)
	var header string
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer service.Close()
	s := openService(t, t.TempDir())
	s.client = service.Client()
	s.SendTo(t.Context(), b.subscription(service.URL+"/push/abc?x=1"), Message{Title: "hi"})
	claims := verifyVAPID(t, s, header)
	if exp, _ := claims["exp"].(float64); claims["aud"] != service.URL || time.Until(time.Unix(int64(exp), 0)) > 24*time.Hour {
		t.Fatalf("claims = %v", claims)
	}
}

func TestSubscribeValidates(t *testing.T) {
	s := openService(t, t.TempDir())
	good := newBrowser(t).subscription("https://push.example/1")
	bad := []Subscription{
		{Endpoint: "http://push.example/1", Keys: good.Keys},
		{Endpoint: good.Endpoint, Keys: Keys{P256dh: "AAAA", Auth: good.Keys.Auth}},
		{Endpoint: good.Endpoint, Keys: Keys{P256dh: good.Keys.P256dh, Auth: "c2hvcnQ"}},
	}
	for _, sub := range bad {
		if err := s.Subscribe(sub); err == nil {
			t.Errorf("Subscribe(%+v) accepted", sub)
		}
	}
	if err := s.Subscribe(good); err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe(good); err != nil || len(s.subscriptions) != 1 {
		t.Fatalf("resubscribing kept %d subscriptions, err %v", len(s.subscriptions), err)
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("subscriptions file mode %v, err %v", info.Mode().Perm(), err)
	}
}

func TestRefusesSharedKeyFile(t *testing.T) {
	dir := t.TempDir()
	openService(t, dir)
	if err := os.Chmod(filepath.Join(dir, "vapid-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("Open with a shared key file: err = %v", err)
	}
}

func TestHTTPEndpoints(t *testing.T) {
	s := openService(t, t.TempDir())
	mux := http.NewServeMux()
	s.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/push/key")
	if err != nil {
		t.Fatal(err)
	}
	var key struct{ Key string }
	json.NewDecoder(resp.Body).Decode(&key)
	resp.Body.Close()
	if key.Key != s.PublicKey() {
		t.Fatalf("key = %q", key.Key)
	}

	sub := newBrowser(t).subscription("https://push.example/1")
	body, _ := json.Marshal(sub)
	for _, tc := range []struct {
		method, body string
		want         int
	}{
		{http.MethodPost, `{"endpoint":"http://x"}`, http.StatusBadRequest},
		{http.MethodPost, string(body), http.StatusNoContent},
		{http.MethodDelete, string(body), http.StatusNoContent},
	} {
		req, _ := http.NewRequest(tc.method, server.URL+"/push/subscriptions", strings.NewReader(tc.body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.body, resp.StatusCode, tc.want)
		}
	}
	if len(s.subscriptions) != 0 {
		t.Fatalf("subscriptions after DELETE = %+v", s.subscriptions)
	}
}
