package httpx

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ctxKey string

const ctxKeyRequestID ctxKey = "request_id"

// Cabeçalhos que o gateway do RapidAPI injeta. Os nomes são definidos por eles;
// não inventar variação.
const (
	HeaderProxySecret  = "X-RapidAPI-Proxy-Secret"
	HeaderUser         = "X-RapidAPI-User"
	HeaderSubscription = "X-RapidAPI-Subscription"
	HeaderRequestID    = "X-Request-Id"
)

type Middleware func(http.Handler) http.Handler

// Chain aplica os middlewares na ordem em que são lidos: Chain(h, a, b) executa
// a, depois b, depois h.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// statusRecorder guarda o status para o log de acesso. http.ResponseWriter não
// expõe o que já foi escrito.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Recover impede que um panic em um handler derrube o processo inteiro e, com
// ele, todos os outros clientes atendidos pelo mesmo binário.
func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("panic no handler",
						"erro", fmt.Sprint(rec),
						"path", r.URL.Path,
						"request_id", RequestIDFrom(r.Context()),
					)
					Error(w, http.StatusInternalServerError, CodeInternal, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RequestID propaga o id que o cliente mandou ou gera um novo, para casar log
// com o ticket de suporte que o assinante abrir.
func RequestID() Middleware {
	var counter uint64
	var mu sync.Mutex
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if id == "" {
				mu.Lock()
				counter++
				id = fmt.Sprintf("%d-%d", time.Now().UnixNano(), counter)
				mu.Unlock()
			}
			w.Header().Set(HeaderRequestID, id)
			ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Logger escreve uma linha por request. Nunca registra a URL de destino
// completa junto do usuário — o par identifica o que o assinante está lendo.
func Logger() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			slog.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duracao_ms", time.Since(start).Milliseconds(),
				"bytes", rec.bytes,
				"request_id", RequestIDFrom(r.Context()),
			)
		})
	}
}

// Timeout corta o request no limite configurado. Sem isso, um site alvo que
// aceita a conexão e nunca responde prende a goroutine indefinidamente.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RapidAPIAuth exige o segredo que só o gateway do RapidAPI conhece. Com o
// segredo vazio a checagem é pulada, o que só acontece em desenvolvimento — a
// config recusa subir em produção sem ele.
func RapidAPIAuth(secret string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				next.ServeHTTP(w, r)
				return
			}
			got := r.Header.Get(HeaderProxySecret)
			// Comparação em tempo constante: comparar com == vaza o tamanho do
			// prefixo correto e permite descobrir o segredo byte a byte.
			if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
				Error(w, http.StatusForbidden, CodeUnauthorized,
					"access allowed only through RapidAPI")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimiter limita por assinante. É proteção da máquina, não cobrança: a
// cota comercial é medida pelo RapidAPI.
type RateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	perMin   int
	burst    int
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewRateLimiter(perMinute int) *RateLimiter {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		perMin:   perMinute,
		// Rajada de 1/4 do minuto: absorve o cliente que dispara um lote de
		// URLs em paralelo sem deixá-lo monopolizar o servidor.
		burst: max(1, perMinute/4),
	}
	go rl.cleanupLoop()
	return rl
}

// cleanupLoop evita que o mapa cresça para sempre — cada assinante que aparece
// uma vez deixaria um limiter residente na memória.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		for k, v := range rl.visitors {
			if time.Since(v.lastSeen) > 15*time.Minute {
				delete(rl.visitors, k)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	v, ok := rl.visitors[key]
	if !ok {
		v = &visitor{limiter: rate.NewLimiter(rate.Limit(float64(rl.perMin)/60.0), rl.burst)}
		rl.visitors[key] = v
	}
	v.lastSeen = time.Now()
	return v.limiter.Allow()
}

func (rl *RateLimiter) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(HeaderUser)
			if key == "" {
				key = clientIP(r)
			}
			if !rl.allow(key) {
				w.Header().Set("Retry-After", "60")
				Error(w, http.StatusTooManyRequests, CodeRateLimited,
					"too many requests; please retry shortly")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}

func RequestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return id
	}
	return ""
}
