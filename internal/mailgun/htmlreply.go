package mailgun

import (
	"html"
	"strings"
)

// The HTML reply is a transformer's report fragment with FileMill's own lines
// around it: a caption naming the file, the links of a sheets-link reply, and
// the support line. Those lines say what the text reply says, in the same
// words, styled the way mail clients keep: inline, with a system font.
//
// Everything FileMill inserts is escaped. The fragment goes in as given: a
// transformer is operator-installed code, trusted like its output files, and
// it escapes its own data.
const (
	htmlFont         = "-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif"
	htmlParagraphCSS = "margin:16px;font-family:" + htmlFont + ";font-size:14px;line-height:1.5;color:#1f2937;"
	htmlCaptionCSS   = "margin:16px 16px 8px 16px;font-family:" + htmlFont + ";font-size:13px;color:#6b7280;"
	htmlFooterCSS    = "margin:16px;font-family:" + htmlFont + ";font-size:13px;line-height:1.5;color:#6b7280;"
)

// htmlReply is the whole HTML document: each job's section in attachment
// order, then the links, then the support line.
func htmlReply(sections, links, labels []string, support string) string {
	var b strings.Builder
	b.WriteString("<!doctype html>\n" +
		`<html><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"></head>` +
		`<body style="margin:0;padding:0;">`)
	for _, section := range sections {
		b.WriteString(section)
	}
	if len(links) > 0 {
		b.WriteString(htmlLinks(links, labels))
	}
	if support != "" {
		b.WriteString(htmlSupportFooter(support))
	}
	b.WriteString("</body></html>\n")
	return b.String()
}

// htmlCaption names the file a job's report is about, above its fragment. It
// is kept even for a single job: the fragment's own heading names what the
// report found, not which attachment it came from.
func htmlCaption(inputName, label string) string {
	return `<p style="` + htmlCaptionCSS + `">` + html.EscapeString(inputName+" ("+label+")") + "</p>"
}

// plainParagraph is a job without a fragment of its own - a failed job, or
// one whose transformer sent text only - as its text reply line, line breaks
// kept.
func plainParagraph(line string) string {
	escaped := html.EscapeString(strings.ReplaceAll(line, "\r\n", "\n"))
	return `<p style="` + htmlParagraphCSS + `">` + strings.ReplaceAll(escaped, "\n", "<br>") + "</p>"
}

// htmlLinks is withLinks for the HTML reply: the same sentence, with each link
// an anchor.
func htmlLinks(links, labels []string) string {
	var b strings.Builder
	b.WriteString(`<p style="` + htmlParagraphCSS + `">` + html.EscapeString(linkSentence(links, labels)))
	for _, link := range links {
		escaped := html.EscapeString(link)
		b.WriteString(`<br><a href="` + escaped + `">` + escaped + "</a>")
	}
	b.WriteString("</p>")
	return b.String()
}

// htmlSupportFooter is withSupportFooter for the HTML reply, with the address
// a mailto link.
func htmlSupportFooter(address string) string {
	escaped := html.EscapeString(address)
	return `<p style="` + htmlFooterCSS + `">` + html.EscapeString(supportInvitation) +
		` <a href="mailto:` + escaped + `">` + escaped + "</a>.</p>"
}
