// SPDX-License-Identifier: GPL-3.0-or-later

// Package push sends Web Push notifications (RFC 8030) to the browsers
// that subscribed from the web page. Each message is encrypted for its
// subscription (RFC 8291), and a VAPID key identifies the server to the
// push services (RFC 8292).
package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Subject names the sender in VAPID tokens, so a push service can reach
// the project that runs the server.
const Subject = "https://github.com/joncbenderkh/cc-proxy"

// maxSubscriptions bounds the stored subscriptions; the oldest go first.
const maxSubscriptions = 32

// recordSize is the aes128gcm record size; each message is one record.
const recordSize = 4096

// maxPayload is the longest message that fits one record next to the
// header, the padding delimiter and the authentication tag.
const maxPayload = recordSize - 86 - 1 - 16

var b64 = base64.RawURLEncoding

// Message is one notification. The service worker shows Title and Body,
// replaces an earlier notification with the same Tag, and opens URL,
// relative to the page, when the notification is tapped.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Tag   string `json:"tag,omitempty"`
	URL   string `json:"url,omitempty"`
	// TTL is how long the push service keeps an undelivered message.
	TTL time.Duration `json:"-"`
	// Urgent asks the push service to wake a device that saves power.
	Urgent bool `json:"-"`
}

// Subscription is a browser's push endpoint and keys, as
// PushSubscription.toJSON gives them.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     Keys   `json:"keys"`
}

// Keys holds the browser's P-256 public key and authentication secret,
// both base64url encoded.
type Keys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// Service keeps the VAPID key and the subscriptions and sends messages.
type Service struct {
	key       *ecdsa.PrivateKey
	publicKey string
	path      string
	client    *http.Client
	logger    *slog.Logger

	mu            sync.Mutex
	subscriptions []Subscription
}

// Open loads the VAPID key and the subscriptions kept in dir, creating the
// key on first use. Both files are readable only by their owner.
func Open(dir string, logger *slog.Logger) (*Service, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(dir, "vapid-key"))
	if err != nil {
		return nil, err
	}
	public, err := key.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	s := &Service{
		key:       key,
		publicKey: b64.EncodeToString(public),
		path:      filepath.Join(dir, "push-subscriptions.json"),
		client:    &http.Client{Timeout: 30 * time.Second},
		logger:    logger,
	}
	data, err := readPrivate(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.subscriptions); err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	return s, nil
}

// PublicKey is the VAPID public key the browser needs to subscribe, as
// base64url of the uncompressed point.
func (s *Service) PublicKey() string {
	return s.publicKey
}

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := readPrivate(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s holds no PEM private key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%s holds no P-256 ECDSA key", path)
	}
	return key, nil
}

func createKey(path string) (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(file, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		file.Close()
		return nil, err
	}
	return key, file.Close()
}

func readPrivate(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible to other users (mode %v); run chmod 600 on it", path, info.Mode().Perm())
	}
	return os.ReadFile(path)
}

// Subscribe stores sub, replacing an earlier one with the same endpoint.
func (s *Service) Subscribe(sub Subscription) error {
	if err := sub.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscriptions = slices.DeleteFunc(s.subscriptions, func(old Subscription) bool { return old.Endpoint == sub.Endpoint })
	s.subscriptions = append(s.subscriptions, sub)
	if extra := len(s.subscriptions) - maxSubscriptions; extra > 0 {
		s.subscriptions = slices.Delete(s.subscriptions, 0, extra)
	}
	return s.saveLocked()
}

// Unsubscribe forgets the subscription with the given endpoint.
func (s *Service) Unsubscribe(endpoint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := slices.DeleteFunc(slices.Clone(s.subscriptions), func(sub Subscription) bool { return sub.Endpoint == endpoint })
	if len(kept) == len(s.subscriptions) {
		return nil
	}
	s.subscriptions = kept
	return s.saveLocked()
}

// saveLocked writes the subscriptions to a temporary file and renames it
// over the old one, so a crash never leaves a partial file.
func (s *Service) saveLocked() error {
	data, err := json.Marshal(s.subscriptions)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), s.path)
}

func (sub Subscription) validate() error {
	endpoint, err := url.Parse(sub.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		return errors.New("endpoint must be an https URL")
	}
	if _, err := sub.browserKey(); err != nil {
		return err
	}
	if _, err := sub.authSecret(); err != nil {
		return err
	}
	return nil
}

func (sub Subscription) browserKey() (*ecdh.PublicKey, error) {
	raw, err := decode(sub.Keys.P256dh)
	if err != nil {
		return nil, errors.New("keys.p256dh must be base64url")
	}
	key, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return nil, errors.New("keys.p256dh must be an uncompressed P-256 point")
	}
	return key, nil
}

func (sub Subscription) authSecret() ([]byte, error) {
	secret, err := decode(sub.Keys.Auth)
	if err != nil || len(secret) != 16 {
		return nil, errors.New("keys.auth must be 16 bytes of base64url")
	}
	return secret, nil
}

// decode accepts base64url with or without padding.
func decode(text string) ([]byte, error) {
	return b64.DecodeString(strings.TrimRight(text, "="))
}

// Send delivers m to every subscription and forgets those the push
// service reports as gone.
func (s *Service) Send(ctx context.Context, m Message) {
	s.mu.Lock()
	subscriptions := slices.Clone(s.subscriptions)
	s.mu.Unlock()
	for _, sub := range subscriptions {
		s.SendTo(ctx, sub, m)
	}
}

// SendTo delivers m to one subscription, forgetting it when the push
// service reports it as gone.
func (s *Service) SendTo(ctx context.Context, sub Subscription, m Message) {
	status, err := s.deliver(ctx, sub, m)
	host := ""
	if endpoint, err := url.Parse(sub.Endpoint); err == nil {
		host = endpoint.Host
	}
	switch {
	case err != nil:
		s.logger.Warn("push failed", "push_host", host, "err", err)
	case status == http.StatusNotFound || status == http.StatusGone:
		s.logger.Info("push subscription gone", "push_host", host, "status", status)
		if err := s.Unsubscribe(sub.Endpoint); err != nil {
			s.logger.Warn("push unsubscribe failed", "err", err)
		}
	case status >= 300:
		s.logger.Warn("push rejected", "push_host", host, "status", status)
	default:
		s.logger.Info("push", "push_host", host, "status", status, "tag", m.Tag)
	}
}

func (s *Service) deliver(ctx context.Context, sub Subscription, m Message) (int, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	if len(payload) > maxPayload {
		return 0, fmt.Errorf("message is %d bytes, longer than %d", len(payload), maxPayload)
	}
	body, err := encrypt(sub, payload)
	if err != nil {
		return 0, err
	}
	endpoint, err := url.Parse(sub.Endpoint)
	if err != nil {
		return 0, err
	}
	token, err := s.vapidToken(endpoint.Scheme+"://"+endpoint.Host, time.Now().Add(12*time.Hour))
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "vapid t="+token+", k="+s.publicKey)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(m.TTL.Seconds())))
	if m.Urgent {
		req.Header.Set("Urgency", "high")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// vapidToken signs a JWT (ES256) for the push service at audience.
func (s *Service) vapidToken(audience string, expires time.Time) (string, error) {
	claims, err := json.Marshal(map[string]any{"aud": audience, "exp": expires.Unix(), "sub": Subject})
	if err != nil {
		return "", err
	}
	signed := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	r, sig, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	sig.FillBytes(signature[32:])
	return signed + "." + b64.EncodeToString(signature), nil
}

// encrypt seals plaintext for sub as a single aes128gcm record (RFC 8291).
func encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	browserKey, err := sub.browserKey()
	if err != nil {
		return nil, err
	}
	authSecret, err := sub.authSecret()
	if err != nil {
		return nil, err
	}
	serverKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := serverKey.ECDH(browserKey)
	if err != nil {
		return nil, err
	}
	serverPublic := serverKey.PublicKey().Bytes()
	keyInfo := slices.Concat([]byte("WebPush: info\x00"), browserKey.Bytes(), serverPublic)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header := binary.BigEndian.AppendUint32(slices.Clone(salt), recordSize)
	header = append(header, byte(len(serverPublic)))
	header = append(header, serverPublic...)
	// The 0x02 delimiter marks the last (and only) record.
	return gcm.Seal(header, nonce, append(slices.Clone(plaintext), 2), nil), nil
}
