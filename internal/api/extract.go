// Package api expõe os endpoints HTTP. Aqui só mora tradução: query string e
// JSON viram Options, erro de domínio vira status code. Regra de negócio fica
// em extract; rede fica em safefetch.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/phelipehass/microapis/internal/extract"
	"github.com/phelipehass/microapis/internal/httpx"
	"github.com/phelipehass/microapis/internal/safefetch"
)

// maxRequestBody limita o HTML enviado no POST. É separado do limite de
// download: aqui o custo é de banda de entrada e memória do nosso processo.
const maxRequestBody = 10 << 20 // 10 MiB

type Handler struct {
	fetcher *safefetch.Fetcher
}

func NewHandler(f *safefetch.Fetcher) *Handler {
	return &Handler{fetcher: f}
}

// ExtractResponse é o contrato público. Article vem embutido, então seus campos
// aparecem no nível raiz do JSON — mudar isso quebra todo cliente existente.
type ExtractResponse struct {
	URL         string `json:"url,omitempty"`
	ResolvedURL string `json:"resolved_url,omitempty"`
	extract.Article
	DurationMs int64 `json:"duration_ms"`
}

type extractRequest struct {
	URL           string `json:"url"`
	HTML          string `json:"html"`
	Format        string `json:"format"`
	IncludeImages *bool  `json:"include_images"`
	IncludeLinks  *bool  `json:"include_links"`
	FullPage      *bool  `json:"full_page"`
}

// GetExtract atende GET /v1/extract?url=... — o caminho que 95% dos assinantes
// vão usar, porque cabe num navegador e no botão de teste do RapidAPI.
func (h *Handler) GetExtract(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := r.URL.Query()

	rawURL := strings.TrimSpace(q.Get("url"))
	if rawURL == "" {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeInvalidRequest,
			"the url parameter is required")
		return
	}

	opts, err := optionsFromQuery(q)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
		return
	}

	res, err := h.fetcher.Fetch(r.Context(), rawURL)
	if err != nil {
		writeFetchError(w, r.Context(), err)
		return
	}

	art, err := extract.FromHTML(res.Body, res.FinalURL, opts)
	if err != nil {
		writeExtractError(w, err)
		return
	}

	resolved := ""
	if res.FinalURL != nil && res.FinalURL.String() != rawURL {
		resolved = res.FinalURL.String()
	}

	httpx.JSON(w, http.StatusOK, ExtractResponse{
		URL:         rawURL,
		ResolvedURL: resolved,
		Article:     *art,
		DurationMs:  time.Since(start).Milliseconds(),
	})
}

// PostExtract aceita HTML cru no corpo. Existe por dois motivos: o cliente que
// já baixou a página não paga o round-trip de novo, e páginas atrás de login
// (que nunca conseguiríamos buscar) passam a ser atendíveis.
func (h *Handler) PostExtract(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req extractRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge,
				"request body exceeds 10 MiB")
			return
		}
		httpx.Error(w, http.StatusBadRequest, httpx.CodeInvalidRequest,
			"invalid json: "+err.Error())
		return
	}

	opts := extract.DefaultOptions()
	format, err := extract.ParseFormat(req.Format)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
		return
	}
	opts.Format = format
	if req.IncludeImages != nil {
		opts.IncludeImages = *req.IncludeImages
	}
	if req.IncludeLinks != nil {
		opts.IncludeLinks = *req.IncludeLinks
	}
	if req.FullPage != nil {
		opts.FullPage = *req.FullPage
	}

	// Sem html no corpo, o POST se comporta como o GET: busca a url.
	if strings.TrimSpace(req.HTML) == "" {
		if strings.TrimSpace(req.URL) == "" {
			httpx.Error(w, http.StatusBadRequest, httpx.CodeInvalidRequest,
				"provide either html or url in the request body")
			return
		}
		res, err := h.fetcher.Fetch(r.Context(), req.URL)
		if err != nil {
			writeFetchError(w, r.Context(), err)
			return
		}
		h.respond(w, req.URL, res.FinalURL, res.Body, opts, start)
		return
	}

	// url aqui é só base para resolver href relativo; nenhuma conexão sai,
	// então não passa pela validação de rede do safefetch.
	var base *url.URL
	if s := strings.TrimSpace(req.URL); s != "" {
		parsed, err := url.Parse(s)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, httpx.CodeInvalidURL,
				"invalid url: "+err.Error())
			return
		}
		base = parsed
	}

	h.respond(w, req.URL, base, []byte(req.HTML), opts, start)
}

func (h *Handler) respond(w http.ResponseWriter, requested string, base *url.URL, body []byte, opts extract.Options, start time.Time) {
	art, err := extract.FromHTML(body, base, opts)
	if err != nil {
		writeExtractError(w, err)
		return
	}
	resolved := ""
	if base != nil && base.String() != requested {
		resolved = base.String()
	}
	httpx.JSON(w, http.StatusOK, ExtractResponse{
		URL:         requested,
		ResolvedURL: resolved,
		Article:     *art,
		DurationMs:  time.Since(start).Milliseconds(),
	})
}

func optionsFromQuery(q url.Values) (extract.Options, error) {
	opts := extract.DefaultOptions()

	format, err := extract.ParseFormat(q.Get("format"))
	if err != nil {
		return opts, err
	}
	opts.Format = format

	if opts.IncludeImages, err = boolParam(q, "include_images", opts.IncludeImages); err != nil {
		return opts, err
	}
	if opts.IncludeLinks, err = boolParam(q, "include_links", opts.IncludeLinks); err != nil {
		return opts, err
	}
	if opts.FullPage, err = boolParam(q, "full_page", opts.FullPage); err != nil {
		return opts, err
	}
	return opts, nil
}

func boolParam(q url.Values, key string, def bool) (bool, error) {
	raw := strings.TrimSpace(q.Get(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def, errors.New(key + " must be true or false")
	}
	return v, nil
}

// writeFetchError mapeia falha de rede em status. A distinção importa para o
// cliente: 403 é culpa do pedido, 502 é o site alvo, 504 é lentidão.
func writeFetchError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	case errors.Is(err, safefetch.ErrInvalidURL):
		httpx.Error(w, http.StatusBadRequest, httpx.CodeInvalidURL, err.Error())
	case errors.Is(err, safefetch.ErrBlockedHost):
		httpx.Error(w, http.StatusForbidden, httpx.CodeBlockedHost,
			"this url points to a private network address and cannot be fetched")
	case errors.Is(err, safefetch.ErrTooLarge):
		httpx.Error(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge, err.Error())
	case errors.Is(err, safefetch.ErrUnsupportedCT):
		httpx.Error(w, http.StatusUnsupportedMediaType, httpx.CodeUnsupportedCT,
			"the url did not return html (this api extracts articles from web pages)")
	case errors.Is(err, safefetch.ErrTooManyRedirect):
		httpx.Error(w, http.StatusBadGateway, httpx.CodeFetchFailed, err.Error())
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		httpx.Error(w, http.StatusGatewayTimeout, httpx.CodeTimeout,
			"the target site took too long to respond")
	default:
		httpx.Error(w, http.StatusBadGateway, httpx.CodeFetchFailed, err.Error())
	}
}

func writeExtractError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, extract.ErrNoContent):
		httpx.Error(w, http.StatusUnprocessableEntity, httpx.CodeNoContent,
			"no readable article content found on this page; try full_page=true")
	case errors.Is(err, extract.ErrInvalidHTML):
		httpx.Error(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, httpx.CodeInternal, "failed to process the page")
	}
}
