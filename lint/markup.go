package lint

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/errata-ai/vale/core"
	"github.com/gobwas/glob"
	"github.com/jdkato/regexp"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	grh "github.com/yuin/goldmark/renderer/html"
	"golang.org/x/net/html"
)

// Error messages
var parseFail = "error: [%s] parse failure; is your markup valid?"

// reStructuredText configuration.
//
// reCodeBlock is used to convert Sphinx-style code directives to the regular
// `::` for rst2html.
var reCodeBlock = regexp.MustCompile(`.. (?:raw|code(?:-block)?):: (\w+)`)

// HACK: We replace custom Sphinx directives with `.. code::`.
//
// This isn't ideal, but it appears to be necessary.
//
// See https://github.com/errata-ai/vale/issues/119.
var reSphinx = regexp.MustCompile(`.. glossary::`)
var rstArgs = []string{
	"--quiet",
	"--halt=5",
	"--report=5",
	"--link-stylesheet",
	"--no-file-insertion",
	"--no-toc-backlinks",
	"--no-footnote-backlinks",
	"--no-section-numbering",
}

// AsciiDoc configuration.
var adocArgs = []string{
	"-s",
	"--quiet",
	"--safe-mode",
	"secure",
	"-",
}

// XML configuration.
var xsltArgs = []string{
	"--stringparam",
	"use.extensions",
	"0",
	"--stringparam",
	"generate.toc",
	"nop",
}

// Markdown configuration.
var goldMd = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
	),
	goldmark.WithRendererOptions(
		grh.WithUnsafe(),
	),
)
var reFrontMatter = regexp.MustCompile(
	`^(?s)(?:---|\+\+\+)\n(.+?)\n(?:---|\+\+\+)`)

// Convert extended info strings -- e.g., ```callout{'title': 'NOTE'} -- that
// might confuse Blackfriday into normal "```".
var reExInfo = regexp.MustCompile("```" + `.+`)

// HTML configuration.
var heading = regexp.MustCompile(`^h\d$`)

// skipTags are tags that we don't want to lint.
var skipTags = []string{"script", "style", "pre", "figure"}

// skipClasses are classes that we don't want to lint:
// 	- `problematic` is added by rst2html to processing errors which, in our
// 	  case, could be things like file-insertion URLs.
// 	- `pre` is added by rst2html to code spans.
var skipClasses = []string{"problematic", "pre"}
var inlineTags = []string{
	"b", "big", "i", "small", "abbr", "acronym", "cite", "dfn", "em", "kbd",
	"strong", "a", "br", "img", "span", "sub", "sup", "code", "tt", "del"}
var tagToScope = map[string]string{
	"th":         "text.table.header",
	"td":         "text.table.cell",
	"li":         "text.list",
	"blockquote": "text.blockquote",

	// NOTE: These shouldn't inherit from `text`
	// (or else they'll be linted twice.)
	"strong": "strong",
	"b":      "strong",
	"a":      "link",
	"em":     "emphasis",
	"i":      "emphasis",
	"code":   "code",
}

func (l Linter) lintHTMLTokens(f *core.File, ctx string, fsrc []byte, offset int) {
	var txt, attr, raw, tag string
	var tokt html.TokenType
	var tok html.Token
	var inBlock, inline, skip, skipClass bool

	lines := len(f.Lines) + offset
	buf := bytes.NewBufferString("")
	act := bytes.NewBufferString("")

	// The user has specified a custom list of tags/classes to ignore.
	if len(l.Manager.Config.SkippedScopes) > 0 {
		skipTags = l.Manager.Config.SkippedScopes
	}
	if len(l.Manager.Config.IgnoredClasses) > 0 {
		skipClasses = append(skipClasses, l.Manager.Config.IgnoredClasses...)
	}

	// queue holds each segment of text we encounter in a block, which we then
	// use to sequentially update our context.
	queue := []string{}

	// tagHistory holds the HTML tags we encounter in a given block -- e.g.,
	// if we see <ul>, <li>, <p>, we'd get tagHistory = [ul li p]. It's reset
	// on every non-inline end tag.
	tagHistory := []string{}

	tokens := html.NewTokenizer(bytes.NewReader(fsrc))

	skipped := []string{"tt", "code"}
	if len(l.Manager.Config.IgnoredScopes) > 0 {
		skipped = l.Manager.Config.IgnoredScopes
	}

	for {
		tokt = tokens.Next()
		tok = tokens.Token()
		txt = html.UnescapeString(strings.TrimSpace(tok.Data))

		skipClass = checkClasses(attr, skipClasses)
		if tokt == html.ErrorToken {
			break
		} else if tokt == html.StartTagToken && core.StringInSlice(txt, skipTags) {
			inBlock = true
		} else if inBlock && core.StringInSlice(txt, skipTags) {
			inBlock = false
		} else if tokt == html.StartTagToken {
			inline = core.StringInSlice(txt, inlineTags)
			skip = core.StringInSlice(txt, skipped)
			tagHistory = append(tagHistory, txt)
			tag = txt
		} else if tokt == html.EndTagToken && core.StringInSlice(txt, inlineTags) {
			tag = ""
		} else if tokt == html.CommentToken {
			f.UpdateComments(txt)
		} else if tokt == html.TextToken {
			skip = skip || shouldBeSkipped(tagHistory, f.NormedExt)
			if scope, match := tagToScope[tag]; match && core.StringInSlice(tag, inlineTags) {
				// NOTE: We need to create a "temporary" context because this
				// text is actually linted twice: once as a 'link' and once as
				// part of the overall paragraph. See issue #105 for more info.
				tempCtx := updateContext(ctx, queue)
				l.lintText(f, core.NewBlock(tempCtx, txt, txt, scope), lines, 0)
				tag = ""
			}
			queue = append(queue, txt)
			if !inBlock && txt != "" {
				txt, raw, skip = clean(txt, f.NormedExt, skip, skipClass, inline)
				buf.WriteString(txt)
				act.WriteString(raw)
			}
		}

		if tokt == html.EndTagToken && !core.StringInSlice(txt, inlineTags) {
			content := buf.String()
			actual := act.String()
			if strings.TrimSpace(content) != "" {
				l.lintScope(f, ctx, content, actual, tagHistory, lines)
			}

			ctx = updateContext(ctx, queue)
			queue = []string{}
			tagHistory = []string{}

			buf.Reset()
			act.Reset()
		}

		attr = getAttribute(tok, "class")
		ctx = clearElements(ctx, tok)

		if tok.Data == "img" {
			for _, a := range tok.Attr {
				if a.Key == "alt" {
					block := core.NewBlock(ctx, a.Val, a.Val, "text.attr."+a.Key)
					l.lintText(f, block, lines, 0)
				}
			}
		}
	}

	summary := core.NewBlock(f.Content, f.Summary.String(), "", "summary."+f.RealExt)
	l.lintText(f, summary, lines, 0)

	// Run all rules with `scope: raw`
	l.lintText(f, core.NewBlock("", f.Content, "", "raw."+f.RealExt), lines, 0)
}

func (l Linter) lintScope(f *core.File, ctx, txt, raw string, tags []string, lines int) {
	for _, tag := range tags {
		scope, match := tagToScope[tag]
		if (match && !core.StringInSlice(tag, inlineTags)) || heading.MatchString(tag) {
			if match {
				scope = scope + f.RealExt
			} else {
				scope = "text.heading." + tag + f.RealExt
			}
			txt = strings.TrimLeft(txt, " ")
			l.lintText(f, core.NewBlock(ctx, txt, raw, scope), lines, 0)
			return
		}
	}

	// NOTE: We don't include headings, list items, or table cells (which are
	// processed above) in our Summary content.
	f.Summary.WriteString(txt + " ")
	l.lintProse(f, ctx, txt, raw, lines, 0)
}

func checkClasses(attr string, ignore []string) bool {
	for _, class := range strings.Split(attr, " ") {
		if core.StringInSlice(class, ignore) {
			return true
		}
	}
	return false
}

// HACK: We need to look for inserted `spans` within `tt` tags.
//
// See https://github.com/errata-ai/vale/issues/140.
func shouldBeSkipped(tagHistory []string, ext string) bool {
	if ext == ".rst" {
		n := len(tagHistory)
		for i := n - 1; i >= 0; i-- {
			if tagHistory[i] == "span" {
				continue
			}
			return tagHistory[i] == "tt" && i+1 != n
		}
	}
	return false
}

func codify(ext, text string) string {
	if ext == ".md" || ext == ".adoc" {
		return "`" + text + "`"
	} else if ext == ".rst" {
		return "``" + text + "``"
	}
	return text
}

func updateContext(ctx string, queue []string) string {
	for _, s := range queue {
		ctx = updateCtx(ctx, s, html.TextToken)
	}
	return ctx
}

func clean(txt, ext string, skip, skipClass, inline bool) (string, string, bool) {
	punct := []string{".", "?", "!", ",", ":", ";"}
	first, _ := utf8.DecodeRuneInString(txt)
	starter := core.StringInSlice(string(first), punct) && !skip
	raw := txt
	if skip || skipClass {
		raw = codify(ext, txt)
		txt, _ = core.Substitute(txt, txt, '*')
		txt = codify(ext, txt)
		skip = false
	}
	if inline && !starter {
		txt = " " + txt
		raw = " " + raw
	}
	return txt, raw, skip
}

func getAttribute(tok html.Token, key string) string {
	for _, attr := range tok.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func updateCtx(ctx string, txt string, tokt html.TokenType) string {
	var found bool
	if (tokt == html.TextToken || tokt == html.CommentToken) && txt != "" {
		for _, s := range strings.Split(txt, "\n") {
			ctx, found = core.Substitute(ctx, s, '@')
			if !found {
				for _, w := range strings.Fields(s) {
					ctx, _ = core.Substitute(ctx, w, '@')
				}
			}
		}
	}
	return ctx
}

func clearElements(ctx string, tok html.Token) string {
	if tok.Data == "img" || tok.Data == "a" || tok.Data == "p" || tok.Data == "script" {
		for _, a := range tok.Attr {
			if a.Key == "href" || a.Key == "id" || a.Key == "src" {
				ctx = updateCtx(ctx, a.Val, html.TextToken)
			}
		}
	}
	return ctx
}

func (l Linter) lintHTML(f *core.File) {
	l.lintHTMLTokens(f, f.Content, []byte(f.Content), 0)
}

func (l Linter) prep(content, block, inline, ext string) (string, error) {
	s := reFrontMatter.ReplaceAllString(content, block)
	s = reExInfo.ReplaceAllString(s, "```")

	for syntax, regexes := range l.Manager.Config.TokenIgnores {
		sec, err := glob.Compile(syntax)
		if err != nil {
			return s, err
		} else if sec.Match(ext) {
			for _, r := range regexes {
				pat, err := regexp.Compile(r)
				if err == nil {
					s = pat.ReplaceAllString(s, inline)
				}
			}
		}
	}

	for syntax, regexes := range l.Manager.Config.BlockIgnores {
		sec, err := glob.Compile(syntax)
		if err != nil {
			return s, err
		} else if sec.Match(ext) {
			for _, r := range regexes {
				pat, err := regexp.Compile(r)
				if err == nil {
					if ext == ".rst" {
						// HACK: We need to add padding for the literal block.
						for _, c := range pat.FindAllStringSubmatch(s, -1) {
							new := fmt.Sprintf(block, core.Indent(c[0], "    "))
							s = strings.Replace(s, c[0], new, 1)
						}
					} else {
						s = pat.ReplaceAllString(s, block)
					}
				}
			}
		}
	}

	return s, nil
}

func (l Linter) lintMarkdown(f *core.File) error {
	var buf bytes.Buffer

	s, err := l.prep(f.Content, "\n```\n$1\n```\n", "`$1`", ".md")
	if err != nil {
		return core.NewE100(f.Path, err)
	}

	if err := goldMd.Convert([]byte(s), &buf); err != nil {
		return core.NewE100(f.Path, err)
	}

	// NOTE: This is required to avoid finding matches info strings. For
	// example, if we're looking for 'json' we many incorrectly report the
	// location as being in an infostring like '```json'.
	//
	// See https://github.com/errata-ai/vale/issues/248.
	body := reExInfo.ReplaceAllStringFunc(f.Content, func(m string) string {
		parts := strings.Split(m, "`")
		return "```" + strings.Repeat("*", len(parts[len(parts)-1]))
	})

	l.lintHTMLTokens(f, body, buf.Bytes(), 0)
	return nil
}

func (l Linter) lintSphinx(f *core.File) error {
	file := filepath.Base(f.Path)

	built := strings.Replace(file, filepath.Ext(file), ".html", 1)
	built = filepath.Join(l.Manager.Config.SphinxBuild, "html", built)

	html, err := ioutil.ReadFile(built)
	if err != nil {
		return core.NewE100(f.Path, err)
	}

	l.lintHTMLTokens(f, f.Content, html, 0)
	return nil
}

func (l Linter) lintRST(file *core.File) error {
	rst2html := core.Which([]string{"rst2html", "rst2html.py"})
	python := core.Which([]string{
		"python", "py", "python.exe", "python3", "python3.exe", "py3"})

	if rst2html == "" || python == "" {
		return core.NewE100("lintRST", errors.New("rst2html not found"))
	} else if l.Manager.Config.SphinxBuild != "" {
		return l.lintSphinx(file)
	}

	var out bytes.Buffer
	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		// rst2html is executable by default on Windows.
		cmd = exec.Command(python, append([]string{rst2html}, rstArgs...)...)
	} else {
		cmd = exec.Command(rst2html, rstArgs...)
	}

	s, err := l.prep(file.Content, "\n::\n\n%s\n", "``$1``", ".rst")
	if err != nil {
		return core.NewE100(file.Path, err)
	}
	s = reSphinx.ReplaceAllString(s, ".. code::")

	cmd.Stdin = strings.NewReader(reCodeBlock.ReplaceAllString(s, "::"))
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return core.NewE201FromPosition(err.Error(), file.Path, 1)
	}

	html := bytes.Replace(out.Bytes(), []byte("\r"), []byte(""), -1)
	bodyStart := bytes.Index(html, []byte("<body>\n"))
	if bodyStart < 0 {
		bodyStart = -7
	}
	bodyEnd := bytes.Index(html, []byte("\n</body>"))
	if bodyEnd < 0 || bodyEnd >= len(html) {
		bodyEnd = len(html) - 1
		if bodyEnd < 0 {
			bodyEnd = 0
		}
	}
	l.lintHTMLTokens(file, file.Content, html[bodyStart+7:bodyEnd], 0)

	return nil
}

func (l Linter) lintADoc(file *core.File) error {
	var out bytes.Buffer

	asciidoctor := core.Which([]string{"asciidoctor"})
	if asciidoctor == "" {
		return core.NewE100("lintAdoc", errors.New("asciidoctor not found"))
	}

	cmd := exec.Command(asciidoctor, adocArgs...)
	s, err := l.prep(file.Content, "\n----\n$1\n----\n", "`$1`", ".adoc")
	if err != nil {
		return core.NewE100(file.Path, err)
	}

	cmd.Stdin = strings.NewReader(s)
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return core.NewE100(file.Path, err)
	}

	// NOTE: Asciidoctor converts "'" to "’".
	//
	// See #206.
	var sanitizer = strings.NewReplacer(
		"\u2018", "&apos;",
		"\u2019", "&apos;",
		"&#8217;", "&apos;",
		"&rsquo;", "&apos;")
	input := sanitizer.Replace(out.String())

	l.lintHTMLTokens(file, file.Content, []byte(input), 0)
	return nil
}

func (l Linter) lintXML(file *core.File) error {
	var out bytes.Buffer

	xsltproc := core.Which([]string{"xsltproc", "xsltproc.exe"})
	if xsltproc == "" {
		return core.NewE100("lintXML", errors.New("xsltproc not found"))
	} else if file.Transform == "" {
		return core.NewE100(
			"lintXML",
			errors.New("no XSLT transform provided"))
	}

	xsltArgs = append(xsltArgs, []string{file.Transform, "-"}...)

	cmd := exec.Command(xsltproc, xsltArgs...)
	cmd.Stdin = strings.NewReader(file.Content)
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return core.NewE100(file.Path, err)
	}

	l.lintHTMLTokens(file, file.Content, out.Bytes(), 0)
	return nil
}

func (l Linter) lintDITA(file *core.File) error {
	dita := core.Which([]string{"dita", "dita.bat"})
	if dita == "" {
		return core.NewE100("lintDITA", errors.New("dita not found"))
	}

	tempDir, err := ioutil.TempDir("", "dita-")
	defer os.RemoveAll(tempDir)

	if err != nil {
		return core.NewE201FromPosition(err.Error(), file.Path, 1)
	}

	// FIXME: The `dita` command is *slow* (~4s per file)!
	cmd := exec.Command(dita, []string{
		"-i", file.Path, "-f", "html5", "-o", tempDir, "--nav-toc=none"}...)

	if err := cmd.Run(); err != nil {
		return core.NewE100(file.Path, err)
	}

	basename := filepath.Base(file.Path)
	data, err := ioutil.ReadFile(filepath.Join(
		tempDir,
		strings.TrimSuffix(basename, filepath.Ext(basename))+".html"))

	if err != nil {
		return core.NewE201FromPosition(err.Error(), file.Path, 1)
	}

	// NOTE: We have to remove the `<head>` tag to avoid
	// introducing new content into the HTML.
	head1 := bytes.Index(data, []byte("<head>"))
	head2 := bytes.Index(data, []byte("</head>"))
	l.lintHTMLTokens(
		file, file.Content, append(data[:head1], data[head2:]...), 0)

	return nil
}
