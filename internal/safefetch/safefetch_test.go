package safefetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUrlComEsquemaNaoHttpEhRejeitada(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://exemplo.com",
		"ftp://exemplo.com/arquivo",
		"javascript:alert(1)",
	} {
		if _, err := ParseAndValidate(raw); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("%s deveria ser ErrInvalidURL, veio %v", raw, err)
		}
	}
}

func TestUrlComIpInternoLiteralEhBloqueada(t *testing.T) {
	// A metadata da cloud é o alvo clássico de SSRF: quem consegue lê-la
	// consegue as credenciais da instância.
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://localhost.localdomain/", // não resolve aqui, mas o esquema passa
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/admin",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://[::1]/",
		"http://100.64.0.1/",
		"http://0.0.0.0/",
	} {
		u, err := ParseAndValidate(raw)
		if err == nil && net.ParseIP(u.Hostname()) != nil {
			t.Errorf("%s passou pela validação mas é endereço interno", raw)
		}
	}
}

func TestDialControlBloqueiaEnderecoInterno(t *testing.T) {
	control := newDialControl(false)

	bloqueados := []string{
		"127.0.0.1:80",
		"169.254.169.254:80", // metadata AWS/GCP/Azure
		"10.1.2.3:443",
		"192.168.0.10:8080",
		"172.20.5.5:80",
		"[::1]:80",
		"[fe80::1]:80",
		"[fc00::1]:80",
		"100.64.1.1:80",
		"0.0.0.0:80",
		"224.0.0.1:80", // multicast
	}
	for _, addr := range bloqueados {
		if err := control("tcp4", addr, nil); err == nil {
			t.Errorf("%s deveria ter sido bloqueado no dial", addr)
		} else if !errors.Is(err, ErrBlockedHost) {
			t.Errorf("%s: esperava ErrBlockedHost, veio %v", addr, err)
		}
	}

	permitidos := []string{"8.8.8.8:443", "1.1.1.1:80", "[2606:4700::1111]:443"}
	for _, addr := range permitidos {
		if err := control("tcp4", addr, nil); err != nil {
			t.Errorf("%s é público e deveria passar, veio %v", addr, err)
		}
	}
}

func TestDialControlBloqueiaIpv4MapeadoEmIpv6(t *testing.T) {
	// ::ffff:127.0.0.1 é loopback disfarçado de IPv6. Sem a normalização
	// To4() no guard, ele escaparia de todas as checagens v4.
	if err := newDialControl(false)("tcp6", "[::ffff:127.0.0.1]:80", nil); err == nil {
		t.Fatal("loopback mapeado em ipv6 deveria ser bloqueado")
	}
}

func TestFetchNormalizaCharsetParaUtf8(t *testing.T) {
	// Latin-1 ainda é comum em site brasileiro; sem conversão o acento sai
	// corrompido e o cliente recebe markdown ilegível.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
		// "informação" em ISO-8859-1
		w.Write([]byte("<html><body><p>informa\xe7\xe3o</p></body></html>")) //nolint:errcheck
	}))
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch falhou: %v", err)
	}
	if !strings.Contains(string(res.Body), "informação") {
		t.Errorf("esperava texto convertido para utf-8, veio %q", res.Body)
	}
}

func TestFetchRecusaConteudoAcimaDoLimite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(strings.Repeat("a", 2000))) //nolint:errcheck
	}))
	defer srv.Close()

	f := New(Config{
		MaxBytes: 1000, Timeout: 5 * time.Second, MaxRedirects: 3,
		UserAgent: "teste", AllowPrivateIPs: true,
	})
	if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("esperava ErrTooLarge, veio %v", err)
	}
}

func TestFetchRecusaContentTypeNaoTextual(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.4")) //nolint:errcheck
	}))
	defer srv.Close()

	if _, err := newTestFetcher().Fetch(context.Background(), srv.URL); !errors.Is(err, ErrUnsupportedCT) {
		t.Fatalf("esperava ErrUnsupportedCT, veio %v", err)
	}
}

func TestFetchPropagaErroDeStatusDoSiteAlvo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := newTestFetcher().Fetch(context.Background(), srv.URL); !errors.Is(err, ErrUpstream) {
		t.Fatalf("esperava ErrUpstream, veio %v", err)
	}
}

func TestFetchLimitaRedirecionamentos(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
	}))
	defer srv.Close()

	f := New(Config{
		MaxBytes: 1 << 20, Timeout: 5 * time.Second, MaxRedirects: 3,
		UserAgent: "teste", AllowPrivateIPs: true,
	})
	if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrTooManyRedirect) {
		t.Fatalf("esperava ErrTooManyRedirect, veio %v", err)
	}
}

func TestFetchRegistraUrlFinalAposRedirecionamento(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/destino", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><p>ok</p></body></html>")) //nolint:errcheck
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/destino", http.StatusMovedPermanently)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := newTestFetcher().Fetch(context.Background(), srv.URL+"/origem")
	if err != nil {
		t.Fatalf("fetch falhou: %v", err)
	}
	if !strings.HasSuffix(res.FinalURL.Path, "/destino") {
		t.Errorf("esperava url final /destino, veio %s", res.FinalURL)
	}
}

func newTestFetcher() *Fetcher {
	// AllowPrivateIPs só existe para este cenário: o httptest sobe em
	// 127.0.0.1, que a proteção corretamente barraria em produção.
	return New(Config{
		MaxBytes: 1 << 20, Timeout: 5 * time.Second, MaxRedirects: 5,
		UserAgent: "teste", AllowPrivateIPs: true,
	})
}
