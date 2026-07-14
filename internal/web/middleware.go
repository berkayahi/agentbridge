package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

const csrfCookieName = "agentbridge_csrf"

func authorizeServeRequest(peer, scheme, identity string, serveMode bool, allowed map[string]struct{}) bool {
	ip := net.ParseIP(strings.TrimSpace(peer))
	if !serveMode || ip == nil || !ip.IsLoopback() || scheme != "https" {
		return false
	}
	_, ok := allowed[strings.TrimSpace(identity)]
	return ok
}

func (s *Server) securityMiddleware(c fiber.Ctx) error {
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderXFrameOptions, "DENY")
	c.Set("Referrer-Policy", "no-referrer")
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'")
	c.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	c.Set("X-Request-ID", randomToken(12))
	identity := s.identity(c)
	if !authorizeServeRequest(s.peerIP(c), s.scheme(c), identity, s.config.ServeMode, s.allowed) {
		return fiber.NewError(fiber.StatusForbidden)
	}
	c.Locals("tailscale_identity", identity)
	return c.Next()
}

func (s *Server) issueCSRF(c fiber.Ctx) error {
	identity, _ := c.Locals("tailscale_identity").(string)
	token := s.csrf.issue(identity)
	c.Cookie(&fiber.Cookie{Name: csrfCookieName, Value: token, Path: "/api/v1/auth", Secure: true, HTTPOnly: false, SameSite: fiber.CookieSameSiteStrictMode, MaxAge: 300})
	return c.JSON(fiber.Map{"token": token})
}

func (s *Server) requireCSRF(c fiber.Ctx) error {
	identity, _ := c.Locals("tailscale_identity").(string)
	header := c.Get("X-CSRF-Token")
	cookie := c.Cookies(csrfCookieName)
	if header == "" || !hmac.Equal([]byte(header), []byte(cookie)) || !s.csrf.consume(identity, header) {
		return fiber.NewError(fiber.StatusForbidden)
	}
	return c.Next()
}

type csrfTokens struct {
	secret []byte
	now    func() time.Time
	mu     sync.Mutex
	issued map[string]string
}

func newCSRFTokens(secret []byte, now func() time.Time) *csrfTokens {
	return &csrfTokens{secret: append([]byte(nil), secret...), now: now, issued: make(map[string]string)}
}

func (c *csrfTokens) issue(identity string) string {
	expires := strconv.FormatInt(c.now().Add(5*time.Minute).Unix(), 36)
	nonce := randomToken(18)
	payload := expires + "." + nonce
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write([]byte(identity + "\x00" + payload))
	token := payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	c.mu.Lock()
	c.issued[identity] = token
	c.mu.Unlock()
	return token
}

func (c *csrfTokens) consume(identity, token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 36, 64)
	if err != nil || c.now().Unix() > expires {
		return false
	}
	payload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write([]byte(identity + "\x00" + payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if issued, exists := c.issued[identity]; !exists || !hmac.Equal([]byte(issued), []byte(token)) {
		return false
	}
	delete(c.issued, identity)
	return true
}

func randomToken(bytesCount int) string {
	value := make([]byte, bytesCount)
	if _, err := rand.Read(value); err != nil {
		panic("crypto/rand unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(value)
}
