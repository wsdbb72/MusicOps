package app

import _ "embed"

//go:embed assets/musicops.png
var appIconPNG []byte

const appWebManifest = `{
  "name": "MusicOps",
  "short_name": "MusicOps",
  "description": "本地音乐流水线",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#f7f4ef",
  "theme_color": "#146b7a",
  "icons": [
    {
      "src": "/brand-icon.png",
      "sizes": "1024x1024",
      "type": "image/png",
      "purpose": "any maskable"
    }
  ]
}
`
