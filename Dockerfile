# Build estático: sem CGO o binário não depende de libc, então a imagem final
# não precisa carregar toolchain nenhuma.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Camada só de dependências: alterar código não invalida o download dos módulos.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath tira o caminho da máquina de build do binário; -w -s removem tabela
# de símbolos e DWARF, cortando uns 30% do tamanho final.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-w -s" \
    -o /out/server ./cmd/server

FROM alpine:3.21

# ca-certificates é obrigatório: sem ele toda busca HTTPS falha na validação
# do certificado. wget serve ao healthcheck.
RUN apk add --no-cache ca-certificates wget tzdata \
    && adduser -D -u 10001 apiuser

COPY --from=build /out/server /usr/local/bin/server

# Roda sem privilégio: se um dia houver RCE na cadeia de parsing de HTML, o
# atacante cai como usuário sem permissão em vez de root.
USER apiuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/usr/local/bin/server"]
