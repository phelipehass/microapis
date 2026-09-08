// Package safefetch busca conteúdo HTTP de URLs fornecidas por terceiros sem
// transformar o serviço em um proxy aberto.
//
// Uma API que aceita URL arbitrária é, por definição, um SSRF esperando
// acontecer: o cliente pode pedir http://169.254.169.254/ (metadata da cloud)
// ou http://localhost:6379 e ler a rede interna pelos nossos olhos. A defesa
// aqui valida o IP *já resolvido*, no momento exato do connect, via
// Dialer.Control — o único ponto imune a DNS rebinding, onde o domínio resolve
// para um IP público na checagem e para 127.0.0.1 na conexão seguinte.
package safefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html/charset"
)

// Erros sentinela: a camada HTTP os traduz em status code apropriado.
var (
	ErrInvalidURL      = errors.New("invalid url")
	ErrBlockedHost     = errors.New("blocked host")
	ErrTooLarge        = errors.New("content exceeds maximum size")
	ErrUnsupportedCT   = errors.New("unsupported content-type")
	ErrTooManyRedirect = errors.New("too many redirects")
	ErrUpstream        = errors.New("failed to fetch url")
)

// Config controla os limites da busca. Todos existem para conter abuso:
// sem eles um único request pode prender uma goroutine baixando 4GB.
type Config struct {
	MaxBytes     int64
	Timeout      time.Duration
	MaxRedirects int
	UserAgent    string
	// AllowPrivateIPs desliga a proteção de rede. Só para teste — o servidor
	// de teste sobe em 127.0.0.1, que a proteção bloquearia corretamente.
	AllowPrivateIPs bool
}

func DefaultConfig() Config {
	return Config{
		MaxBytes:     5 << 20, // 5 MiB de HTML já cobre qualquer artigo real
		Timeout:      15 * time.Second,
		MaxRedirects: 5,
		// UA de navegador: uma fatia relevante dos sites devolve 403 para
		// agentes desconhecidos, e "extraiu o artigo" é o produto.
		UserAgent: "Mozilla/5.0 (compatible; ReadableAPI/1.0; +https://rapidapi.com)",
	}
}

// Result é a resposta bruta já normalizada para UTF-8.
type Result struct {
	Body        []byte
	FinalURL    *url.URL
	ContentType string
	StatusCode  int
}

type Fetcher struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *Fetcher {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   newDialControl(cfg.AllowPrivateIPs),
	}

	transport := &http.Transport{
		// Proxy explicitamente nulo: um HTTP_PROXY no ambiente faria toda
		// conexão sair pelo proxy, e o Control passaria a validar o IP do
		// proxy em vez do IP do alvo — furo silencioso na proteção.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= cfg.MaxRedirects {
				return ErrTooManyRedirect
			}
			// O IP de cada salto é revalidado pelo Control no connect; aqui
			// só barramos troca de esquema (ex.: https -> file/gopher).
			if !isAllowedScheme(req.URL.Scheme) {
				return ErrBlockedHost
			}
			return nil
		},
	}

	return &Fetcher{cfg: cfg, client: client}
}

func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	u, err := parseAndValidate(rawURL, f.cfg.AllowPrivateIPs)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	req.Header.Set("User-Agent", f.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")

	resp, err := f.client.Do(req)
	if err != nil {
		// O erro do Control chega embrulhado em *url.Error; desembrulhamos
		// para o handler distinguir "host bloqueado" (403) de "site fora" (502).
		if errors.Is(err, ErrBlockedHost) {
			return nil, ErrBlockedHost
		}
		if errors.Is(err, ErrTooManyRedirect) {
			return nil, ErrTooManyRedirect
		}
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: status %d", ErrUpstream, resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !isTextual(ct) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCT, ct)
	}

	// Lê MaxBytes+1 para diferenciar "exatamente no limite" de "estourou".
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.cfg.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if int64(len(body)) > f.cfg.MaxBytes {
		return nil, ErrTooLarge
	}

	// Normaliza para UTF-8 lendo o charset do header e da meta tag. Sem isso,
	// site em ISO-8859-1 (comum no Brasil) sai com acento corrompido.
	utf8Body, err := decodeToUTF8(body, ct)
	if err != nil {
		utf8Body = body // charset desconhecido: melhor entregar cru que falhar
	}

	final := resp.Request.URL
	return &Result{
		Body:        utf8Body,
		FinalURL:    final,
		ContentType: ct,
		StatusCode:  resp.StatusCode,
	}, nil
}

// ParseAndValidate rejeita o que nem vale abrir conexão: esquema não-HTTP,
// URL sem host, e host que já é literal de IP privado. É a primeira das duas
// camadas — a segunda é o Dialer.Control, que pega o que só aparece após o DNS.
func ParseAndValidate(rawURL string) (*url.URL, error) {
	return parseAndValidate(rawURL, false)
}

func parseAndValidate(rawURL string, allowPrivate bool) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: empty", ErrInvalidURL)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if !isAllowedScheme(u.Scheme) {
		return nil, fmt.Errorf("%w: unsupported scheme %q (use http or https)", ErrInvalidURL, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: missing host", ErrInvalidURL)
	}
	if ip := net.ParseIP(u.Hostname()); !allowPrivate && ip != nil && isDisallowedIP(ip) {
		return nil, fmt.Errorf("%w: %s points to an internal network", ErrBlockedHost, u.Hostname())
	}
	return u, nil
}

func isAllowedScheme(s string) bool {
	return s == "http" || s == "https"
}

func isTextual(contentType string) bool {
	if contentType == "" {
		return true // servidor omisso: deixa o parser decidir
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch ct {
	case "text/html", "application/xhtml+xml", "text/plain", "application/xml", "text/xml":
		return true
	}
	return false
}

func decodeToUTF8(body []byte, contentType string) ([]byte, error) {
	r, err := charset.NewReader(strings.NewReader(string(body)), contentType)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// newDialControl devolve o hook que roda depois do DNS e antes do connect.
func newDialControl(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		if network != "tcp4" && network != "tcp6" && network != "tcp" {
			return fmt.Errorf("%w: network %q", ErrBlockedHost, network)
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: address %q", ErrBlockedHost, address)
		}
		ip := net.ParseIP(host)
		if ip == nil || isDisallowedIP(ip) {
			return fmt.Errorf("%w: %s is an internal or reserved address", ErrBlockedHost, host)
		}
		return nil
	}
}
