package uploader

import "testing"

// TestHostWebsite pins the lookup contract the three surfaces (admin page
// panels, Discord alert, /api/upload_keys JSON) all rely on: case-insensitive
// matching on the canonical host names, empty string for unknown hosts so
// callers render no link rather than a broken one.
func TestHostWebsite(t *testing.T) {
	tcs := []struct {
		name string
		in   string
		want string
	}{
		{"exact canonical", "Upload18", "https://upload18.org"},
		{"different case", "upload18", "https://upload18.org"},
		{"video host", "VoeSX", "https://voe.sx"},
		{"image host", "ImgBB", "https://imgbb.com"},
		{"host not in map", "SomeNewHost", ""},
		{"empty", "", ""},
	}
	for _, tc := range tcs {
		if got := HostWebsite(tc.in); got != tc.want {
			t.Errorf("HostWebsite(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Every managed uploadkeys host must resolve — the admin panel is built
	// from that list, and a missing entry there would render a dead row.
	for _, host := range []string{
		"VoeSX", "Streamtape", "Mixdrop", "Vidara", "LuluStream", "DoodStream",
		"Upload18", "Dropload", "Videro", "FileHost",
		"Catbox", "ImgBB", "FreeImage", "ImgPile", "ImgChest",
	} {
		if HostWebsite(host) == "" {
			t.Errorf("managed host %q has no website mapping", host)
		}
	}
}
