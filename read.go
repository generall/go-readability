package readability

import (
	"bytes"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	wl "github.com/abadojack/whatlanggo"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	ghtml "html"
	"io/ioutil"
	"math"
	"net/http"
	nurl "net/url"
	pt "path"
	"regexp"
	"strings"
	"time"
)

var (
	unlikelyCandidates   = regexp.MustCompile(`(?is)banner|breadcrumbs|combx|comment|community|cover-wrap|disqus|extra|foot|header|legends|menu|related|remark|replies|rss|shoutbox|sidebar|skyscraper|social|sponsor|supplemental|ad-break|agegate|pagination|pager|popup|yom-remote`)
	okMaybeItsACandidate = regexp.MustCompile(`(?is)and|article|body|column|main|shadow`)
	positive             = regexp.MustCompile(`(?is)article|body|content|entry|hentry|h-entry|main|page|pagination|post|text|blog|story`)
	negative             = regexp.MustCompile(`(?is)hidden|^hid$| hid$| hid |^hid |banner|combx|comment|com-|contact|foot|footer|footnote|masthead|media|meta|outbrain|promo|related|scroll|share|shoutbox|sidebar|skyscraper|sponsor|shopping|tags|tool|widget`)
	extraneous           = regexp.MustCompile(`(?is)print|archive|comment|discuss|e[\-]?mail|share|reply|all|login|sign|single|utility`)
	byline               = regexp.MustCompile(`(?is)byline|author|dateline|writtenby|p-author`)
	divToPElements       = regexp.MustCompile(`(?is)<(a|blockquote|dl|div|img|ol|p|pre|table|ul|select)`)
	replaceBrs           = regexp.MustCompile(`(?is)(<br[^>]*>[ \n\r\t]*){2,}`)
	killBreaks           = regexp.MustCompile(`(?is)(<br\s*/?>(\s|&nbsp;?)*)+`)
	videos               = regexp.MustCompile(`(?is)//(www\.)?(dailymotion|youtube|youtube-nocookie|player\.vimeo)\.com`)
	unlikelyElements     = regexp.MustCompile(`(?is)(input|time|button)`)
	pIsSentence          = regexp.MustCompile(`(?is)\.( |$)`)
	spaces               = regexp.MustCompile(`(?is)\s{2,}`)
	comments             = regexp.MustCompile(`(?is)<!--[^>]+-->`)
)

type candidateItem struct {
	score float64
	node  *goquery.Selection
}

type readability struct {
	html       string
	url        *nurl.URL
	candidates map[string]candidateItem
}

// Metadata is metadata of an article
type Metadata struct {
	Title       string
	Image       string
	Excerpt     string
	Author      string
	MinReadTime int
	MaxReadTime int
}

// Article is the content of an URL
type Article struct {
	URL        string
	Meta       Metadata
	Content    string
	RawContent string
}

// Parse an URL to readability format
func Parse(url string, timeout time.Duration) (Article, error) {
	// Make sure url is valid
	parsedURL, err := nurl.Parse(url)
	if err != nil {
		return Article{}, err
	}

	// Fetch page from URL
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return Article{}, err
	}
	defer resp.Body.Close()

	btHTML, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return Article{}, err
	}
	strHTML := string(btHTML)

	// Replaces 2 or more successive <br> elements with a single <p>.
	// Whitespace between <br> elements are ignored. For example:
	//   <div>foo<br>bar<br> <br><br>abc</div>
	// will become:
	//   <div>foo<br>bar<p>abc</p></div>
	strHTML = replaceBrs.ReplaceAllString(strHTML, "</p><p>")
	strHTML = strings.TrimSpace(strHTML)

	// Check if HTML page is empty
	if strHTML == "" {
		return Article{}, fmt.Errorf("HTML is empty")
	}

	// Create goquery document
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(strHTML))
	if err != nil {
		return Article{}, err
	}

	// Create new readability
	r := readability{
		url:        parsedURL,
		candidates: make(map[string]candidateItem),
	}

	// Prepare document and fetch content
	r.prepareDocument(doc)
	contentNode := r.getArticleContent(doc)

	// Get article metadata
	meta := r.getArticleMetadata(doc)
	meta.MinReadTime, meta.MaxReadTime = r.estimateReadTime(contentNode)

	// Get text and HTML from content
	textContent := ""
	htmlContent := ""
	if contentNode != nil {
		// If we haven't found an excerpt in the article's metadata, use the first paragraph
		if meta.Excerpt == "" {
			p := contentNode.Find("p").First().Text()
			meta.Excerpt = normalizeText(p)
		}

		// Get content text and HTML
		textContent = r.getTextContent(contentNode)
		htmlContent = r.getHTMLContent(contentNode)
	}

	article := Article{
		URL:        parsedURL.String(),
		Meta:       meta,
		Content:    textContent,
		RawContent: htmlContent,
	}

	return article, nil
}

// Prepare the HTML document for readability to scrape it.
// This includes things like stripping Javascript, CSS, and handling terrible markup.
func (r *readability) prepareDocument(doc *goquery.Document) {
	// Remove tags
	doc.Find("script").Remove()
	doc.Find("noscript").Remove()
	doc.Find("style").Remove()
	doc.Find("link").Remove()

	// Replace font tags to span
	doc.Find("font").Each(func(_ int, font *goquery.Selection) {
		html, _ := font.Html()
		font.ReplaceWithHtml("<span>" + html + "</span>")
	})
}

// Attempts to get metadata for the article.
func (r *readability) getArticleMetadata(doc *goquery.Document) Metadata {
	metadata := Metadata{}
	mapAttribute := make(map[string]string)

	doc.Find("meta").Each(func(_ int, meta *goquery.Selection) {
		metaName, _ := meta.Attr("name")
		metaProperty, _ := meta.Attr("property")
		metaContent, _ := meta.Attr("content")

		metaName = strings.TrimSpace(metaName)
		metaProperty = strings.TrimSpace(metaProperty)
		metaContent = strings.TrimSpace(metaContent)

		// Fetch author name
		if strings.Contains(metaName+metaProperty, "author") {
			metadata.Author = metaContent
			return
		}

		// Fetch description and title
		if metaName == "title" ||
			metaName == "description" ||
			metaName == "twitter:title" ||
			metaName == "twitter:image" ||
			metaName == "twitter:description" {
			if _, exist := mapAttribute[metaName]; !exist {
				mapAttribute[metaName] = metaContent
			}
			return
		}

		if metaProperty == "og:description" ||
			metaProperty == "og:image" ||
			metaProperty == "og:title" {
			if _, exist := mapAttribute[metaProperty]; !exist {
				mapAttribute[metaProperty] = metaContent
			}
			return
		}
	})

	// Set final image
	if _, exist := mapAttribute["og:image"]; exist {
		metadata.Image = mapAttribute["og:image"]
	} else if _, exist := mapAttribute["twitter:image"]; exist {
		metadata.Image = mapAttribute["twitter:image"]
	}

	if metadata.Image != "" && strings.HasPrefix(metadata.Image, "//") {
		metadata.Image = "http:" + metadata.Image
	}

	// Set final description
	if _, exist := mapAttribute["description"]; exist {
		metadata.Excerpt = mapAttribute["description"]
	} else if _, exist := mapAttribute["og:description"]; exist {
		metadata.Excerpt = mapAttribute["og:description"]
	} else if _, exist := mapAttribute["twitter:description"]; exist {
		metadata.Excerpt = mapAttribute["twitter:description"]
	}

	// Set final title
	metadata.Title = r.getArticleTitle(doc)
	if metadata.Title == "" {
		if _, exist := mapAttribute["og:title"]; exist {
			metadata.Title = mapAttribute["og:title"]
		} else if _, exist := mapAttribute["twitter:title"]; exist {
			metadata.Title = mapAttribute["twitter:title"]
		}
	}

	return metadata
}

// Get the article title
func (r *readability) getArticleTitle(doc *goquery.Document) string {
	// Get title tag
	title := doc.Find("title").First().Text()
	title = normalizeText(title)
	originalTitle := title

	// Create list of separator
	separators := []string{`|`, `-`, `\`, `/`, `>`, `»`}
	hierarchialSeparators := []string{`\`, `/`, `>`, `»`}

	// If there's a separator in the title, first remove the final part
	titleHadHierarchicalSeparators := false
	if idx, sep := findSeparator(title, separators...); idx != -1 {
		titleHadHierarchicalSeparators = hasSeparator(title, hierarchialSeparators...)

		index := strings.LastIndex(originalTitle, sep)
		title = originalTitle[:index]

		// If the resulting title is too short (3 words or fewer), remove
		// the first part instead:
		if len(strings.Fields(title)) < 3 {
			index = strings.Index(originalTitle, sep)
			title = originalTitle[index+1:]
		}
	} else if strings.Contains(title, ": ") {
		// Check if we have an heading containing this exact string, so we
		// could assume it's the full title.
		existInHeading := false
		doc.Find("h1,h2").EachWithBreak(func(_ int, heading *goquery.Selection) bool {
			headingText := strings.TrimSpace(heading.Text())
			if headingText == title {
				existInHeading = true
				return false
			}

			return true
		})

		// If we don't, let's extract the title out of the original title string.
		if !existInHeading {
			index := strings.LastIndex(originalTitle, ":")
			title = originalTitle[index+1:]

			// If the title is now too short, try the first colon instead:
			if len(strings.Fields(title)) < 3 {
				index = strings.Index(originalTitle, ":")
				title = originalTitle[index+1:]
				// But if we have too many words before the colon there's something weird
				// with the titles and the H tags so let's just use the original title instead
			} else {
				index = strings.Index(originalTitle, ":")
				title = originalTitle[:index]
				if len(strings.Fields(title)) > 5 {
					title = originalTitle
				}
			}
		}
	} else if strLen(title) > 150 || strLen(title) < 15 {
		hOne := doc.Find("h1").First()
		if hOne != nil {
			title = normalizeText(hOne.Text())
		}
	}

	// If we now have 4 words or fewer as our title, and either no
	// 'hierarchical' separators (\, /, > or ») were found in the original
	// title or we decreased the number of words by more than 1 word, use
	// the original title.
	curTitleWordCount := len(strings.Fields(title))
	noSeparatorWordCount := len(strings.Fields(removeSeparator(originalTitle, separators...)))
	if curTitleWordCount <= 4 && (!titleHadHierarchicalSeparators || curTitleWordCount != noSeparatorWordCount-1) {
		title = originalTitle
	}

	return title
}

// Using a variety of metrics (content score, classname, element types), find the content that is
// most likely to be the stuff a user wants to read. Then return it wrapped up in a div.
func (r *readability) getArticleContent(doc *goquery.Document) *goquery.Selection {
	// First, node prepping. Trash nodes that look cruddy (like ones with the
	// class name "comment", etc), and turn divs into P tags where they have been
	// used inappropriately (as in, where they contain no other block level elements.)
	doc.Find("*").Each(func(i int, s *goquery.Selection) {
		matchString := s.AttrOr("class", "") + " " + s.AttrOr("id", "")

		// If byline, remove this element
		if rel := s.AttrOr("rel", ""); rel == "author" || byline.MatchString(matchString) {
			s.Remove()
			return
		}

		// Remove unlikely candidates
		if unlikelyCandidates.MatchString(matchString) &&
			!okMaybeItsACandidate.MatchString(matchString) &&
			!s.Is("body") && !s.Is("a") {
			s.Remove()
			return
		}

		if unlikelyElements.MatchString(r.getTagName(s)) {
			s.Remove()
			return
		}

		// Remove DIV, SECTION, and HEADER nodes without any content(e.g. text, image, video, or iframe).
		if s.Is("div,section,header,h1,h2,h3,h4,h5,h6") && r.isElementEmpty(s) {
			s.Remove()
			return
		}

		// Turn all divs that don't have children block level elements into p's
		if s.Is("div") {
			sHTML, _ := s.Html()
			if !divToPElements.MatchString(sHTML) {
				s.Nodes[0].Data = "p"
			}
		}
	})

	// Loop through all paragraphs, and assign a score to them based on how content-y they look.
	// Then add their score to their parent node.
	// A score is determined by things like number of commas, class names, etc. Maybe eventually link density.
	r.candidates = make(map[string]candidateItem)
	doc.Find("p").Each(func(i int, s *goquery.Selection) {
		// If this paragraph is less than 25 characters, don't even count it.
		innerText := normalizeText(s.Text())
		if strLen(innerText) < 25 {
			return
		}

		// Exclude nodes with no ancestor.
		ancestors := r.getNodeAncestors(s, 3)
		if len(ancestors) == 0 {
			return
		}

		// Calculate content score
		// Add a point for the paragraph itself as a base.
		contentScore := 1.0

		// Add points for any commas within this paragraph.
		contentScore += float64(strings.Count(innerText, ","))
		contentScore += float64(strings.Count(innerText, "，"))

		// For every 100 characters in this paragraph, add another point. Up to 3 points.
		contentScore += math.Min(math.Floor(float64(strLen(innerText)/100)), 3)

		// Initialize and score ancestors.
		for level, ancestor := range ancestors {
			// Node score divider:
			// - parent:             1 (no division)
			// - grandparent:        2
			// - great grandparent+: ancestor level * 3
			scoreDivider := 0
			if level == 0 {
				scoreDivider = 1
			} else if level == 1 {
				scoreDivider = 2
			} else {
				scoreDivider = level * 3
			}

			ancestorHash := hashStr(ancestor)
			if _, ok := r.candidates[ancestorHash]; !ok {
				r.candidates[ancestorHash] = r.initializeNodeScore(ancestor)
			}

			candidate := r.candidates[ancestorHash]
			candidate.score += contentScore / float64(scoreDivider)
			r.candidates[ancestorHash] = candidate
		}
	})

	// After we've calculated scores, loop through all of the possible
	// candidate nodes we found and find the one with the highest score.
	var topCandidate *candidateItem
	for hash, candidate := range r.candidates {
		candidate.score = candidate.score * (1 - r.getLinkDensity(candidate.node))
		r.candidates[hash] = candidate

		if topCandidate == nil || candidate.score > topCandidate.score {
			if topCandidate == nil {
				topCandidate = new(candidateItem)
			}

			topCandidate.score = candidate.score
			topCandidate.node = candidate.node
		}
	}

	// If top candidate not found, stop
	if topCandidate == nil {
		return nil
	}

	r.prepArticle(topCandidate.node)
	return topCandidate.node
}

// Check if a node is empty
func (r *readability) isElementEmpty(s *goquery.Selection) bool {
	html, _ := s.Html()
	html = strings.TrimSpace(html)
	return html == ""
}

// Get tag name from a node
func (r *readability) getTagName(s *goquery.Selection) string {
	if s == nil || len(s.Nodes) == 0 {
		return ""
	}
	return s.Nodes[0].Data
}

func (r *readability) getNodeAncestors(node *goquery.Selection, maxDepth int) []*goquery.Selection {
	ancestors := []*goquery.Selection{}
	parent := *node

	for i := 0; i < maxDepth; i++ {
		parent = *parent.Parent()
		if len(parent.Nodes) == 0 {
			return ancestors
		}

		ancestors = append(ancestors, &parent)
	}

	return ancestors
}

// Check if a given node has one of its ancestor tag name matching the provided one.
func (r *readability) hasAncestorTag(node *goquery.Selection, tag string) bool {
	for parent := *node; len(parent.Nodes) > 0; parent = *parent.Parent() {
		if parent.Nodes[0].Data == tag {
			return true
		}
	}

	return false
}

// Initialize a node and checks the className/id for special names
// to add to its score.
func (r *readability) initializeNodeScore(node *goquery.Selection) candidateItem {
	contentScore := 0.0
	switch r.getTagName(node) {
	case "article":
		contentScore += 10
	case "section":
		contentScore += 8
	case "div":
		contentScore += 5
	case "pre", "blockquote", "td":
		contentScore += 3
	case "form", "ol", "ul", "dl", "dd", "dt", "li", "address":
		contentScore -= 3
	case "th", "h1", "h2", "h3", "h4", "h5", "h6":
		contentScore -= 5
	}

	contentScore += r.getClassWeight(node)
	return candidateItem{contentScore, node}
}

// Get an elements class/id weight. Uses regular expressions to tell if this
// element looks good or bad.
func (r *readability) getClassWeight(node *goquery.Selection) float64 {
	weight := 0.0
	if str, b := node.Attr("class"); b {
		if negative.MatchString(str) {
			weight -= 25
		}

		if positive.MatchString(str) {
			weight += 25
		}
	}

	if str, b := node.Attr("id"); b {
		if negative.MatchString(str) {
			weight -= 25
		}

		if positive.MatchString(str) {
			weight += 25
		}
	}

	return weight
}

// Get the density of links as a percentage of the content
// This is the amount of text that is inside a link divided by the total text in the node.
func (r *readability) getLinkDensity(node *goquery.Selection) float64 {
	if node == nil {
		return 0
	}

	textLength := strLen(normalizeText(node.Text()))
	if textLength == 0 {
		return 0
	}

	linkLength := 0
	node.Find("a").Each(func(_ int, link *goquery.Selection) {
		linkLength += strLen(link.Text())
	})

	return float64(linkLength) / float64(textLength)
}

// Prepare the article node for display. Clean out any inline styles,
// iframes, forms, strip extraneous <p> tags, etc.
func (r *readability) prepArticle(content *goquery.Selection) {
	if content == nil {
		return
	}

	// Remove styling attribute
	r.cleanStyle(content)

	// Clean out junk from the article content
	r.cleanConditionally(content, "form")
	r.cleanConditionally(content, "fieldset")
	r.clean(content, "h1")
	r.clean(content, "object")
	r.clean(content, "embed")
	r.clean(content, "footer")
	r.clean(content, "link")

	// If there is only one h2 or h3 and its text content substantially equals article title,
	// they are probably using it as a header and not a subheader,
	// so remove it since we already extract the title separately.
	if content.Find("h2").Length() == 1 {
		r.clean(content, "h2")
	}

	if content.Find("h3").Length() == 1 {
		r.clean(content, "h3")
	}

	r.clean(content, "iframe")
	r.clean(content, "input")
	r.clean(content, "textarea")
	r.clean(content, "select")
	r.clean(content, "button")
	r.cleanHeaders(content)

	// Do these last as the previous stuff may have removed junk
	// that will affect these
	r.cleanConditionally(content, "table")
	r.cleanConditionally(content, "ul")
	r.cleanConditionally(content, "div")

	// Fix all relative URL
	r.fixRelativeURIs(content)

	// Last time, clean all empty tags and remove class name
	content.Find("*").Each(func(_ int, s *goquery.Selection) {
		if r.isElementEmpty(s) {
			s.Remove()
		}

		s.RemoveAttr("class")
		s.RemoveAttr("id")
	})
}

// Remove the style attribute on every e and under.
func (r *readability) cleanStyle(s *goquery.Selection) {
	s.Find("*").Each(func(i int, s1 *goquery.Selection) {
		tagName := s1.Nodes[0].Data
		if tagName == "svg" {
			return
		}

		s1.RemoveAttr("align")
		s1.RemoveAttr("background")
		s1.RemoveAttr("bgcolor")
		s1.RemoveAttr("border")
		s1.RemoveAttr("cellpadding")
		s1.RemoveAttr("cellspacing")
		s1.RemoveAttr("frame")
		s1.RemoveAttr("hspace")
		s1.RemoveAttr("rules")
		s1.RemoveAttr("style")
		s1.RemoveAttr("valign")
		s1.RemoveAttr("vspace")
		s1.RemoveAttr("onclick")
		s1.RemoveAttr("onmouseover")
		s1.RemoveAttr("border")
		s1.RemoveAttr("style")

		if tagName != "table" && tagName != "th" && tagName != "td" &&
			tagName != "hr" && tagName != "pre" {
			s1.RemoveAttr("width")
			s1.RemoveAttr("height")
		}
	})
}

// Clean a node of all elements of type "tag".
// (Unless it's a youtube/vimeo video. People love movies.)
func (r *readability) clean(s *goquery.Selection, tag string) {
	if s == nil {
		return
	}

	isEmbed := false
	if tag == "object" || tag == "embed" || tag == "iframe" {
		isEmbed = true
	}

	s.Find(tag).Each(func(i int, target *goquery.Selection) {
		attributeValues := ""
		for _, attribute := range target.Nodes[0].Attr {
			attributeValues += " " + attribute.Val
		}

		if isEmbed && videos.MatchString(attributeValues) {
			return
		}

		if isEmbed && videos.MatchString(target.Text()) {
			return
		}

		target.Remove()
	})
}

// Clean an element of all tags of type "tag" if they look fishy.
// "Fishy" is an algorithm based on content length, classnames, link density, number of images & embeds, etc.
func (r *readability) cleanConditionally(e *goquery.Selection, tag string) {
	if e == nil {
		return
	}

	isList := tag == "ul" || tag == "ol"

	e.Find(tag).Each(func(i int, node *goquery.Selection) {
		contentScore := 0.0
		weight := r.getClassWeight(node)
		if weight+contentScore < 0 {
			node.Remove()
			return
		}

		// If there are not very many commas, and the number of
		// non-paragraph elements is more than paragraphs or other
		// ominous signs, remove the element.
		nodeText := normalizeText(node.Text())
		nCommas := strings.Count(nodeText, ",")
		nCommas += strings.Count(nodeText, "，")
		if nCommas < 10 {
			p := node.Find("p").Length()
			img := node.Find("img").Length()
			li := node.Find("li").Length() - 100
			input := node.Find("input").Length()

			embedCount := 0
			node.Find("embed").Each(func(i int, embed *goquery.Selection) {
				if !videos.MatchString(embed.AttrOr("src", "")) {
					embedCount++
				}
			})

			linkDensity := r.getLinkDensity(node)
			contentLength := strLen(normalizeText(node.Text()))
			haveToRemove := (!isList && li > p) ||
				(img > 1 && float64(p)/float64(img) < 0.5 && !r.hasAncestorTag(node, "figure")) ||
				(float64(input) > math.Floor(float64(p)/3)) ||
				(!isList && contentLength < 25 && (img == 0 || img > 2) && !r.hasAncestorTag(node, "figure")) ||
				(!isList && weight < 25 && linkDensity > 0.2) ||
				(weight >= 25 && linkDensity > 0.5) ||
				((embedCount == 1 && contentLength < 75) || embedCount > 1)

			if haveToRemove {
				node.Remove()
			}
		}
	})
}

// Clean out spurious headers from an Element. Checks things like classnames and link density.
func (r *readability) cleanHeaders(s *goquery.Selection) {
	s.Find("h1,h2,h3").Each(func(_ int, s1 *goquery.Selection) {
		if r.getClassWeight(s1) < 0 {
			s1.Remove()
		}
	})
}

// Converts each <a> and <img> uri in the given element to an absolute URI,
// ignoring #ref URIs.
func (r *readability) fixRelativeURIs(node *goquery.Selection) {
	if node == nil {
		return
	}

	node.Find("img").Each(func(i int, img *goquery.Selection) {
		src := img.AttrOr("src", "")
		if file, ok := img.Attr("file"); ok {
			src = file
			img.SetAttr("src", file)
			img.RemoveAttr("file")
		}

		if src == "" {
			img.Remove()
			return
		}

		if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
			newSrc := nurl.URL(*r.url)
			if strings.HasPrefix(src, "/") {
				newSrc.Path = src
			} else {
				newSrc.Path = pt.Join(newSrc.Path, src)
			}
			img.SetAttr("src", newSrc.String())
		}
	})

	node.Find("a").Each(func(_ int, link *goquery.Selection) {
		if href, ok := link.Attr("href"); ok {
			if !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
				newHref := nurl.URL(*r.url)
				if strings.HasPrefix(href, "/") {
					newHref.Path = href
				} else if !strings.HasPrefix(href, "#") {
					newHref.Path = pt.Join(newHref.Path, href)
				}
				link.SetAttr("href", newHref.String())
			}
		}
	})
}

// Estimate read time based on the language number of character in contents.
// Using data from http://iovs.arvojournals.org/article.aspx?articleid=2166061
func (r *readability) estimateReadTime(content *goquery.Selection) (int, int) {
	if content == nil {
		return 0, 0
	}

	// Check the language
	contentText := normalizeText(content.Text())
	lang := wl.LangToString(wl.DetectLang(contentText))

	// Get number of words and images
	nChar := strLen(contentText)
	nImg := content.Find("img").Length()
	if nChar == 0 && nImg == 0 {
		return 0, 0
	}

	// Calculate character per minute by language
	// Fallback to english
	var cpm, sd float64
	switch lang {
	case "arb":
		sd = 88
		cpm = 612
	case "nld":
		sd = 143
		cpm = 978
	case "fin":
		sd = 121
		cpm = 1078
	case "fra":
		sd = 126
		cpm = 998
	case "deu":
		sd = 86
		cpm = 920
	case "heb":
		sd = 130
		cpm = 833
	case "ita":
		sd = 140
		cpm = 950
	case "jpn":
		sd = 56
		cpm = 357
	case "pol":
		sd = 126
		cpm = 916
	case "por":
		sd = 145
		cpm = 913
	case "rus":
		sd = 175
		cpm = 986
	case "slv":
		sd = 145
		cpm = 885
	case "spa":
		sd = 127
		cpm = 1025
	case "swe":
		sd = 156
		cpm = 917
	case "tur":
		sd = 156
		cpm = 1054
	default:
		sd = 188
		cpm = 987
	}

	// Calculate read time, assumed one image requires 12 second (0.2 minute)
	minReadTime := float64(nChar)/(cpm+sd) + float64(nImg)*0.2
	maxReadTime := float64(nChar)/(cpm-sd) + float64(nImg)*0.2

	// Round number
	minReadTime = math.Floor(minReadTime + 0.5)
	maxReadTime = math.Floor(maxReadTime + 0.5)

	return int(minReadTime), int(maxReadTime)
}

func (r *readability) getHTMLContent(content *goquery.Selection) string {
	html, err := content.Html()
	if err != nil {
		return ""
	}

	html = ghtml.UnescapeString(html)
	html = comments.ReplaceAllString(html, "")
	html = killBreaks.ReplaceAllString(html, "<br />")
	html = spaces.ReplaceAllString(html, " ")
	return html
}

func (r *readability) getTextContent(content *goquery.Selection) string {
	var buf bytes.Buffer

	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			nodeText := normalizeText(n.Data)
			if nodeText != "" {
				buf.WriteString(nodeText)
			}
		} else if n.Parent != nil && n.Parent.DataAtom != atom.P {
			buf.WriteString("|X|")
		}

		if n.FirstChild != nil {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}
		}
	}

	for _, n := range content.Nodes {
		f(n)
	}

	finalContent := ""
	paragraphs := strings.Split(buf.String(), "|X|")
	for _, paragraph := range paragraphs {
		if paragraph != "" {
			finalContent += paragraph + "\n\n"
		}
	}

	finalContent = strings.TrimSpace(finalContent)
	return finalContent
}
