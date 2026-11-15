package uploader

import "strings"

// hostWebsites maps every upload host this fleet knows about to the site you
// sign up on / grab a new API key from.  It feeds three surfaces at once:
//
//   - the admin page's Upload Keys panel and Upload Host Backoffs panel
//     (each host row links to its site),
//   - the Discord "🔑 upload host needs a new key" alert (direct link instead
//     of googling the host),
//   - GET /api/upload_keys (the Website field in the JSON payload).
//
// Keys are the uploader's canonical host names ("VoeSX", "Upload18", ...).
// Lookup is case-insensitive so template/API callers can pass whatever casing
// they have.  Unknown hosts return "" and callers render no link.
var hostWebsites = map[string]string{
	// video hosts
	"VoeSX":      "https://voe.sx",
	"Streamtape": "https://streamtape.com",
	"Mixdrop":    "https://mixdrop.co",
	"Vidara":     "https://vidara.so",
	"LuluStream": "https://lulustream.com",
	"DoodStream": "https://doodstream.com",
	"Upload18":   "https://upload18.org",
	"Dropload":   "https://dropload.co",
	"Videro":     "https://videro.my",
	"FileHost":   "https://filehost.net",
	"GoFile":     "https://gofile.io",
	"AnonMP4":    "https://anonmp4.to",
	// image hosts
	"Catbox":    "https://catbox.moe",
	"ImgBB":     "https://imgbb.com",
	"FreeImage": "https://freeimage.host",
	"ImgPile":   "https://imgpile.com",
	"ImgChest":  "https://imgchest.com",
}

// HostWebsite returns the sign-up/dashboard URL for an upload host, or ""
// when the name is unknown.  Matching is case-insensitive.
func HostWebsite(name string) string {
	if name == "" {
		return ""
	}
	for k, v := range hostWebsites {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
