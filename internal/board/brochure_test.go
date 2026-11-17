package board

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestClassifyBrochure(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string // "" means rejected
	}{
		{"pdf", []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\nbinary\x00body"), ContentTypePDF},
		{"pdf magic only", []byte("%PDF-"), ContentTypePDF},
		{"ascii text", []byte("10 Market Street\nTwo bedrooms\n"), ContentTypeText},
		{"utf-8 text", []byte("Квартира біля ринку, 2 кімнати"), ContentTypeText},
		{"text that only resembles a pdf", []byte("%PDF without the dash"), ContentTypeText},
		{"empty", nil, ""},
		{"nul byte", []byte("hello\x00world"), ""},
		{"invalid utf-8", []byte("caf\xe9"), ""},
		{"truncated utf-8", []byte("ok \xd0"), ""},
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), ""},
		{"lowercase pdf magic", []byte("%pdf-\x00"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ClassifyBrochure(tc.data)
			if tc.want == "" {
				if ok {
					t.Fatalf("accepted as %q, want rejected", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("got %q ok=%v, want %q", got, ok, tc.want)
			}
		})
	}
}

func TestClassifyBrochureAcceptsTextAtTheSizeLimit(t *testing.T) {
	data := bytes.Repeat([]byte("a"), MaxBrochureSize)
	if got, ok := ClassifyBrochure(data); !ok || got != ContentTypeText {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestBrochurePathIgnoresClientInput(t *testing.T) {
	got := BrochurePath("/data", "u1", "j1")
	if want := filepath.Join("/data", "u1", "j1", "body"); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
