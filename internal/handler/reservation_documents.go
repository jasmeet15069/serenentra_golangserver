package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hotelharmony/api/pkg/response"
)

// Guest identity documents captured at the desk.
//
// Metadata lives in reservation_documents; the bytes live on the uploads volume
// mounted at /app/uploads. The volume is not optional and ships in the same
// commit as this file: without it the files sit in the container's writable
// layer and every deploy deletes them, exactly as it once deleted every
// platform backup — except a lost passport scan is a compliance record that
// cannot be recreated by simply taking another, while the database goes on
// claiming the file is there.
//
// These are among the most sensitive rows the system holds, so:
//
//   - nothing is ever served from a static path. Every read goes through an
//     authenticated, role-checked, tenant-scoped handler that streams the file.
//   - the stored name is a UUID, never the client's filename. A caller-supplied
//     name is a path-traversal vector and also leaks the guest's name to anyone
//     who can list the directory.
//   - the type is decided by the leading bytes, not by the Content-Type header
//     or the extension, both of which the client chooses freely.

const (
	// 5 MB. A phone photo of a passport is well under this; anything larger is
	// a scan nobody needs at full resolution or an upload that should fail.
	maxDocumentBytes = 5 << 20

	uploadRootEnv     = "UPLOAD_DIR"
	defaultUploadRoot = "/app/uploads"
)

// documentTypes are the ID proofs the front-office form offers.
var documentTypes = map[string]bool{
	"passport": true, "driver_license": true, "national_id": true, "voter_id": true,
}

// sniffMIME identifies a file from its leading bytes.
//
// Deliberately not http.DetectContentType, which falls back to
// "text/plain" or "application/octet-stream" for anything it does not know and
// would let an executable through as an unremarkable blob. Only the three
// formats the spec allows are accepted, and everything else is refused.
func sniffMIME(b []byte) (string, bool) {
	switch {
	case len(b) >= 3 && bytes.Equal(b[:3], []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg", true
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png", true
	case len(b) >= 5 && bytes.Equal(b[:5], []byte("%PDF-")):
		return "application/pdf", true
	}
	return "", false
}

func uploadRoot() string {
	if v := strings.TrimSpace(os.Getenv(uploadRootEnv)); v != "" {
		return v
	}
	return defaultUploadRoot
}

// documentPath is where a document's bytes live.
//
// Sharded by tenant so one hotel's documents are never interleaved with
// another's, which makes both an export and a deletion request tractable. The
// filename is the document's own UUID — the client's filename never reaches the
// filesystem.
func documentPath(hotelID, docID uuid.UUID, mime string) string {
	ext := ".bin"
	switch mime {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "application/pdf":
		ext = ".pdf"
	}
	return filepath.Join(uploadRoot(), hotelID.String(), docID.String()+ext)
}

func (h *ReservationHandler) registerDocumentRoutes(r fiber.Router) {
	r.Post("/reservations/:id/documents", h.UploadDocument)
	r.Get("/reservations/:id/documents", h.ListDocuments)
	r.Get("/reservations/:id/documents/:docID/content", h.DownloadDocument)
	r.Delete("/reservations/:id/documents/:docID", h.DeleteDocument)
}

// UploadDocument stores an ID proof against a reservation.
func (h *ReservationHandler) UploadDocument(c *fiber.Ctx) error {
	if !h.requireFrontDesk(c) {
		return nil
	}
	stayID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid reservation id")
	}
	hotelID := tenantHotelID(c)

	// Scoped to the tenant, so a document cannot be attached to another hotel's
	// reservation by guessing an id.
	stay, err := h.roomRepo.FindStayByID(c.Context(), hotelID, stayID)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "reservation not found")
	}

	docType := strings.ToLower(strings.TrimSpace(c.FormValue("doc_type")))
	if !documentTypes[docType] {
		return response.Error(c, fiber.StatusUnprocessableEntity,
			"doc_type must be one of passport, driver_license, national_id, voter_id")
	}
	docNumber := strings.TrimSpace(c.FormValue("doc_number"))

	fh, err := c.FormFile("file")
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "a file field is required")
	}
	if fh.Size > maxDocumentBytes {
		return response.Error(c, fiber.StatusRequestEntityTooLarge,
			fmt.Sprintf("file is %d bytes, limit is %d", fh.Size, maxDocumentBytes))
	}
	src, err := fh.Open()
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "could not read the uploaded file")
	}
	defer src.Close()

	// LimitReader as well as the Size check: Size comes from the multipart
	// header, which the client writes, so it is a hint and not a guarantee.
	data, err := io.ReadAll(io.LimitReader(src, maxDocumentBytes+1))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "could not read the uploaded file")
	}
	if len(data) > maxDocumentBytes {
		return response.Error(c, fiber.StatusRequestEntityTooLarge, "file exceeds the 5 MB limit")
	}
	if len(data) == 0 {
		return response.Error(c, fiber.StatusUnprocessableEntity, "the uploaded file is empty")
	}

	mime, ok := sniffMIME(data)
	if !ok {
		// The extension and Content-Type are both chosen by the caller, so a
		// .pdf whose bytes are an executable is refused here rather than stored
		// and served back later.
		return response.Error(c, fiber.StatusUnprocessableEntity,
			"only JPEG, PNG and PDF are accepted (checked by file content, not extension)")
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	docID := uuid.New()
	path := documentPath(hotelID, docID, mime)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "could not prepare storage")
	}
	// 0600: the file is readable only by the API process. Nothing else on the
	// box has any business reading a guest's passport.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "could not store the document")
	}

	// Metadata after the bytes: a row pointing at a file that was never written
	// is a download that fails, which is the failure mode the backup work had to
	// go back and fix. The reverse — a file with no row — is only wasted disk.
	if _, err := h.db.Querier(c.Context()).Exec(c.Context(), `
		INSERT INTO reservation_documents
			(id, hotel_id, guest_stay_id, guest_id, doc_type, doc_number,
			 file_path, mime_type, size_bytes, sha256, uploaded_by, created_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11,now())`,
		docID, hotelID, stayID, stay.GuestID, docType, docNumber,
		path, mime, len(data), digest, actorUserID(c),
	); err != nil {
		_ = os.Remove(path)
		return response.Error(c, fiber.StatusInternalServerError, "could not record the document")
	}

	// Mirror onto the CRM guest so a returning guest's ID is on their profile,
	// not only on the stay they happened to present it for. Best-effort: the
	// document is already stored and is the record that matters.
	if gid, gErr := h.roomRepo.EnsureGuest(c.Context(), hotelID,
		stay.GuestName, derefStr(stay.GuestEmail), derefStr(stay.GuestPhone)); gErr == nil && gid != uuid.Nil {
		_ = h.roomRepo.SetGuestIdentity(c.Context(), hotelID, gid, docType, docNumber)
	}

	return response.Created(c, fiber.Map{
		"id": docID, "doc_type": docType, "mime_type": mime,
		"size_bytes": len(data), "sha256": digest,
	})
}

type documentMeta struct {
	ID        uuid.UUID `json:"id"`
	DocType   string    `json:"doc_type"`
	DocNumber *string   `json:"doc_number"`
	MimeType  string    `json:"mime_type"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	// Whether the bytes are actually still on disk. The backup module learned
	// the hard way that a row is not evidence of a file, so this is stat-ed
	// rather than assumed.
	Available bool `json:"artifact_available"`
}

// ListDocuments returns metadata only — never the bytes.
func (h *ReservationHandler) ListDocuments(c *fiber.Ctx) error {
	if !h.requireFrontDesk(c) {
		return nil
	}
	stayID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid reservation id")
	}
	hotelID := tenantHotelID(c)

	rows, err := h.db.Querier(c.Context()).Query(c.Context(), `
		SELECT id, doc_type, doc_number, mime_type, size_bytes, sha256, file_path, created_at
		  FROM reservation_documents
		 WHERE hotel_id = $1 AND guest_stay_id = $2
		 ORDER BY created_at DESC`, hotelID, stayID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list documents")
	}
	defer rows.Close()

	out := make([]documentMeta, 0)
	for rows.Next() {
		var m documentMeta
		var path string
		if err := rows.Scan(&m.ID, &m.DocType, &m.DocNumber, &m.MimeType,
			&m.SizeBytes, &m.SHA256, &path, &m.CreatedAt); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to read documents")
		}
		_, statErr := os.Stat(path)
		m.Available = statErr == nil
		out = append(out, m)
	}
	return response.OK(c, out)
}

// DownloadDocument streams the bytes to an authorised member of staff.
func (h *ReservationHandler) DownloadDocument(c *fiber.Ctx) error {
	if !h.requireFrontDesk(c) {
		return nil
	}
	stayID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid reservation id")
	}
	docID, err := uuid.Parse(c.Params("docID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid document id")
	}
	hotelID := tenantHotelID(c)

	var path, mime string
	// hotel_id AND guest_stay_id in the predicate: the document must belong to
	// this tenant *and* to the reservation in the URL, so neither id alone is
	// enough to reach someone else's passport.
	err = h.db.Querier(c.Context()).QueryRow(c.Context(), `
		SELECT file_path, mime_type FROM reservation_documents
		 WHERE id = $1 AND hotel_id = $2 AND guest_stay_id = $3`,
		docID, hotelID, stayID).Scan(&path, &mime)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return response.Error(c, fiber.StatusNotFound, "document not found")
		}
		return response.Error(c, fiber.StatusInternalServerError, "failed to read the document")
	}

	if _, statErr := os.Stat(path); statErr != nil {
		// The row survived but the file did not — the exact state the uploads
		// volume exists to prevent. Say so rather than serving a 500.
		return response.Error(c, fiber.StatusGone, "the stored file is no longer present")
	}

	c.Set("Content-Type", mime)
	// inline would let a PDF render in the browser tab and be trivially shared
	// as a URL; attachment keeps it a deliberate download.
	c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", docID.String()))
	c.Set("Cache-Control", "no-store")
	return c.SendFile(path)
}

// DeleteDocument removes a document, for a wrong upload or an erasure request.
//
// Manager-level: deleting an identity record is not a routine desk action, and
// it is the one operation here that cannot be undone.
func (h *ReservationHandler) DeleteDocument(c *fiber.Ctx) error {
	if !h.requireFrontDeskManager(c) {
		return nil
	}
	stayID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid reservation id")
	}
	docID, err := uuid.Parse(c.Params("docID"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid document id")
	}
	hotelID := tenantHotelID(c)

	var path string
	err = h.db.Querier(c.Context()).QueryRow(c.Context(), `
		DELETE FROM reservation_documents
		 WHERE id = $1 AND hotel_id = $2 AND guest_stay_id = $3
		 RETURNING file_path`, docID, hotelID, stayID).Scan(&path)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return response.Error(c, fiber.StatusNotFound, "document not found")
		}
		return response.Error(c, fiber.StatusInternalServerError, "delete failed")
	}
	// Row first, then the bytes. If the unlink fails the record is already gone,
	// which is the safer direction for an erasure request — an orphaned file is
	// wasted disk, an orphaned row is a document the system still offers.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return response.OK(c, fiber.Map{
			"status": "deleted", "warning": "record removed but the file could not be unlinked",
		})
	}
	return response.OK(c, fiber.Map{"status": "deleted"})
}
