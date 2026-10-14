//go:build keycloak

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Teste contra o Keycloak real do docker-compose (realm "wallet" importado de deploy/keycloak).
// Pré-requisito: `docker compose up -d keycloak` e aguardar o realm ficar disponível.
// KEYCLOAK_URL (padrão http://localhost:8081) aponta para o Keycloak visto do host.

func kcBase() string {
	if v := os.Getenv("KEYCLOAK_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8081"
}

func clientCredentials(t *testing.T, client, secret string) (string, int) {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {client}, "client_secret": {secret}}
	res, err := http.PostForm(kcBase()+"/realms/wallet/protocol/openid-connect/token", form)
	if err != nil {
		t.Fatalf("Keycloak inacessível em %s: %v", kcBase(), err)
	}
	defer res.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	return body.AccessToken, res.StatusCode
}

func TestKeycloakTokens(t *testing.T) {
	v := NewVerifier(kcBase()+"/realms/wallet", kcBase()+"/realms/wallet/protocol/openid-connect/certs")
	ctx := context.Background()

	cases := []struct {
		client, secret, role string
	}{
		{"provider-a", "provider-a-secret", RoleProvider},
		{"provider-b", "provider-b-secret", RoleProvider},
		{"wallet-internal-service", "internal-secret", RoleInternal},
	}
	for _, c := range cases {
		tok, status := clientCredentials(t, c.client, c.secret)
		if status != 200 || tok == "" {
			t.Fatalf("%s: token não emitido (HTTP %d)", c.client, status)
		}
		p, err := v.Verify(ctx, tok)
		if err != nil {
			t.Fatalf("%s: token do Keycloak rejeitado: %v", c.client, err)
		}
		if p.ClientID != c.client || !p.HasRole(c.role) {
			t.Errorf("%s: principal inesperado %+v", c.client, p)
		}
		if c.role == RoleProvider && p.HasRole(RoleInternal) {
			t.Errorf("%s não deveria ter o papel interno", c.client)
		}
		// adulterar o payload invalida a assinatura
		parts := strings.Split(tok, ".")
		forged := parts[0] + "." + parts[1] + "A." + parts[2]
		if _, err := v.Verify(ctx, forged); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: token adulterado deveria ser rejeitado, veio %v", c.client, err)
		}
	}
	if _, status := clientCredentials(t, "provider-a", "senha-errada"); status == 200 {
		t.Error("o Keycloak não deveria emitir token com segredo errado")
	}
	// um verificador configurado para outro emissor rejeita o token
	other := NewVerifier("http://outro-emissor/realms/x", kcBase()+"/realms/wallet/protocol/openid-connect/certs")
	tok, _ := clientCredentials(t, "provider-a", "provider-a-secret")
	if _, err := other.Verify(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("emissor diferente deveria ser rejeitado, veio %v", err)
	}
}

// Token realmente emitido pelo Keycloak com validade curta (cliente provider-short-lived, 3 s):
// válido logo após a emissão e rejeitado depois de expirar. O leeway é zerado para o teste ser
// determinístico (em produção a tolerância padrão é de 30 s).
func TestKeycloakExpiredToken(t *testing.T) {
	v := NewVerifier(kcBase()+"/realms/wallet", kcBase()+"/realms/wallet/protocol/openid-connect/certs").WithLeeway(0)
	ctx := context.Background()

	tok, status := clientCredentials(t, "provider-short-lived", "short-lived-secret")
	if status != 200 || tok == "" {
		t.Fatalf("token não emitido (HTTP %d); reimporte o realm: docker compose down -v && docker compose up -d", status)
	}
	if _, err := v.Verify(ctx, tok); err != nil {
		t.Fatalf("token recém-emitido deveria ser válido: %v", err)
	}
	time.Sleep(4 * time.Second)
	if _, err := v.Verify(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("token expirado deveria ser rejeitado como não autenticado, veio %v", err)
	}
}
