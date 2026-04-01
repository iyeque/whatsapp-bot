package utils

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// SearchWeb performs a web search using DuckDuckGo HTML and returns a summary of results
func SearchWeb(query string) (string, error) {
	// Try the HTML version first
	results, err := scrapeDuckDuckGo("https://html.duckduckgo.com/html/?q=", query)
	if err == nil && results != "" {
		return results, nil
	}

	// Fallback to the lite version if HTML version fails or is empty
	return scrapeDuckDuckGo("https://lite.duckduckgo.com/lite/?q=", query)
}

var (
	titleRegex   = regexp.MustCompile(`class="result__a"[^>]*>([^<]+)</a>`)
	snippetRegex = regexp.MustCompile(`class="result__snippet"[^>]*>([\s\S]*?)</a>`)
	liteTitleRegex   = regexp.MustCompile(`class='result-link'[^>]*>([^<]+)</a>`)
	liteSnippetRegex = regexp.MustCompile(`class='result-snippet'[^>]*>([\s\S]*?)</td>`)
	fallbackTitleRegex = regexp.MustCompile(`<a[^>]+href="http[^"]+"[^>]*>([^<]{20,})</a>`)
	tagRegex = regexp.MustCompile(`<[^>]*>`)
)

func scrapeDuckDuckGo(baseURL string, query string) (string, error) {
	searchURL := baseURL + url.QueryEscape(query)
	
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return "", err
	}
	
	// Use a very standard browser user agent
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	html := string(body)
	
	var titles [][]string
	var snippets [][]string

	if strings.Contains(baseURL, "lite") {
		titles = liteTitleRegex.FindAllStringSubmatch(html, 8)
		snippets = liteSnippetRegex.FindAllStringSubmatch(html, 8)
	} else {
		titles = titleRegex.FindAllStringSubmatch(html, 8)
		snippets = snippetRegex.FindAllStringSubmatch(html, 8)
	}

	if len(titles) == 0 {
		titles = fallbackTitleRegex.FindAllStringSubmatch(html, 5)
	}

	if len(titles) == 0 {
		return "", fmt.Errorf("no results found in HTML")
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Web search results for '%s':\n\n", query))
	
	for i := 0; i < len(titles); i++ {
		title := strings.TrimSpace(titles[i][1])
		snippet := "No description available."
		if i < len(snippets) {
			snippet = tagRegex.ReplaceAllString(snippets[i][1], "")
			snippet = strings.TrimSpace(snippet)
		}
		
		// Clean up common HTML entities
		title = cleanHTML(title)
		snippet = cleanHTML(snippet)

		builder.WriteString(fmt.Sprintf("%d. %s\n   %s\n\n", i+1, title, snippet))
	}

	return builder.String(), nil
}

func cleanHTML(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return s
}
