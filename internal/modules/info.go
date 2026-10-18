package modules

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Info describes a module for a person choosing between them.
type Info struct {
	// Name is the executable's name, the one INCLUDE and EXCLUDE use.
	Name string `json:"name"`
	// Label is the display name from the module's config XML, or Name when
	// the XML has none.
	Label string `json:"label"`
	// Description is the first paragraph of the XML's description, the
	// summary XScreenSaver's own settings dialog leads with. It may be empty.
	Description string `json:"description"`
}

// Info reads a module's label and description from ConfigDir/<name>.xml.
//
// A missing or malformed file is not an error worth failing a whole listing
// over: the module still runs, so Info falls back to the bare name and
// returns the error alongside it for a caller that wants to log it.
func (f *Finder) Info(name string) (Info, error) {
	info := Info{Name: name, Label: name}
	path := filepath.Join(f.ConfigDir, name+".xml")
	file, err := os.Open(path)
	if err != nil {
		return info, fmt.Errorf("reading %s: %w", path, err)
	}
	defer file.Close()

	var doc struct {
		Label       string   `xml:"_label,attr"`
		Description []string `xml:"_description"`
	}
	dec := xml.NewDecoder(bufio.NewReader(file))
	// The distro's files declare ISO-8859-1, which encoding/xml refuses
	// without a reader for it.
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&doc); err != nil {
		return info, fmt.Errorf("parsing %s: %w", path, err)
	}
	if l := strings.TrimSpace(doc.Label); l != "" {
		info.Label = l
	}
	if len(doc.Description) > 0 {
		info.Description = firstParagraph(doc.Description[0])
	}
	return info, nil
}

// firstParagraph joins the lines before the first blank line, skipping blank
// lines before any text, and collapses runs of whitespace.
func firstParagraph(s string) string {
	var words []string
	for line := range strings.SplitSeq(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			if len(words) > 0 {
				break
			}
			continue
		}
		words = append(words, fields...)
	}
	return strings.Join(words, " ")
}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "utf-8", "us-ascii":
		return input, nil
	case "iso-8859-1", "latin1", "latin-1":
		// Each Latin-1 byte is the code point of the same value. The files
		// are a few kilobytes, so transcoding them whole is simplest.
		raw, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		b.Grow(len(raw))
		for _, c := range raw {
			b.WriteRune(rune(c))
		}
		return strings.NewReader(b.String()), nil
	}
	return nil, fmt.Errorf("unsupported charset %q", charset)
}
