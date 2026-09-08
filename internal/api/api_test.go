package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phelipehass/microapis/internal/api"
	"github.com/phelipehass/microapis/internal/config"
	"github.com/phelipehass/microapis/internal/httpx"
	"github.com/phelipehass/microapis/internal/safefetch"
)

const segredo = "segredo-de-teste"

// paginaDeTeste sobe um site alvo em 127.0.0.1 com um artigo de verdade.
func paginaDeTeste(t *testing.T) *httptest.Server {
	t.Helper()
	paragrafo := strings.Repeat(
		"Conteúdo suficiente para o algoritmo de leitura reconhecer isto como um artigo publicado. ", 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html><html lang="pt-BR"><head><title>Artigo de Teste</title></head>
		<body><article><h1>Artigo de Teste</h1><p>` + paragrafo + `</p>
		<h2>Seção</h2><p>` + paragrafo + `</p></article></body></html>`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

// novoServidor devolve o router pronto. allowPrivate=true é o que permite ao
// teste buscar o httptest em 127.0.0.1, que a proteção barraria em produção.
func novoServidor(t *testing.T, comSegredo bool, allowPrivate bool) http.Handler {
	t.Helper()
	cfg := config.Config{
		Port:               "0",
		Env:                "test",
		RateLimitPerMinute: 1000,
		RequestTimeout:     20 * time.Second,
	}
	if comSegredo {
		cfg.RapidAPIProxySecret = segredo
	}
	f := safefetch.New(safefetch.Config{
		MaxBytes: 1 << 20, Timeout: 10 * time.Second, MaxRedirects: 5,
		UserAgent: "teste", AllowPrivateIPs: allowPrivate,
	})
	return api.NewRouter(cfg, api.NewHandler(f))
}

func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeErro(t *testing.T, rec *httptest.ResponseRecorder) httpx.ErrorResponse {
	t.Helper()
	var body httpx.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("resposta de erro não é json válido: %v (%s)", err, rec.Body.String())
	}
	return body
}

func TestRequestSemSegredoDoProxyEhRecusado(t *testing.T) {
	// Sem essa barreira, quem descobre o IP do VPS usa a API fora do medidor
	// do RapidAPI — ou seja, de graça.
	h := novoServidor(t, true, true)
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/v1/extract?url=https://exemplo.com", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("esperava 403, veio %d", rec.Code)
	}
	if got := decodeErro(t, rec).Error.Code; got != httpx.CodeUnauthorized {
		t.Errorf("esperava code %q, veio %q", httpx.CodeUnauthorized, got)
	}
}

func TestRequestComSegredoErradoEhRecusado(t *testing.T) {
	h := novoServidor(t, true, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/extract?url=https://exemplo.com", nil)
	req.Header.Set(httpx.HeaderProxySecret, "segredo-errado")

	if rec := do(t, h, req); rec.Code != http.StatusForbidden {
		t.Fatalf("esperava 403, veio %d", rec.Code)
	}
}

func TestHealthNaoExigeAutenticacao(t *testing.T) {
	// O healthcheck do Docker e o monitor do VPS não têm o segredo do RapidAPI.
	h := novoServidor(t, true, true)
	if rec := do(t, h, httptest.NewRequest(http.MethodGet, "/health", nil)); rec.Code != http.StatusOK {
		t.Fatalf("esperava 200 em /health, veio %d", rec.Code)
	}
}

func TestExtracaoComSucessoDevolveMarkdownEMetadados(t *testing.T) {
	site := paginaDeTeste(t)
	h := novoServidor(t, false, true)

	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/v1/extract?url="+site.URL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("esperava 200, veio %d: %s", rec.Code, rec.Body.String())
	}

	var resp api.ExtractResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json inválido: %v", err)
	}
	if resp.Title != "Artigo de Teste" {
		t.Errorf("título: esperava %q, veio %q", "Artigo de Teste", resp.Title)
	}
	if !strings.Contains(resp.Content, "## Seção") {
		t.Errorf("esperava markdown no conteúdo, veio:\n%s", resp.Content)
	}
	if resp.Format != "markdown" {
		t.Errorf("formato: esperava markdown, veio %q", resp.Format)
	}
	if resp.WordCount == 0 {
		t.Error("word_count não deveria ser zero")
	}
}

func TestUrlAusenteDevolve400(t *testing.T) {
	h := novoServidor(t, false, true)
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/v1/extract", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("esperava 400, veio %d", rec.Code)
	}
	if got := decodeErro(t, rec).Error.Code; got != httpx.CodeInvalidRequest {
		t.Errorf("esperava code %q, veio %q", httpx.CodeInvalidRequest, got)
	}
}

func TestFormatoInvalidoDevolve400(t *testing.T) {
	h := novoServidor(t, false, true)
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/v1/extract?url=https://exemplo.com&format=pdf", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("esperava 400, veio %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUrlParaRedeInternaDevolve403(t *testing.T) {
	// O teste que mais importa: a API não pode virar proxy para a rede interna
	// de quem a hospeda. allowPrivate=false = comportamento de produção.
	h := novoServidor(t, false, false)

	for _, alvo := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:6379/",
		"http://10.0.0.1/admin",
		"http://192.168.0.1/",
	} {
		rec := do(t, h, httptest.NewRequest(http.MethodGet, "/v1/extract?url="+alvo, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: esperava 403, veio %d (%s)", alvo, rec.Code, rec.Body.String())
			continue
		}
		if got := decodeErro(t, rec).Error.Code; got != httpx.CodeBlockedHost {
			t.Errorf("%s: esperava code %q, veio %q", alvo, httpx.CodeBlockedHost, got)
		}
	}
}

func TestEsquemaNaoHttpDevolve400(t *testing.T) {
	h := novoServidor(t, false, false)
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/v1/extract?url=file:///etc/passwd", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("esperava 400, veio %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPostComHtmlCruNaoFazRequisicaoDeRede(t *testing.T) {
	// allowPrivate=false e nenhum servidor no ar: se o handler tentasse buscar
	// algo, o teste falharia. Prova que o caminho é puramente local.
	h := novoServidor(t, false, false)
	paragrafo := strings.Repeat("Texto longo o bastante para virar artigo reconhecível pelo algoritmo. ", 10)
	body := `{"url":"https://exemplo.com/post","html":"<html><head><title>Do Corpo</title></head><body><article><h1>Do Corpo</h1><p>` +
		paragrafo + `</p></article></body></html>"}`

	req := httptest.NewRequest(http.MethodPost, "/v1/extract", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, h, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("esperava 200, veio %d: %s", rec.Code, rec.Body.String())
	}
	var resp api.ExtractResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json inválido: %v", err)
	}
	if resp.Title != "Do Corpo" {
		t.Errorf("título: esperava %q, veio %q", "Do Corpo", resp.Title)
	}
}

func TestPostSemHtmlENemUrlDevolve400(t *testing.T) {
	h := novoServidor(t, false, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/extract", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")

	if rec := do(t, h, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("esperava 400, veio %d", rec.Code)
	}
}

func TestPostComJsonInvalidoDevolve400(t *testing.T) {
	h := novoServidor(t, false, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/extract", strings.NewReader(`{"url":`))
	req.Header.Set("Content-Type", "application/json")

	if rec := do(t, h, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("esperava 400, veio %d", rec.Code)
	}
}

func TestPaginaSemArtigoDevolve422(t *testing.T) {
	// 422 e não 500: o pedido estava certo, a página é que não tem artigo.
	// O cliente precisa distinguir para saber que repetir não adianta.
	h := novoServidor(t, false, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/extract",
		strings.NewReader(`{"html":"<html><body><div></div></body></html>"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := do(t, h, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("esperava 422, veio %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeErro(t, rec).Error.Code; got != httpx.CodeNoContent {
		t.Errorf("esperava code %q, veio %q", httpx.CodeNoContent, got)
	}
}

func TestRateLimitDevolve429AposEstourarACota(t *testing.T) {
	cfg := config.Config{
		Port: "0", Env: "test",
		RateLimitPerMinute: 1, // burst mínimo: o segundo request já estoura
		RequestTimeout:     10 * time.Second,
	}
	f := safefetch.New(safefetch.Config{
		MaxBytes: 1 << 20, Timeout: 5 * time.Second, MaxRedirects: 3,
		UserAgent: "teste", AllowPrivateIPs: false,
	})
	h := api.NewRouter(cfg, api.NewHandler(f))

	var ultimo int
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/extract?url=http://10.0.0.1/", nil)
		req.Header.Set(httpx.HeaderUser, "assinante-teste")
		ultimo = do(t, h, req).Code
	}
	if ultimo != http.StatusTooManyRequests {
		t.Fatalf("esperava 429 após estourar a cota, veio %d", ultimo)
	}
}
