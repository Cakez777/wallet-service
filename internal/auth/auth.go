// Package auth valida tokens JWT emitidos por um IdP OAuth 2.0/OIDC (Keycloak).
//
// A validação é local: assinatura RS256 contra as chaves públicas do IdP (JWKS, com cache e
// refresh quando aparece um kid novo), emissor (iss) e expiração (exp). Não há emissão de
// tokens nem cadastro de senhas aqui.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Papéis (realm roles do Keycloak) que o serviço reconhece.
const (
	RoleProvider = "wallet-provider" // provedor de jogos: envia e consulta as próprias transações
	RoleInternal = "wallet-internal" // serviço interno: operações de carteira
)

var (
	// ErrUnauthenticated: token ausente, malformado, expirado, de outro emissor ou mal assinado.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrIdPUnavailable: não foi possível obter as chaves do IdP (falha transitória).
	ErrIdPUnavailable = errors.New("identity provider unavailable")
)

// Principal é a identidade autenticada. Para client_credentials, ClientID (claim azp)
// identifica o chamador e, para provedores, é o providerId autorizado.
type Principal struct {
	Subject  string
	ClientID string
	Roles    []string
}

// HasRole informa se o principal possui o papel.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// ProviderID é o provedor autorizado para este principal (o client_id do IdP).
func (p Principal) ProviderID() string { return p.ClientID }

type claims struct {
	jwt.RegisteredClaims
	Azp         string `json:"azp"`
	ClientID    string `json:"client_id"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Verifier valida tokens contra o JWKS de um emissor.
type Verifier struct {
	issuer     string
	jwksURL    string
	client     *http.Client
	minRefresh time.Duration

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	lastFetch time.Time
}

// NewVerifier cria o verificador. issuer deve ser exatamente o valor do claim iss.
func NewVerifier(issuer, jwksURL string) *Verifier {
	return &Verifier{
		issuer: issuer, jwksURL: jwksURL,
		client:     &http.Client{Timeout: 5 * time.Second},
		minRefresh: 10 * time.Second,
		keys:       map[string]*rsa.PublicKey{},
	}
}

// Verify valida o token e devolve a identidade.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	var c claims
	tok, err := jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		if errors.Is(err, ErrIdPUnavailable) {
			return Principal{}, err
		}
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if !tok.Valid {
		return Principal{}, ErrUnauthenticated
	}
	client := c.Azp
	if client == "" {
		client = c.ClientID
	}
	if client == "" {
		return Principal{}, fmt.Errorf("%w: token without azp/client_id", ErrUnauthenticated)
	}
	return Principal{Subject: c.Subject, ClientID: client, Roles: c.RealmAccess.Roles}, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	empty := len(v.keys) == 0
	last := v.lastFetch
	v.mu.RUnlock()
	if ok {
		return k, nil
	}
	// kid desconhecido: atualiza o JWKS, no máximo uma vez por minRefresh (a menos que o cache esteja vazio).
	if !empty && time.Since(last) < v.minRefresh {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

type jwks struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (v *Verifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIdPUnavailable, err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIdPUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: jwks returned %d", ErrIdPUnavailable, resp.StatusCode)
	}
	var set jwks
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&set); err != nil {
		return fmt.Errorf("%w: invalid jwks: %v", ErrIdPUnavailable, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return fmt.Errorf("%w: jwks has no usable RSA keys", ErrIdPUnavailable)
	}
	v.mu.Lock()
	v.keys, v.lastFetch = keys, time.Now()
	v.mu.Unlock()
	return nil
}
