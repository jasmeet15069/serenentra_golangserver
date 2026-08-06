package handler

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The extension and the Content-Type header are both chosen by whoever is
// uploading, so the leading bytes are the only trustworthy signal. A .pdf whose
// contents are an executable has to be refused at upload, not stored and handed
// back to a member of staff later.
func TestSniffMIME(t *testing.T) {
	cases := []struct {
		name     string
		bytes    []byte
		wantMIME string
		wantOK   bool
	}{
		{"JPEG", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}, "image/jpeg", true},
		{"PNG", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00}, "image/png", true},
		{"PDF", []byte("%PDF-1.7\n%âãÏÓ"), "application/pdf", true},

		{"a Windows executable", []byte{'M', 'Z', 0x90, 0x00}, "", false},
		{"an ELF binary", []byte{0x7F, 'E', 'L', 'F', 0x02}, "", false},
		{"a shell script", []byte("#!/bin/sh\nrm -rf /\n"), "", false},
		{"HTML, which could carry script", []byte("<html><script>alert(1)</script>"), "", false},
		{"SVG, which is XML and can carry script", []byte(`<svg xmlns="http://www.w3.org/2000/svg">`), "", false},
		{"a ZIP, which an Office file also looks like", []byte{'P', 'K', 0x03, 0x04}, "", false},
		{"GIF is an image but not an accepted one", []byte("GIF89a"), "", false},
		{"plain text", []byte("just some text"), "", false},
		{"empty", []byte{}, "", false},

		{"a truncated PNG header", []byte{0x89, 'P', 'N'}, "", false},
		{"a truncated JPEG header", []byte{0xFF, 0xD8}, "", false},
		{"the PDF marker not at the start", []byte("  %PDF-1.7"), "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mime, ok := sniffMIME(c.bytes)
			if ok != c.wantOK {
				t.Fatalf("accepted = %v, want %v (mime %q)", ok, c.wantOK, mime)
			}
			if mime != c.wantMIME {
				t.Errorf("mime = %q, want %q", mime, c.wantMIME)
			}
		})
	}
}

// The stored filename is the document's own UUID. A client-supplied name is
// both a path-traversal vector and a leak — anyone who can list the directory
// would otherwise learn the guest's name from the filename.
func TestDocumentPathIgnoresClientInput(t *testing.T) {
	hotelID := uuid.MustParse("4212e55d-7415-41be-b763-7bd4c4cb0a85")
	docID := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	for _, tc := range []struct {
		mime    string
		wantExt string
	}{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"application/pdf", ".pdf"},
		{"application/octet-stream", ".bin"},
	} {
		got := documentPath(hotelID, docID, tc.mime)

		if filepath.Ext(got) != tc.wantExt {
			t.Errorf("%s: extension = %q, want %q", tc.mime, filepath.Ext(got), tc.wantExt)
		}
		if !strings.Contains(got, docID.String()) {
			t.Errorf("%s: path %q does not use the document uuid as its name", tc.mime, got)
		}
		// Sharded per tenant, so one hotel's documents are never interleaved
		// with another's — which is what makes an export or an erasure request
		// tractable.
		if !strings.Contains(filepath.ToSlash(got), hotelID.String()+"/") {
			t.Errorf("%s: path %q is not scoped to the tenant directory", tc.mime, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("%s: path %q contains a traversal segment", tc.mime, got)
		}
	}
}

// Every ID proof the front-office form offers must be storable, and nothing
// else. A value outside this set reaching the column means the form and the
// backend have drifted apart.
func TestDocumentTypes(t *testing.T) {
	for _, want := range []string{"passport", "driver_license", "national_id", "voter_id"} {
		if !documentTypes[want] {
			t.Errorf("%q is offered by the form but rejected by the API", want)
		}
	}
	for _, reject := range []string{"", "PASSPORT", "aadhaar", "selfie", "credit_card"} {
		if documentTypes[reject] {
			t.Errorf("%q should not be an accepted document type", reject)
		}
	}
}
