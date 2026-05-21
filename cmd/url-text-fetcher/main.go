package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	htmlstd "html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	serverName              = "url-text-fetcher-go"
	serverVersion           = "0.1.0"
	defaultProtocol         = "2024-11-05"
	duckDuckGoLiteURL       = "https://lite.duckduckgo.com/lite/"
	searchUserAgent         = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
	maxRequestBytes         = 1 << 20
	maxDownloadBytes        = 2 << 20
	defaultMaxChars         = 20000
	maxOutputChars          = 100000
	defaultMaxLinks         = 200
	maxLinks                = 500
	defaultMaxSearchResults = 10
	maxSearchResults        = 50
)

var (
	blockTagPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`),
		regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style\s*>`),
		regexp.MustCompile(`(?is)<noscript\b[^>]*>.*?</noscript\s*>`),
		regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</svg\s*>`),
	}
	breakTagPattern = regexp.MustCompile(`(?i)<\s*/?\s*(br|p|div|li|tr|td|th|h[1-6]|section|article|header|footer|main|nav|pre|blockquote)\b[^>]*>`)
	anyTagPattern   = regexp.MustCompile(`(?s)<[^>]+>`)
	anchorPattern   = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a\s*>`)
	hrefAttrPattern = regexp.MustCompile(`(?is)\bhref\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	spacePattern    = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankPattern    = regexp.MustCompile(`\n{3,}`)

	blockedIPPrefixes = mustPrefixes([]string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"192.168.0.0/16",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"::/128",
		"::1/128",
		"64:ff9b:1::/48",
		"100::/64",
		"2001::/23",
		"2001:db8::/32",
		"fc00::/7",
		"fe80::/10",
		"ff00::/8",
	})
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolCallParams struct {
	Name      string                     `json:"name"`
	Arguments map[string]json.RawMessage `json:"arguments"`
}

type pageLink struct {
	Text string `json:"text,omitempty"`
	URL  string `json:"url"`
}

type searchResult struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

func main() {
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), maxRequestBytes)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var request rpcRequest
		if err := json.Unmarshal(line, &request); err != nil {
			writeResponse(encoder, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "Parse error"}})
			continue
		}

		if len(request.ID) == 0 {
			continue
		}

		writeResponse(encoder, handleRequest(request))
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stdio read error: %v\n", err)
		os.Exit(1)
	}
}

func writeResponse(encoder *json.Encoder, response rpcResponse) {
	if err := encoder.Encode(response); err != nil {
		fmt.Fprintf(os.Stderr, "stdio write error: %v\n", err)
		os.Exit(1)
	}
}

func handleRequest(request rpcRequest) rpcResponse {
	if request.JSONRPC != "2.0" {
		return errorResponse(request.ID, -32600, "Invalid Request")
	}

	switch request.Method {
	case "initialize":
		return resultResponse(request.ID, initializeResult(request.Params))
	case "ping":
		return resultResponse(request.ID, map[string]any{})
	case "tools/list":
		return resultResponse(request.ID, map[string]any{"tools": toolDefinitions()})
	case "tools/call":
		result, err := callTool(request.Params)
		if err != nil {
			return resultResponse(request.ID, toolResult(err.Error(), true))
		}
		return resultResponse(request.ID, toolResult(result, false))
	case "prompts/list":
		return resultResponse(request.ID, map[string]any{"prompts": []any{}})
	case "resources/list":
		return resultResponse(request.ID, map[string]any{"resources": []any{}})
	default:
		return errorResponse(request.ID, -32601, "Method not found")
	}
}

func initializeResult(params json.RawMessage) map[string]any {
	protocolVersion := defaultProtocol
	var initParams struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &initParams) == nil && initParams.ProtocolVersion != "" {
		protocolVersion = initParams.ProtocolVersion
	}

	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
	}
}

func resultResponse(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id json.RawMessage, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func toolResult(text string, isError bool) map[string]any {
	result := map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	}
	if isError {
		result["isError"] = true
	}
	return result
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "fetch_url_text",
			"description": "Safely fetch readable text from a public HTTP(S) URL. Use fetch_page_links when you need URLs for navigation.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "Absolute public http:// or https:// URL.",
					},
					"max_chars": map[string]any{
						"type":        "integer",
						"description": "Maximum characters returned after text extraction.",
						"default":     defaultMaxChars,
						"minimum":     1000,
						"maximum":     maxOutputChars,
					},
				},
				"required": []string{"url"},
			},
		},
		{
			"name":        "fetch_page_links",
			"description": "Safely fetch a public HTTP(S) page and return navigable links as objects with text and url fields.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "Absolute public http:// or https:// URL.",
					},
					"max_links": map[string]any{
						"type":        "integer",
						"description": "Maximum number of links returned.",
						"default":     defaultMaxLinks,
						"minimum":     1,
						"maximum":     maxLinks,
					},
				},
				"required": []string{"url"},
			},
		},
		{
			"name":        "fetch_search_results",
			"description": "Search the web via DuckDuckGo Lite and return navigable result URLs as objects with title and url fields.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search query.",
					},
					"max_results": map[string]any{
						"type":        "integer",
						"description": "Maximum number of search results returned.",
						"default":     defaultMaxSearchResults,
						"minimum":     1,
						"maximum":     maxSearchResults,
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

func callTool(params json.RawMessage) (string, error) {
	var call toolCallParams
	if err := json.Unmarshal(params, &call); err != nil {
		return "", errors.New("invalid tool call parameters")
	}

	switch call.Name {
	case "fetch_url_text":
		fetchURL, err := stringArg(call.Arguments, "url")
		if err != nil {
			return "", err
		}
		maxChars := intArg(call.Arguments, "max_chars", defaultMaxChars, 1000, maxOutputChars)
		body, contentType, _, err := fetchPublicURL(fetchURL)
		if err != nil {
			return "", err
		}
		text := extractText(body, contentType)
		return limitString(text, maxChars), nil
	case "fetch_page_links":
		fetchURL, err := stringArg(call.Arguments, "url")
		if err != nil {
			return "", err
		}
		maxLinkCount := intArg(call.Arguments, "max_links", defaultMaxLinks, 1, maxLinks)
		body, _, finalURL, err := fetchPublicURL(fetchURL)
		if err != nil {
			return "", err
		}
		links := extractLinks(finalURL, body, maxLinkCount)
		encoded, err := json.MarshalIndent(links, "", "  ")
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	case "fetch_search_results":
		query, err := stringArg(call.Arguments, "query")
		if err != nil {
			return "", err
		}
		maxResultCount := intArg(call.Arguments, "max_results", defaultMaxSearchResults, 1, maxSearchResults)
		results, err := searchDuckDuckGoLite(query, maxResultCount)
		if err != nil {
			return "", err
		}
		encoded, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	default:
		return "", fmt.Errorf("unknown tool: %s", call.Name)
	}
}

func stringArg(args map[string]json.RawMessage, name string) (string, error) {
	value, ok := args[name]
	if !ok || len(value) == 0 {
		return "", fmt.Errorf("missing required argument: %s", name)
	}

	var result string
	if err := json.Unmarshal(value, &result); err != nil || strings.TrimSpace(result) == "" {
		return "", fmt.Errorf("argument %s must be a non-empty string", name)
	}
	return strings.TrimSpace(result), nil
}

func intArg(args map[string]json.RawMessage, name string, fallback, minValue, maxValue int) int {
	value, ok := args[name]
	if !ok || len(value) == 0 {
		return fallback
	}

	var result int
	if err := json.Unmarshal(value, &result); err != nil {
		return fallback
	}
	if result < minValue {
		return minValue
	}
	if result > maxValue {
		return maxValue
	}
	return result
}

func fetchPublicURL(rawURL string) (string, string, string, error) {
	parsed, err := validateURL(rawURL)
	if err != nil {
		return "", "", "", err
	}

	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", "", "", fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("User-Agent", "url-text-fetcher-go/0.1")
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml,text/plain,application/json;q=0.8,*/*;q=0.1")

	response, err := safeHTTPClient().Do(request)
	if err != nil {
		return "", "", "", fmt.Errorf("fetch failed: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("fetch failed: HTTP %d", response.StatusCode)
	}

	contentType := response.Header.Get("Content-Type")
	if !isAllowedContentType(contentType) {
		return "", "", "", fmt.Errorf("blocked content type: %s", contentType)
	}
	if response.ContentLength > maxDownloadBytes {
		return "", "", "", fmt.Errorf("response too large: %d bytes", response.ContentLength)
	}

	limited := io.LimitReader(response.Body, maxDownloadBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return "", "", "", fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxDownloadBytes {
		return "", "", "", fmt.Errorf("response exceeds %d bytes", maxDownloadBytes)
	}

	return strings.ToValidUTF8(string(body), ""), contentType, response.Request.URL.String(), nil
}

func searchDuckDuckGoLite(query string, limit int) ([]searchResult, error) {
	parsed, err := validateURL(duckDuckGoLiteURL)
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("q", query)
	request, err := http.NewRequest(http.MethodPost, parsed.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create search request: %w", err)
	}
	request.Header.Set("User-Agent", searchUserAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := safeHTTPClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("search failed: HTTP %d", response.StatusCode)
	}
	contentType := response.Header.Get("Content-Type")
	if !isAllowedContentType(contentType) {
		return nil, fmt.Errorf("blocked search content type: %s", contentType)
	}
	if response.ContentLength > maxDownloadBytes {
		return nil, fmt.Errorf("search response too large: %d bytes", response.ContentLength)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read search response: %w", err)
	}
	if len(body) > maxDownloadBytes {
		return nil, fmt.Errorf("search response exceeds %d bytes", maxDownloadBytes)
	}

	bodyText := strings.ToValidUTF8(string(body), "")
	results := extractSearchResults(response.Request.URL.String(), bodyText, limit)
	if len(results) == 0 && strings.Contains(bodyText, "Unfortunately, bots use DuckDuckGo too") {
		return nil, errors.New("DuckDuckGo returned a bot challenge instead of search results")
	}
	return results, nil
}

func safeHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           safeDialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          2,
			MaxIdleConnsPerHost:   1,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			_, err := validateURL(req.URL.String())
			return err
		},
	}
}

func validateURL(rawURL string) (*url.URL, error) {
	if strings.ContainsAny(rawURL, "\x00\r\n\t") {
		return nil, errors.New("URL contains invalid control characters")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.User != nil {
		return nil, errors.New("URL user info is not allowed")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("URL must use http:// or https://")
	}
	if parsed.Hostname() == "" || !parsed.IsAbs() {
		return nil, errors.New("URL must be absolute and include a host")
	}
	if parsed.Fragment != "" {
		parsed.Fragment = ""
	}
	if err := validatePort(parsed); err != nil {
		return nil, err
	}
	if err := validatePublicHost(parsed.Hostname()); err != nil {
		return nil, err
	}

	return parsed, nil
}

func validatePort(parsed *url.URL) error {
	port := parsed.Port()
	if port == "" {
		return nil
	}

	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return errors.New("invalid URL port")
	}
	if parsed.Scheme == "http" && number != 80 {
		return errors.New("http URLs may only use port 80")
	}
	if parsed.Scheme == "https" && number != 443 {
		return errors.New("https URLs may only use port 443")
	}
	return nil
}

func validatePublicHost(host string) error {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("localhost URLs are blocked")
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		return validatePublicIP(addr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve host: %w", err)
	}
	if len(addrs) == 0 {
		return errors.New("host did not resolve")
	}

	for _, resolved := range addrs {
		addr, ok := netip.AddrFromSlice(resolved.IP)
		if !ok {
			return fmt.Errorf("invalid resolved IP for %s", host)
		}
		if err := validatePublicIP(addr); err != nil {
			return fmt.Errorf("blocked resolved IP %s: %w", resolved.IP.String(), err)
		}
	}
	return nil
}

func validatePublicIP(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return errors.New("IP is not public internet-routable")
	}

	for _, prefix := range blockedIPPrefixes {
		if prefix.Contains(addr) {
			return errors.New("IP is in a blocked network range")
		}
	}
	return nil
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if err := validatePublicHost(host); err != nil {
		return nil, err
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, resolved := range addrs {
		addr, ok := netip.AddrFromSlice(resolved.IP)
		if !ok || validatePublicIP(addr) != nil {
			continue
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
	}
	return nil, errors.New("no public IP addresses available for host")
}

func isAllowedContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mediaType == "" {
		return true
	}
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/xhtml+xml", "application/rss+xml", "application/atom+xml":
		return true
	default:
		return false
	}
}

func extractText(body string, contentType string) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mediaType != "" && !strings.Contains(mediaType, "html") && !strings.Contains(mediaType, "xml") {
		return strings.TrimSpace(body)
	}

	text := body
	for _, pattern := range blockTagPatterns {
		text = pattern.ReplaceAllString(text, "\n")
	}
	text = breakTagPattern.ReplaceAllString(text, "\n")
	text = anyTagPattern.ReplaceAllString(text, " ")
	text = htmlstd.UnescapeString(text)
	text = normalizeWhitespace(text)
	return text
}

func normalizeWhitespace(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(spacePattern.ReplaceAllString(line, " "))
		if line != "" {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(blankPattern.ReplaceAllString(strings.Join(kept, "\n"), "\n\n"))
}

func extractLinks(baseURL string, body string, limit int) []pageLink {
	base, err := url.Parse(baseURL)
	if err != nil {
		return []pageLink{}
	}

	seen := map[string]struct{}{}
	links := make([]pageLink, 0, limit)
	matches := anchorPattern.FindAllStringSubmatch(body, -1)

	for _, match := range matches {
		hrefMatch := hrefAttrPattern.FindStringSubmatch(match[1])
		if len(hrefMatch) < 2 {
			continue
		}

		href := strings.TrimSpace(hrefMatch[1])
		href = strings.Trim(href, `"'`)
		href = htmlstd.UnescapeString(href)
		if href == "" || strings.HasPrefix(strings.ToLower(href), "javascript:") || strings.HasPrefix(strings.ToLower(href), "mailto:") {
			continue
		}

		candidate, err := url.Parse(href)
		if err != nil {
			continue
		}
		absolute := base.ResolveReference(candidate)
		absolute.Fragment = ""
		if absolute.Scheme != "http" && absolute.Scheme != "https" {
			continue
		}
		if absolute.User != nil || validatePort(absolute) != nil {
			continue
		}
		if !isSafeLinkTarget(absolute) {
			continue
		}
		link := absolute.String()
		if _, ok := seen[link]; ok {
			continue
		}

		seen[link] = struct{}{}
		links = append(links, pageLink{Text: limitString(extractText(match[2], "text/html"), 200), URL: link})
		if len(links) >= limit {
			break
		}
	}

	return links
}

func extractSearchResults(baseURL string, body string, limit int) []searchResult {
	base, err := url.Parse(baseURL)
	if err != nil {
		return []searchResult{}
	}

	seen := map[string]struct{}{}
	results := make([]searchResult, 0, limit)
	matches := anchorPattern.FindAllStringSubmatch(body, -1)

	for _, match := range matches {
		hrefMatch := hrefAttrPattern.FindStringSubmatch(match[1])
		if len(hrefMatch) < 2 {
			continue
		}

		target := searchResultURL(base, hrefMatch[1])
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}

		title := limitString(extractText(match[2], "text/html"), 300)
		if title == "" {
			continue
		}

		seen[target] = struct{}{}
		results = append(results, searchResult{Title: title, URL: target})
		if len(results) >= limit {
			break
		}
	}

	return results
}

func searchResultURL(base *url.URL, rawHref string) string {
	href := strings.TrimSpace(rawHref)
	href = strings.Trim(href, `"'`)
	href = htmlstd.UnescapeString(href)
	if href == "" {
		return ""
	}

	candidate, err := url.Parse(href)
	if err != nil {
		return ""
	}
	absolute := base.ResolveReference(candidate)
	absolute.Fragment = ""

	if isDuckDuckGoHost(absolute.Hostname()) {
		redirectTarget := absolute.Query().Get("uddg")
		if redirectTarget == "" {
			return ""
		}
		absolute, err = url.Parse(redirectTarget)
		if err != nil {
			return ""
		}
		absolute.Fragment = ""
	}

	if absolute.Scheme != "http" && absolute.Scheme != "https" {
		return ""
	}
	if absolute.User != nil || validatePort(absolute) != nil || !isSafeLinkTarget(absolute) {
		return ""
	}
	return absolute.String()
}

func isDuckDuckGoHost(host string) bool {
	host = strings.ToLower(host)
	return host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com")
}

func isSafeLinkTarget(link *url.URL) bool {
	host := link.Hostname()
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return validatePublicIP(addr) == nil
	}
	return host != ""
}

func limitString(text string, maxChars int) string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars])
}

func mustPrefixes(raw []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			panic(err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}
