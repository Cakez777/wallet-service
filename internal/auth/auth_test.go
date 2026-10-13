package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "http://idp.test/realms/wallet"

type idp struct {
	t    *testing.T
	srv  *httptest.Server
	keys map[string]*rsa.PrivateKey // kid -> chave publicada
	down atomic.Bool
}

func newIdP(t *testing.T, kids ...string) *idp {
	i := &idp{t: t, keys: map[string]*rsa.PrivateKey{}}
	for _, k := range kids {
		i.addKey(k)
	}
	i.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		type jwk struct{ Kty, Kid, N, E string }
		out := struct {
			Keys []map[string]string `json:"keys"`
		}{}
		for kid, k := range i.keys {
			out.Keys = append(out.Keys, map[string]string{
				"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(i.srv.Close)
	return i
}

func (i *idp) addKey(kid string) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		i.t.Fatal(err)
	}
	i.keys[kid] = k
	return k
}

func (i *idp) verifier() *Verifier {
	v := NewVerifier(issuer, i.srv.URL)
	v.minRefresh = 0
	return v
}

func sign(t *testing.T, method jwt.SigningMethod, key any, kid string, mutate func(jwt.MapClaims)) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": issuer, "sub": "svc-1", "azp": "provider-a",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"realm_access": map[string]any{"roles": []string{RoleProvider}},
	}
	if mutate != nil {
		mutate(c)
	}
	tok := jwt.NewWithClaims(method, c)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifyValidToken(t *testing.T) {
	i := newIdP(t, "k1")
	p, err := i.verifier().Verify(context.Background(), sign(t, jwt.SigningMethodRS256, i.keys["k1"], "k1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.ProviderID() != "provider-a" || !p.HasRole(RoleProvider) || p.HasRole(RoleInternal) || p.Subject != "svc-1" {
		t.Errorf("principal inesperado: %+v", p)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	i := newIdP(t, "k1")
	v := i.verifier()
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	hs := sign(t, jwt.SigningMethodHS256, []byte("segredo"), "k1", nil)

	cases := map[string]string{
		"vazio":                     "",
		"lixo":                      "nao.e.jwt",
		"expirado":                  sign(t, jwt.SigningMethodRS256, i.keys["k1"], "k1", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"sem exp":                   sign(t, jwt.SigningMethodRS256, i.keys["k1"], "k1", func(c jwt.MapClaims) { delete(c, "exp") }),
		"emissor errado":            sign(t, jwt.SigningMethodRS256, i.keys["k1"], "k1", func(c jwt.MapClaims) { c["iss"] = "http://evil/realms/x" }),
		"assinatura de outra chave": sign(t, jwt.SigningMethodRS256, other, "k1", nil),
		"HS256":                     hs,
		"sem azp":                   sign(t, jwt.SigningMethodRS256, i.keys["k1"], "k1", func(c jwt.MapClaims) { delete(c, "azp") }),
		"kid desconhecido":          sign(t, jwt.SigningMethodRS256, i.keys["k1"], "kx", nil),
	}
	for name, tok := range cases {
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: esperado ErrUnauthenticated, veio %v", name, err)
		}
	}
}

func TestVerifyIdPUnavailable(t *testing.T) {
	i := newIdP(t, "k1")
	i.down.Store(true)
	_, err := i.verifier().Verify(context.Background(), sign(t, jwt.SigningMethodRS256, i.keys["k1"], "k1", nil))
	if !errors.Is(err, ErrIdPUnavailable) {
		t.Errorf("IdP fora deveria dar ErrIdPUnavailable, veio %v", err)
	}
}

func TestVerifyKeyRotation(t *testing.T) {
	i := newIdP(t, "k1")
	v := i.verifier()
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, i.keys["k1"], "k1", nil)); err != nil {
		t.Fatal(err)
	}
	i.addKey("k2") // o IdP publica uma chave nova
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, i.keys["k2"], "k2", nil)); err != nil {
		t.Errorf("chave nova deveria ser descoberta por refresh: %v", err)
	}
	// IdP cai depois do cache aquecido: tokens com chave conhecida continuam válidos
	i.down.Store(true)
	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, i.keys["k1"], "k1", nil)); err != nil {
		t.Errorf("chave em cache deveria validar com o IdP fora: %v", err)
	}
}
