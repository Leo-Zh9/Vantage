package analytics

import (
	"net/url"
	"strings"
)

// Class is a coarse guess at who sent a request.
type Class uint8

const (
	// Human is a request from a regular browser.
	Human Class = iota
	// Bot is a self-identified crawler, preview fetcher, monitor or HTTP library.
	Bot
	// Scanner is a request for a path that only vulnerability scanners ask for
	// (WordPress logins, .env files, .git directories) on a site that has none.
	Scanner
)

func (c Class) String() string {
	switch c {
	case Bot:
		return "bot"
	case Scanner:
		return "scanner"
	default:
		return "human"
	}
}

// probeMarkers are path fragments that real visitors to a static or Next.js
// site never request. Matching is on the lowercased path.
var probeMarkers = []string{
	".php", "/wp-", "/wordpress", "xmlrpc", "/.env", "/.git", "/.aws", "/.ssh",
	"/.ds_store", "/.vscode", "/.idea", "/phpmyadmin", "/pma", "/cgi-bin", "/admin.",
	"/server-status", "/actuator", "/vendor/", "/boaform", "/hnap1", "/owa/",
	"/autodiscover", "/config.json", "/config.yml", "/backup", ".sql", ".bak",
	"/shell", "/druid", "/solr", "/jenkins", "/manager/html", "/telescope", "/_profiler",
}

// botMarkers are User-Agent fragments used by crawlers, link-preview fetchers,
// uptime monitors and HTTP libraries. Matching is on the lowercased User-Agent.
var botMarkers = []string{
	"bot", "crawl", "spider", "slurp", "curl/", "wget", "python-", "python/",
	"go-http-client", "java/", "okhttp", "httpclient", "libwww", "headless",
	"phantomjs", "scrapy", "facebookexternalhit", "embedly", "preview", "monitor",
	"uptime", "pingdom", "lighthouse", "axios", "node-fetch", "undici", "postman",
	"masscan", "zgrab", "nmap", "nuclei", "censys", "expanse", "scan", "fetch",
	"http.rb", "guzzle", "aiohttp", "httpx", "whatsapp", "telegram", "discord",
	"slack", "skype", "vercel-screenshot", "chrome-lighthouse",
}

// classify guesses the sender of a request from its User-Agent and path. It is
// a heuristic: a determined bot can claim to be a browser, but most of the
// background noise on a personal site identifies itself honestly.
func classify(userAgent, path string) Class {
	p := strings.ToLower(path)
	for _, m := range probeMarkers {
		if strings.Contains(p, m) {
			return Scanner
		}
	}
	if userAgent == "" {
		return Bot
	}
	ua := strings.ToLower(userAgent)
	for _, m := range botMarkers {
		if strings.Contains(ua, m) {
			return Bot
		}
	}
	return Human
}

// assetExts are file types a browser fetches as part of a page, not as a page.
// PDFs are deliberately absent: opening a resume PDF is a page view.
var assetExts = []string{
	".js", ".mjs", ".css", ".map", ".png", ".jpg", ".jpeg", ".gif", ".svg",
	".ico", ".webp", ".avif", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".txt",
	".xml", ".json", ".webmanifest", ".mp4", ".webm", ".mp3",
}

// isAsset reports whether path is a sub-resource (script, image, font) rather
// than a page someone navigated to.
func isAsset(path string) bool {
	if strings.HasPrefix(path, "/_next/") || strings.HasPrefix(path, "/_vercel/") ||
		strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/assets/") {
		return true
	}
	p := strings.ToLower(path)
	for _, ext := range assetExts {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// referrerHost returns the bare host of an external referrer ("linkedin.com"),
// or "" when there is none or the visitor came from the site itself.
func referrerHost(referer, siteHost string) string {
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil {
		return ""
	}
	h := bareHost(u.Hostname())
	if h == "" || h == bareHost(siteHost) {
		return ""
	}
	return h
}

// bareHost lowercases a host and strips any port and leading "www.".
func bareHost(h string) string {
	h = strings.ToLower(h)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimPrefix(h, "www.")
}
