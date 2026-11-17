package board

import (
	"bytes"
	"path/filepath"
	"unicode/utf8"
)

// MaxBrochureSize is the largest brochure accepted, in bytes. A file of
// exactly this size is allowed; one byte more is not.
const MaxBrochureSize = 1 << 20

const (
	ContentTypePDF  = "application/pdf"
	ContentTypeText = "text/plain; charset=utf-8"
)

var pdfMagic = []byte("%PDF-")

// ClassifyBrochure decides a brochure's type from its bytes alone, never from
// the client's Content-Type or the file extension. It returns the content
// type to serve it with, or ok=false when the bytes are neither:
//
//   - PDF: the bytes start with %PDF-.
//   - Text: the whole file is valid UTF-8, has no NUL byte and does not
//     start with %PDF-.
//
// An empty file is neither.
func ClassifyBrochure(data []byte) (contentType string, ok bool) {
	switch {
	case len(data) == 0:
		return "", false
	case bytes.HasPrefix(data, pdfMagic):
		return ContentTypePDF, true
	case utf8.Valid(data) && bytes.IndexByte(data, 0) < 0:
		return ContentTypeText, true
	default:
		return "", false
	}
}

// BrochurePath is where a sheet's brochure is stored. It is built only from
// ids the server generated, never from the client's filename, so no upload
// can write outside its sheet's directory.
func BrochurePath(dataDir, userID, jobID string) string {
	return filepath.Join(dataDir, userID, jobID, "body")
}
