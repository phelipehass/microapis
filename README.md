# microapis — Article Extractor & Markdown API

Micro-API em Go que transforma qualquer página web em Markdown limpo, pronto
para LLM e pipeline de RAG. Feita para ser listada no RapidAPI.

O repositório é um monorepo de propósito: quando o segundo e o terceiro produto
entrarem, cada um ganha seu prefixo de rota no mesmo binário e continua
dividindo middleware, servidor e VPS. Três listagens, um deploy, uma conta.

## O que ela faz

Recebe uma URL e devolve o conteúdo principal do artigo, sem menu, anúncio,
banner de cookie, barra lateral ou rodapé — mais os metadados que normalmente
se raspa na mão: título, autor, data de publicação, idioma, imagem de capa,
contagem de palavras e tempo de leitura.

Diferenciais que sustentam a assinatura:

- **Tabela sobrevive.** A maioria dos conversores achata tabela em texto
  ilegível. O plugin de tabela mantém as colunas — é o que mais importa para
  quem alimenta LLM.
- **Link vira absoluto.** `href` relativo é resolvido contra a URL da página.
- **Qualquer encoding.** Página em ISO-8859-1 volta em UTF-8 correto, sem
  acento corrompido.
- **Três formatos:** `markdown`, `text` ou `html` limpo.
- **POST com HTML cru**, para página atrás de login ou que o cliente já baixou.

## Estrutura

```
cmd/server        binário único, wiring e shutdown gracioso
internal/config   configuração por variável de ambiente
internal/safefetch busca HTTP com proteção contra SSRF
internal/extract  readability + conversão (sem rede, testável isolado)
internal/api      handlers e tradução erro -> status code
internal/httpx    middleware compartilhado entre produtos
openapi.yaml      importado direto no RapidAPI para gerar a listagem
```

A regra que mantém isso saudável: `extract` não conhece rede, `safefetch` não
conhece HTTP de entrada, e `api` só traduz. É o que permite testar a conversão
inteira sem subir servidor.

## Rodando local

```bash
go run ./cmd/server
```

Sem `RAPIDAPI_PROXY_SECRET`, a autenticação de proxy fica desligada — é o modo
de desenvolvimento. Em produção o servidor se recusa a subir sem o segredo.

```bash
curl "http://localhost:8080/v1/extract?url=https://go.dev/blog/go1.22"
curl "http://localhost:8080/v1/extract?url=https://exemplo.com&format=text&include_images=false"
```

## Testes

```bash
go test ./...
```

Nenhum teste faz chamada externa real: os alvos sobem em `httptest`. O teste
mais importante é `TestUrlParaRedeInternaDevolve403` — ele prova que a API não
vira proxy para a rede interna de quem a hospeda. Não deixar regredir.

## Segurança

O ponto crítico de uma API que busca URL arbitrária é o SSRF: sem defesa, o
cliente pede `http://169.254.169.254/` e lê as credenciais da sua instância.

A proteção é em duas camadas:

1. **Antes de conectar** — `ParseAndValidate` rejeita esquema não-HTTP e host
   que já é literal de IP interno.
2. **No momento do connect** — `Dialer.Control` valida o IP *já resolvido*.
   Esta é a camada que fecha o DNS rebinding, em que o domínio resolve para um
   IP público na checagem e para `127.0.0.1` na conexão seguinte.

Somam-se: `Proxy: nil` no transport (um `HTTP_PROXY` no ambiente faria o
Control validar o IP do proxy em vez do alvo), teto de download, limite de
redirecionamento, timeout em toda etapa e container rodando sem privilégio.

## Deploy no VPS

Pré-requisitos: Docker e Docker Compose no servidor, e um domínio apontando
para o IP dele.

```bash
cp .env.example .env
# edite .env: DOMAIN e RAPIDAPI_PROXY_SECRET
docker compose up -d --build
```

O Caddy emite e renova o certificado TLS sozinho — o RapidAPI exige origem em
HTTPS com certificado válido.

A API **não** é publicada na internet: não há `ports` no serviço `api`, só
`expose`. O único caminho até ela é o Caddy. Isso impede que alguém bata direto
em `IP:8080` e escape do medidor de cobrança.

Verificação:

```bash
curl https://seudominio.com.br/health
```

## Listando no RapidAPI

1. **Pegue o segredo do proxy.** No RapidAPI: *Add New API* → depois, na aba
   *Settings* da API, a seção **RapidAPI Proxy Secret**. Copie para o `.env` do
   VPS e reinicie (`docker compose up -d`).
2. **Importe a spec.** *Add New API* → *Import from OpenAPI* → suba
   `openapi.yaml`. Antes, troque `servers.url` pelo seu domínio real.
3. **Confirme que a proteção está ativa.** Bater direto no domínio, sem o
   cabeçalho do proxy, tem que devolver 403:
   ```bash
   curl https://seudominio.com.br/v1/extract?url=https://go.dev
   ```
   Se devolver 200, o segredo não está configurado e você está servindo de graça.
4. **Configure os planos.** Referência de mercado para esta categoria:

   | Plano | Cota | Preço |
   |---|---|---|
   | BASIC | 100/mês | grátis |
   | PRO | 10.000/mês | US$ 9,99 |
   | ULTRA | 100.000/mês | US$ 29,99 |
   | MEGA | 1.000.000/mês | US$ 99,99 |

   O plano gratuito não é caridade: é o mecanismo de descoberta do RapidAPI e a
   principal fonte de conversão.

5. **Descrição e exemplos vendem.** O texto de `openapi.yaml` já está escrito
   em inglês e orientado a benefício, não a recurso. As mensagens de erro da
   API também são em inglês — comentário e nome de teste ficam em português,
   mas nada disso vaza para o assinante.

## Configuração

| Variável | Padrão | Para que serve |
|---|---|---|
| `PORT` | `8080` | Porta do servidor |
| `APP_ENV` | `development` | `production` ativa log JSON e exige o segredo |
| `RAPIDAPI_PROXY_SECRET` | — | Obrigatório em produção |
| `FETCH_TIMEOUT` | `15s` | Teto da busca da página alvo |
| `REQUEST_TIMEOUT` | `25s` | Teto do request inteiro; manter acima do anterior |
| `FETCH_MAX_BYTES` | `5242880` | Teto de download (5 MiB) |
| `FETCH_MAX_REDIRECTS` | `5` | Limite de redirecionamento |
| `RATE_LIMIT_PER_MINUTE` | `120` | Proteção de infra por assinante |
| `ALLOW_PRIVATE_IPS` | `false` | **Só teste.** A config recusa `true` em produção |

## Próximos produtos

O middleware, o servidor e o deploy já são compartilhados. Para adicionar o
segundo produto, basta uma rota nova em `internal/api/router.go` e o pacote de
domínio correspondente:

- **Verificação de e-mail em lote** — sintaxe, MX, domínio descartável e
  role-account. É a vitrine do ponto forte do Go: milhares de checagens DNS
  concorrentes em segundos.
- **Geração de imagem OG/social** — renderização pura em Go, sem Chrome
  headless, o que mantém a margem alta.
