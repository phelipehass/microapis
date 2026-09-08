// Package extract transforma uma página HTML no conteúdo principal já limpo,
// no formato pedido pelo cliente, mais os metadados do artigo.
//
// A separação em relação ao safefetch é intencional: aqui não há rede. Isso
// deixa a regra de extração testável com HTML fixo em disco e permite atender
// o endpoint POST /extract, em que o cliente manda o HTML que ele mesmo já tem.
package extract

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ErrNoContent indica que a página carregou mas não tem artigo reconhecível —
// home de portal, página de login, muro de paywall. É resposta 422, não 500:
// o pedido era válido, o conteúdo é que não existe.
var ErrNoContent = errors.New("no readable content found on the page")

// ErrInvalidHTML cobre HTML que nem o parser tolerante consegue abrir.
var ErrInvalidHTML = errors.New("invalid html")

type Format string

const (
	FormatMarkdown Format = "markdown"
	FormatText     Format = "text"
	FormatHTML     Format = "html"
)

func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "markdown", "md":
		return FormatMarkdown, nil
	case "text", "txt", "plain":
		return FormatText, nil
	case "html":
		return FormatHTML, nil
	}
	return "", fmt.Errorf("invalid format %q (use markdown, text or html)", s)
}

// Options são os botões que o cliente gira. Os defaults valem para o caso de
// uso dominante — alimentar um LLM — em que link inline vale a pena manter.
type Options struct {
	Format        Format
	IncludeImages bool
	IncludeLinks  bool
	// FullPage pula o readability e converte a página inteira. É a válvula de
	// escape para quando a heurística poda demais (docs, changelog, wiki).
	FullPage bool
}

func DefaultOptions() Options {
	return Options{Format: FormatMarkdown, IncludeImages: true, IncludeLinks: true}
}

// Article é o resultado que vai virar JSON. Campo vazio some da resposta.
type Article struct {
	Title              string     `json:"title"`
	Byline             string     `json:"byline,omitempty"`
	Excerpt            string     `json:"excerpt,omitempty"`
	SiteName           string     `json:"site_name,omitempty"`
	Language           string     `json:"language,omitempty"`
	Image              string     `json:"image,omitempty"`
	Favicon            string     `json:"favicon,omitempty"`
	PublishedAt        *time.Time `json:"published_at,omitempty"`
	ModifiedAt         *time.Time `json:"modified_at,omitempty"`
	Format             Format     `json:"format"`
	Content            string     `json:"content"`
	WordCount          int        `json:"word_count"`
	ReadingTimeMinutes int        `json:"reading_time_minutes"`
}

// FromHTML é o ponto de entrada único. pageURL pode ser nil; quando presente,
// serve de base para transformar href/src relativos em absolutos — sem isso o
// markdown sai com links quebrados, a reclamação clássica desse tipo de API.
func FromHTML(raw []byte, pageURL *url.URL, opts Options) (*Article, error) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidHTML, err)
	}

	// O readability roda sempre, mesmo em FullPage: os metadados (título,
	// autor, data, og:image) vêm dele e são metade do valor da resposta.
	parsed, rErr := readability.FromDocument(doc, pageURL)

	contentNode := parsed.Node
	if opts.FullPage {
		// Reparse: o readability trabalha sobre a árvore e não garante que o
		// documento original saia intacto. Parsear de novo é barato e é a
		// única forma óbvia de garantir que full_page devolva a página toda.
		fullDoc, err := html.Parse(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidHTML, err)
		}
		contentNode = bodyOrDoc(fullDoc)
	}

	if contentNode == nil {
		if rErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrNoContent, rErr)
		}
		return nil, ErrNoContent
	}

	applyFilters(contentNode, opts)

	content, err := render(contentNode, pageURL, opts.Format)
	if err != nil {
		return nil, err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, ErrNoContent
	}

	// A contagem sai sempre do texto puro: contar palavras em markdown
	// inflaria o número com sintaxe (##, **, url de link).
	words := countWords(renderText(contentNode))

	art := &Article{
		Title:              strings.TrimSpace(parsed.Title()),
		Byline:             strings.TrimSpace(parsed.Byline()),
		Excerpt:            strings.TrimSpace(parsed.Excerpt()),
		SiteName:           strings.TrimSpace(parsed.SiteName()),
		Language:           strings.TrimSpace(parsed.Language()),
		Image:              parsed.ImageURL(),
		Favicon:            parsed.Favicon(),
		Format:             opts.Format,
		Content:            content,
		WordCount:          words,
		ReadingTimeMinutes: readingTime(words),
	}
	if t, err := parsed.PublishedTime(); err == nil && !t.IsZero() {
		art.PublishedAt = &t
	}
	if t, err := parsed.ModifiedTime(); err == nil && !t.IsZero() {
		art.ModifiedAt = &t
	}
	return art, nil
}

func bodyOrDoc(doc *html.Node) *html.Node {
	var body *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if body != nil {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Body {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if body != nil {
		return body
	}
	return doc
}

// applyFilters remove do DOM o que o cliente pediu para não receber. Fazer isso
// na árvore, e não com regex no markdown pronto, é o que evita destruir um
// bloco de código que por acaso contenha a sintaxe de link ou de imagem.
func applyFilters(root *html.Node, opts Options) {
	if opts.IncludeImages && opts.IncludeLinks {
		return
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		// Captura o próximo irmão antes de mexer: remover c zera c.NextSibling.
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			walk(c)
			c = next
		}
		if n.Type != html.ElementNode {
			return
		}
		switch {
		case !opts.IncludeImages && (n.DataAtom == atom.Img || n.DataAtom == atom.Picture || n.DataAtom == atom.Figure):
			remove(n)
		case !opts.IncludeLinks && n.DataAtom == atom.A:
			unwrap(n) // preserva o texto âncora, descarta o href
		}
	}
	walk(root)
}

func remove(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

// unwrap troca o nó pelos seus filhos, mantendo a ordem.
func unwrap(n *html.Node) {
	parent := n.Parent
	if parent == nil {
		return
	}
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		parent.InsertBefore(c, n)
		c = next
	}
	parent.RemoveChild(n)
}

func render(node *html.Node, pageURL *url.URL, f Format) (string, error) {
	switch f {
	case FormatText:
		return renderText(node), nil
	case FormatHTML:
		var buf bytes.Buffer
		if err := html.Render(&buf, node); err != nil {
			return "", fmt.Errorf("failed to render html: %w", err)
		}
		return buf.String(), nil
	default:
		return renderMarkdown(node, pageURL)
	}
}

func newConverter() *converter.Converter {
	return converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		// Tabela é o motivo nº 1 de reclamação em conversor de markdown: sem o
		// plugin, vira um amontoado de texto sem colunas.
		table.NewTablePlugin(),
	))
}

func renderMarkdown(node *html.Node, pageURL *url.URL) (string, error) {
	opts := []converter.ConvertOptionFunc{}
	if pageURL != nil {
		opts = append(opts, converter.WithDomain(pageURL.Scheme+"://"+pageURL.Host))
	}
	out, err := newConverter().ConvertNode(node, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to convert to markdown: %w", err)
	}
	return string(out), nil
}

// renderText anda na árvore juntando texto e separando blocos com quebra de
// linha. Implementado aqui, e não delegado ao readability, para que o formato
// text saia igual com e sem full_page.
func renderText(node *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			sb.WriteString(n.Data)
			return
		case html.ElementNode:
			if n.DataAtom == atom.Script || n.DataAtom == atom.Style || n.DataAtom == atom.Noscript {
				return
			}
			if n.DataAtom == atom.Br {
				sb.WriteString("\n")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && isBlock(n.DataAtom) {
			sb.WriteString("\n\n")
		}
	}
	walk(node)
	return collapseBlankLines(sb.String())
}

var blockTags = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Ul: true, atom.Ol: true, atom.Li: true, atom.Blockquote: true, atom.Pre: true,
	atom.Table: true, atom.Tr: true, atom.Figure: true, atom.Header: true, atom.Footer: true,
}

func isBlock(a atom.Atom) bool { return blockTags[a] }

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, strings.Join(strings.Fields(ln), " "))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func countWords(text string) int { return len(strings.Fields(text)) }

// readingTime usa 200 palavras/minuto, a média adotada por Medium e Pocket.
func readingTime(words int) int {
	if words == 0 {
		return 0
	}
	return int(math.Max(1, math.Ceil(float64(words)/200.0)))
}
