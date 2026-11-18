package router

import (
	"testing"
)

func TestSTPlayAllowedHost(t *testing.T) {
	cases := map[string]bool{
		"tapecontent.net":               true,
		"861134100.tapecontent.net":     true,
		"foo.tapecontent.net":           true,
		"api.streamtape.com":            false,
		"streamtape.com":                false,
		"tapecontent.net.evil.com":      false,
		"notapecontent.net":             false,
		"":                              false,
		"861134100.tapecontent.net:443": true,
		"861134100.TAPECONTENT.NET":     true,
	}
	for host, want := range cases {
		if got := stPlayAllowedHost(host); got != want {
			t.Errorf("stPlayAllowedHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestStreamtapeVideoURLRelay(t *testing.T) {
	cases := []struct {
		host, link, want string
	}{
		{"Streamtape", "https://streamtape.com/e/AbCdEf1234/", "/api/play/streamtape/AbCdEf1234"},
		{"Streamtape", "https://streamtape.com/v/AbCdEf1234", "/api/play/streamtape/AbCdEf1234"},
		{"streamtape", "https://streamtape.com/e/AbCdEf1234/", "/api/play/streamtape/AbCdEf1234"},
		{"Streamtape", "", ""},
	}
	for _, cse := range cases {
		if got := videoURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("videoURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
		if got := embedURLForHostLink(cse.host, cse.link); cse.want != "" && got != "" {
			t.Errorf("embedURLForHostLink(%q,%q) = %q, want empty (relay path)", cse.host, cse.link, got)
		}
	}
}

func TestLuluStreamEmbedURL(t *testing.T) {
	cases := []struct {
		host, link, want string
	}{
		{"LuluStream", "https://lulustream.com/AbC123xY.html", "https://lulustream.com/embed/AbC123xY"},
		{"LuluStream", "https://lulustream.com/e/AbC123xY", "https://lulustream.com/embed/AbC123xY"},
		{"LuluStream", "https://lulustream.com/embed/AbC123xY", "https://lulustream.com/embed/AbC123xY"},
		{"LuluStream", "https://luluvdo.com/AbC123xY.html", "https://lulustream.com/embed/AbC123xY"},
		{"lulustream", "https://luluvdo.com/e/AbC123xY", "https://lulustream.com/embed/AbC123xY"},
		{"LuluStream", "", ""},
	}
	for _, cse := range cases {
		if got := embedURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("embedURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
		if got := videoURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("videoURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
	}
}

func TestUpload18EmbedURL(t *testing.T) {
	cases := []struct {
		host, link, want string
	}{
		{"Upload18", "https://upload18.to/play/index/cd90ffd5ba95", "https://upload18.to/play/index/cd90ffd5ba95"},
		{"upload18", "https://upload18.to/play/index/cd90ffd5ba95", "https://upload18.to/play/index/cd90ffd5ba95"},
		{"Upload18", "", ""},
	}
	for _, cse := range cases {
		if got := embedURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("embedURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
		if got := videoURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("videoURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
	}
}

func TestDroploadEmbedURL(t *testing.T) {
	cases := []struct {
		host, link, want string
	}{
		{"Dropload", "https://dropload.co/rz91qlhqpp74", "https://dropload.co/e/rz91qlhqpp74"},
		{"Dropload", "https://dropload.co/e/rz91qlhqpp74", "https://dropload.co/e/rz91qlhqpp74"},
		{"dropload", "https://dr0pstream.com/rz91qlhqpp74", "https://dropload.co/e/rz91qlhqpp74"},
		{"Dropload", "", ""},
	}
	for _, cse := range cases {
		if got := embedURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("embedURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
		if got := videoURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("videoURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
	}
}

func TestFileHostEmbedURL(t *testing.T) {
	cases := []struct {
		host, link, want string
	}{
		{"FileHost", "https://filehost.net/abc1234", "https://filehost.net/e/abc1234"},
		{"FileHost", "https://filehost.net/e/abc1234", "https://filehost.net/e/abc1234"},
		{"filehost", "https://filehost.net/abc1234", "https://filehost.net/e/abc1234"},
		{"FileHost", "", ""},
	}
	for _, cse := range cases {
		if got := embedURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("embedURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
		if got := videoURLForHostLink(cse.host, cse.link); got != cse.want {
			t.Errorf("videoURLForHostLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
	}
}

func TestIsStreamtapeLink(t *testing.T) {
	cases := map[string]bool{
		"https://streamtape.com/e/AbCdEf1234/": true,
		"https://streamtape.com/v/AbCdEf1234":  true,
		"STREAMTAPE.COM/e/AbCdEf1234":          true,
		"https://voe.sx/e/abc":                 false,
		"https://mixdrop.ag/e/abc":             false,
		"https://example.com/abc":              false,
		"":                                     false,
	}
	for u, want := range cases {
		if got := isStreamtapeLink(u); got != want {
			t.Errorf("isStreamtapeLink(%q) = %v, want %v", u, got, want)
		}
	}
}
