package desktop

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// buildLaunchAgentPlist renders the macOS LaunchAgent plist for exe
// (Label com.musicops.desktop, RunAtLoad true, ProgramArguments [exe]).
func buildLaunchAgentPlist(exe string) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("\t<key>Label</key><string>com.musicops.desktop</string>\n")
	b.WriteString("\t<key>RunAtLoad</key><true/>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>")
	_ = xml.EscapeText(&b, []byte(exe))
	b.WriteString("</string>\n\t</array>\n</dict>\n</plist>\n")
	return b.String()
}

// parseLaunchAgentPlist extracts ProgramArguments[0] and RunAtLoad from a
// plist. It walks the XML tokens so key order and whitespace do not matter.
func parseLaunchAgentPlist(data []byte) (exe string, runAtLoad bool, err error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var currentKey string
	var inArray bool
	var arrayStrings []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return "", false, err
				}
				currentKey = text
			case "true":
				if currentKey == "RunAtLoad" {
					runAtLoad = true
				}
			case "false":
				if currentKey == "RunAtLoad" {
					runAtLoad = false
				}
			case "array":
				inArray = true
				arrayStrings = nil
			case "string":
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return "", false, err
				}
				if inArray {
					arrayStrings = append(arrayStrings, text)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				inArray = false
				if currentKey == "ProgramArguments" && len(arrayStrings) > 0 {
					exe = arrayStrings[0]
				}
			}
		}
	}
	if exe == "" && !runAtLoad {
		return "", false, errors.New("not a MusicOps launch agent plist")
	}
	return exe, runAtLoad, nil
}
