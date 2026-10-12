package wallet

import (
	"context"
	"testing"
)

func TestCreateWallet_InvalidPlayerID(t *testing.T) {
	service := NewService(&Repository{})

	_, err := service.CreateWallet(context.Background(), "invalid-uuid", "BRL")
	if err == nil {
		t.Fatal("esperava erro devido ao UUID inválido, mas obteve nil")
	}
}

func TestCreateWallet_InvalidCurrency(t *testing.T) {
	service := NewService(&Repository{})

	validUUID := "123e4567-e89b-12d3-a456-426614174000"
	_, err := service.CreateWallet(context.Background(), validUUID, "INVALID")
	if err == nil {
		t.Fatal("esperava erro devido à moeda inválida, mas obteve nil")
	}
}
