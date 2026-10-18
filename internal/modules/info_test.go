package modules

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInfo(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		// The distro's shape: Latin-1 declared, a leading blank line, a second
		// paragraph that is credits rather than description.
		"anemone": "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n\n" +
			"<screensaver name=\"anemone\" _label=\"Anemone\">\n" +
			"  <command arg=\"--root\"/>\n" +
			"  <_description>\nWiggling   tentacles,\nswaying \xe9l\xe9gamment.\n\nWritten by Gabriel Finch; 2002.\n  </_description>\n" +
			"</screensaver>\n",
		"nolabel": `<screensaver name="nolabel"><_description>Just this.</_description></screensaver>`,
		"broken":  `<screensaver _label="Broken"`,
		"escaped": `<screensaver _label="A &amp; B"><_description>x &lt; y</_description></screensaver>`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".xml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &Finder{ConfigDir: dir}

	for _, tc := range []struct {
		name    string
		want    Info
		wantErr bool
	}{
		{"anemone", Info{"anemone", "Anemone", "Wiggling tentacles, swaying élégamment."}, false},
		{"nolabel", Info{"nolabel", "nolabel", "Just this."}, false},
		{"escaped", Info{"escaped", "A & B", "x < y"}, false},
		{"broken", Info{"broken", "broken", ""}, true},
		{"absent", Info{"absent", "absent", ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.Info(tc.name)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("Info = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The fixture above imitates the distro's files; this reads a real one when
// the packages are installed, so a format change upstream shows up here.
func TestInfoOnAnInstalledModule(t *testing.T) {
	if _, err := os.Stat(filepath.Join(DefaultConfigDir, "atlantis.xml")); err != nil {
		t.Skip("xscreensaver-gl-extra is not installed")
	}
	got, err := NewFinder().Info("atlantis")
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "Atlantis" || got.Description == "" {
		t.Errorf("Info = %+v", got)
	}
}

func TestInfoRejectsAnUnknownCharset(t *testing.T) {
	dir := t.TempDir()
	body := `<?xml version="1.0" encoding="KOI8-R"?><screensaver _label="X"/>`
	if err := os.WriteFile(filepath.Join(dir, "koi.xml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := (&Finder{ConfigDir: dir}).Info("koi")
	if err == nil {
		t.Error("Info succeeded on an unsupported charset")
	}
	if got != (Info{Name: "koi", Label: "koi"}) {
		t.Errorf("Info = %+v, want the bare-name fallback", got)
	}
}
