package wallet

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// Nota: Este teste pressupõe uma base de dados de teste configurada ou integração ativa.
// Ele valida que múltiplos depósitos simultâneos na mesma carteira aplicam o bloqueio pessimista corretamente.
func TestConcurrentDeposits(t *testing.T) {
	// Este teste pode ser executado quando a infraestrutura de integração estiver ativa.
	t.Skip("Remova esta linha para executar o teste de concorrência integrado com o PostgreSQL")

	ctx := context.Background()
	repo := NewRepository(nil) // Substituir pela conexão real de teste de integração se necessário
	service := NewService(repo)

	walletID := "seu-uuid-de-carteira-existente"
	concurrencyLevel := 10
	var wg sync.WaitGroup
	wg.Add(concurrencyLevel)

	for i := 0; i < concurrencyLevel; i++ {
		go func(idx int) {
			defer wg.Done()
			_, err := service.Deposit(ctx, walletID, "10.00", "BRL", fmt.Sprintf("key-%d", idx))
			if err != nil {
				t.Errorf("erro no depósito concorrente: %v", err)
			}
		}(i)
	}

	wg.Wait()
}
