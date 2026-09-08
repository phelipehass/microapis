package extract

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// artigoHTML monta uma página com a estrutura que o readability espera. O corpo
// precisa passar do limiar de caracteres da heurística, senão a página é
// classificada como "sem artigo" — por isso o parágrafo é repetido.
func artigoHTML(corpo string) []byte {
	paragrafo := strings.Repeat(
		"Este é um parágrafo de teste com conteúdo suficiente para o algoritmo reconhecer um artigo de verdade. ", 8)
	return []byte(`<!DOCTYPE html>
<html lang="pt-BR">
<head>
  <title>Título do Artigo</title>
  <meta property="og:site_name" content="Blog de Teste">
  <meta property="og:image" content="https://exemplo.com/capa.jpg">
  <meta name="author" content="Fulano de Tal">
</head>
<body>
  <nav><a href="/home">Menu que deve sumir</a></nav>
  <article>
    <h1>Título do Artigo</h1>
    <p>` + paragrafo + `</p>
    ` + corpo + `
    <p>` + paragrafo + `</p>
  </article>
  <footer>Rodapé que deve sumir</footer>
</body>
</html>`)
}

func urlBase() *url.URL {
	u, _ := url.Parse("https://exemplo.com/post/1")
	return u
}

func TestExtraiTituloEMetadadosDoArtigo(t *testing.T) {
	art, err := FromHTML(artigoHTML("<p>miolo</p>"), urlBase(), DefaultOptions())
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if art.Title != "Título do Artigo" {
		t.Errorf("título: esperava %q, veio %q", "Título do Artigo", art.Title)
	}
	if art.SiteName != "Blog de Teste" {
		t.Errorf("site_name: esperava %q, veio %q", "Blog de Teste", art.SiteName)
	}
	if art.WordCount == 0 {
		t.Error("word_count não deveria ser zero")
	}
	if art.ReadingTimeMinutes < 1 {
		t.Errorf("reading_time deveria ser ao menos 1, veio %d", art.ReadingTimeMinutes)
	}
}

func TestConverteCabecalhoEListaParaMarkdown(t *testing.T) {
	art, err := FromHTML(artigoHTML(`<h2>Subtítulo</h2><ul><li>um</li><li>dois</li></ul>`), urlBase(), DefaultOptions())
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if !strings.Contains(art.Content, "## Subtítulo") {
		t.Errorf("esperava cabeçalho markdown, veio:\n%s", art.Content)
	}
	if !strings.Contains(art.Content, "- um") {
		t.Errorf("esperava item de lista markdown, veio:\n%s", art.Content)
	}
}

func TestConverteTabelaParaMarkdown(t *testing.T) {
	// Tabela é o formato que mais interessa a quem alimenta LLM e o que a
	// maioria dos conversores concorrentes destrói.
	corpo := `<table><thead><tr><th>Produto</th><th>Preço</th></tr></thead>
	<tbody><tr><td>Caderno</td><td>80</td></tr></tbody></table>`
	art, err := FromHTML(artigoHTML(corpo), urlBase(), DefaultOptions())
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if !strings.Contains(art.Content, "| Produto") || !strings.Contains(art.Content, "| Caderno") {
		t.Errorf("esperava tabela em markdown, veio:\n%s", art.Content)
	}
}

func TestRemoveNavegacaoERodape(t *testing.T) {
	art, err := FromHTML(artigoHTML("<p>miolo</p>"), urlBase(), DefaultOptions())
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if strings.Contains(art.Content, "Menu que deve sumir") {
		t.Error("navegação vazou para o conteúdo extraído")
	}
	if strings.Contains(art.Content, "Rodapé que deve sumir") {
		t.Error("rodapé vazou para o conteúdo extraído")
	}
}

func TestResolveLinkRelativoParaAbsoluto(t *testing.T) {
	// Sem a url base, o cliente recebe markdown com link quebrado — é a
	// reclamação mais comum nesse tipo de API.
	art, err := FromHTML(artigoHTML(`<p><a href="/outro-post">leia mais</a></p>`), urlBase(), DefaultOptions())
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if !strings.Contains(art.Content, "https://exemplo.com/outro-post") {
		t.Errorf("esperava link absoluto, veio:\n%s", art.Content)
	}
}

func TestIncludeImagesFalsoRemoveImagem(t *testing.T) {
	opts := DefaultOptions()
	opts.IncludeImages = false
	art, err := FromHTML(artigoHTML(`<p><img src="https://exemplo.com/foto.jpg" alt="foto"></p>`), urlBase(), opts)
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if strings.Contains(art.Content, "foto.jpg") {
		t.Errorf("imagem deveria ter sido removida, veio:\n%s", art.Content)
	}
}

func TestIncludeLinksFalsoMantemTextoEDescartaHref(t *testing.T) {
	opts := DefaultOptions()
	opts.IncludeLinks = false
	art, err := FromHTML(artigoHTML(`<p><a href="https://exemplo.com/x">texto ancora</a></p>`), urlBase(), opts)
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if strings.Contains(art.Content, "exemplo.com/x") {
		t.Errorf("href deveria ter sumido, veio:\n%s", art.Content)
	}
	if !strings.Contains(art.Content, "texto ancora") {
		t.Errorf("o texto da âncora deveria ter sido preservado, veio:\n%s", art.Content)
	}
}

func TestFullPageMantemOQueOReadabilityPodaria(t *testing.T) {
	opts := DefaultOptions()
	opts.FullPage = true
	art, err := FromHTML(artigoHTML("<p>miolo</p>"), urlBase(), opts)
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if !strings.Contains(art.Content, "Rodapé que deve sumir") {
		t.Errorf("full_page deveria trazer a página inteira, veio:\n%s", art.Content)
	}
	// Mesmo em full_page os metadados continuam vindo do readability.
	if art.Title != "Título do Artigo" {
		t.Errorf("full_page perdeu o título: %q", art.Title)
	}
}

func TestFormatoTextNaoTrazSintaxeMarkdown(t *testing.T) {
	opts := DefaultOptions()
	opts.Format = FormatText
	art, err := FromHTML(artigoHTML("<h2>Subtítulo</h2>"), urlBase(), opts)
	if err != nil {
		t.Fatalf("extração falhou: %v", err)
	}
	if strings.Contains(art.Content, "##") {
		t.Errorf("formato text não deveria conter sintaxe markdown, veio:\n%s", art.Content)
	}
	if !strings.Contains(art.Content, "Subtítulo") {
		t.Error("o texto do subtítulo deveria estar presente")
	}
}

func TestPaginaSemArtigoDevolveErroDedicado(t *testing.T) {
	// Página de login ou home de portal: pedido válido, conteúdo inexistente.
	// Precisa ser distinguível de erro interno para virar 422 e não 500.
	_, err := FromHTML([]byte("<html><body><div></div></body></html>"), urlBase(), DefaultOptions())
	if !errors.Is(err, ErrNoContent) {
		t.Fatalf("esperava ErrNoContent, veio %v", err)
	}
}

func TestParseFormatAceitaApelidosERejeitaDesconhecido(t *testing.T) {
	casos := map[string]Format{
		"":         FormatMarkdown,
		"markdown": FormatMarkdown,
		"md":       FormatMarkdown,
		"MD":       FormatMarkdown,
		"text":     FormatText,
		"txt":      FormatText,
		"html":     FormatHTML,
	}
	for entrada, esperado := range casos {
		got, err := ParseFormat(entrada)
		if err != nil {
			t.Errorf("ParseFormat(%q) devolveu erro: %v", entrada, err)
			continue
		}
		if got != esperado {
			t.Errorf("ParseFormat(%q): esperava %q, veio %q", entrada, esperado, got)
		}
	}
	if _, err := ParseFormat("pdf"); err == nil {
		t.Error("formato desconhecido deveria devolver erro")
	}
}

func TestSemUrlBaseAExtracaoAindaFunciona(t *testing.T) {
	// É o caminho do POST com html cru e sem url: não pode explodir.
	art, err := FromHTML(artigoHTML("<p>miolo</p>"), nil, DefaultOptions())
	if err != nil {
		t.Fatalf("extração sem url base falhou: %v", err)
	}
	if art.Content == "" {
		t.Error("conteúdo não deveria ser vazio")
	}
}
