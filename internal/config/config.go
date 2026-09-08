// Package config lê a configuração do ambiente. Sem arquivo de config e sem
// flags: o alvo é container, e variável de ambiente é o que o Compose entrega.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port string
	Env  string

	// RapidAPIProxySecret é o segredo que o RapidAPI injeta em todo request
	// que passa pelo gateway deles. Sem essa checagem, qualquer um que
	// descubra o IP do servidor consome a API de graça, fora do medidor.
	RapidAPIProxySecret string

	FetchTimeout  time.Duration
	FetchMaxBytes int64
	MaxRedirects  int
	UserAgent     string

	// RateLimitPerMinute protege a máquina de um único assinante em loop. A
	// cota comercial quem controla é o RapidAPI; isto aqui é defesa de infra.
	RateLimitPerMinute int

	RequestTimeout  time.Duration
	AllowPrivateIPs bool
}

func Load() (Config, error) {
	cfg := Config{
		Port:                getEnv("PORT", "8080"),
		Env:                 getEnv("APP_ENV", "development"),
		RapidAPIProxySecret: os.Getenv("RAPIDAPI_PROXY_SECRET"),
		UserAgent:           getEnv("FETCH_USER_AGENT", "Mozilla/5.0 (compatible; ReadableAPI/1.0; +https://rapidapi.com)"),
	}

	var err error
	if cfg.FetchTimeout, err = getEnvDuration("FETCH_TIMEOUT", 15*time.Second); err != nil {
		return cfg, err
	}
	if cfg.RequestTimeout, err = getEnvDuration("REQUEST_TIMEOUT", 25*time.Second); err != nil {
		return cfg, err
	}
	if cfg.FetchMaxBytes, err = getEnvInt64("FETCH_MAX_BYTES", 5<<20); err != nil {
		return cfg, err
	}
	if cfg.MaxRedirects, err = getEnvInt("FETCH_MAX_REDIRECTS", 5); err != nil {
		return cfg, err
	}
	if cfg.RateLimitPerMinute, err = getEnvInt("RATE_LIMIT_PER_MINUTE", 120); err != nil {
		return cfg, err
	}
	if cfg.AllowPrivateIPs, err = getEnvBool("ALLOW_PRIVATE_IPS", false); err != nil {
		return cfg, err
	}

	// Falha ao subir, não em produção sob carga: rodar em produção sem o
	// segredo do proxy significa servir de graça sem perceber.
	if cfg.IsProduction() && cfg.RapidAPIProxySecret == "" {
		return cfg, fmt.Errorf("RAPIDAPI_PROXY_SECRET é obrigatório quando APP_ENV=production")
	}
	if cfg.IsProduction() && cfg.AllowPrivateIPs {
		return cfg, fmt.Errorf("ALLOW_PRIVATE_IPS não pode ser true em produção: abre SSRF para a rede interna")
	}
	return cfg, nil
}

func (c Config) IsProduction() bool {
	return strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "prod")
}

func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("%s inválido: %w", key, err)
	}
	return v, nil
}

func getEnvInt64(key string, def int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def, fmt.Errorf("%s inválido: %w", key, err)
	}
	return v, nil
}

func getEnvBool(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def, fmt.Errorf("%s inválido: %w", key, err)
	}
	return v, nil
}

func getEnvDuration(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("%s inválido (use formato de duração, ex: 15s): %w", key, err)
	}
	return v, nil
}
