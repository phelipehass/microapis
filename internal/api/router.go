package api

import (
	"net/http"
	"time"

	"github.com/phelipehass/microapis/internal/config"
	"github.com/phelipehass/microapis/internal/httpx"
)

// NewRouter monta a árvore de rotas. O binário é único de propósito: quando o
// segundo e o terceiro produto entrarem, cada um ganha seu prefixo /v1/<produto>
// aqui e continua compartilhando middleware, servidor e VPS.
func NewRouter(cfg config.Config, h *Handler) http.Handler {
	rl := httpx.NewRateLimiter(cfg.RateLimitPerMinute)

	// Rotas de produto: exigem o segredo do proxy e passam pelo rate limit.
	product := http.NewServeMux()
	product.HandleFunc("GET /v1/extract", h.GetExtract)
	product.HandleFunc("POST /v1/extract", h.PostExtract)

	root := http.NewServeMux()
	root.Handle("/v1/", httpx.Chain(product,
		httpx.RapidAPIAuth(cfg.RapidAPIProxySecret),
		rl.Middleware(),
	))

	// /health fica fora da autenticação: é o que o monitor do VPS e o
	// healthcheck do Docker consultam, e eles não têm o segredo do RapidAPI.
	root.HandleFunc("GET /health", handleHealth)
	root.HandleFunc("GET /{$}", handleRoot)

	return httpx.Chain(root,
		httpx.Recover(),
		httpx.RequestID(),
		httpx.Logger(),
		httpx.Timeout(cfg.RequestTimeout),
	)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func handleRoot(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"name": "Article Extractor & Markdown API",
		"docs": "https://rapidapi.com",
		"endpoints": []string{
			"GET  /v1/extract?url=...",
			"POST /v1/extract",
			"GET  /health",
		},
	})
}
