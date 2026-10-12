# Estágio de Build
FROM golang:1.27-alpine AS builder

WORKDIR /app


# Copia dependências
COPY go.mod go.sum ./
RUN go mod download

# Copia o código fonte
COPY . .

# Compila o binário estático
RUN CGO_ENABLED=0 GOOS=linux go build -o /wallet-service ./main.go

# Estágio de Execução (Imagem final leve)
FROM alpine:3.19

WORKDIR /root/

# Copia o binário compilado do estágio anterior
COPY --from=builder /wallet-service .

EXPOSE 8080

CMD ["./wallet-service"]