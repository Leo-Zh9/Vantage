package analytics

import "testing"

const chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func TestClassify(t *testing.T) {
	cases := []struct {
		ua, path string
		want     Class
	}{
		{chromeUA, "/", Human},
		{chromeUA, "/projects/vantage", Human},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1", "/", Human},
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "/", Bot},
		{"LinkedInBot/1.0 (compatible; Mozilla/5.0; Apache-HttpClient +http://www.linkedin.com)", "/", Bot},
		{"curl/8.7.1", "/", Bot},
		{"python-requests/2.32.3", "/", Bot},
		{"", "/", Bot},
		{chromeUA, "/wp-login.php", Scanner}, // the path gives it away, whatever the UA says
		{"Mozilla/5.0 zgrab/0.x", "/.env", Scanner},
		{"", "/.git/config", Scanner},
		{chromeUA, "/xmlrpc.php", Scanner},
	}
	for _, c := range cases {
		if got := classify(c.ua, c.path); got != c.want {
			t.Errorf("classify(%q, %q) = %v, want %v", c.ua, c.path, got, c.want)
		}
	}
}

func TestIsAsset(t *testing.T) {
	assets := []string{"/_next/static/chunks/main.js", "/favicon.ico", "/og.png", "/styles.css", "/robots.txt", "/fonts/inter.woff2"}
	pages := []string{"/", "/about", "/projects/vantage", "/resume.pdf"}
	for _, p := range assets {
		if !isAsset(p) {
			t.Errorf("isAsset(%q) = false, want true", p)
		}
	}
	for _, p := range pages {
		if isAsset(p) {
			t.Errorf("isAsset(%q) = true, want false", p)
		}
	}
}

func TestReferrerHost(t *testing.T) {
	cases := []struct{ ref, site, want string }{
		{"https://www.linkedin.com/in/someone/", "www.leo-zhang.com", "linkedin.com"},
		{"https://github.com/Leo-Zh9", "www.leo-zhang.com", "github.com"},
		{"https://www.leo-zhang.com/about", "www.leo-zhang.com", ""}, // internal navigation
		{"https://leo-zhang.com/", "www.leo-zhang.com:443", ""},      // apex vs www, with port
		{"", "www.leo-zhang.com", ""},
		{"::not a url", "www.leo-zhang.com", ""},
	}
	for _, c := range cases {
		if got := referrerHost(c.ref, c.site); got != c.want {
			t.Errorf("referrerHost(%q, %q) = %q, want %q", c.ref, c.site, got, c.want)
		}
	}
}
