package upload

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	keelid "github.com/nrynss/keel/id"
	"github.com/nrynss/keel/mediastore"
)

// Mount registers the handler on mux under its base path. The collection
// path and its subtree are both registered, so a request the handler does
// not serve is answered here rather than by a neighbouring route that
// happens to share the prefix.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle(h.base, h)
	mux.Handle(h.base+"/", h)
}

// ServeHTTP serves the upload protocol:
//
//	POST {base}                  open an upload
//	PUT  {base}/{id}/chunks/{n}  store chunk n
//	GET  {base}/{id}             report the upload's state
//	POST {base}/{id}/complete    assemble, check and store the upload
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel, ok := h.relative(r.URL.Path)
	if !ok {
		h.fail(w, noSuchUpload())
		return
	}
	if rel == "" {
		if !h.methodAllowed(w, r, http.MethodPost) {
			return
		}
		h.begin(w, r)
		return
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if !keelid.Valid(parts[0]) {
		h.fail(w, noSuchUpload())
		return
	}
	switch {
	case len(parts) == 1:
		if !h.methodAllowed(w, r, http.MethodGet) {
			return
		}
		h.state(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "complete":
		if !h.methodAllowed(w, r, http.MethodPost) {
			return
		}
		h.complete(w, r, parts[0])
	case len(parts) == 3 && parts[1] == "chunks":
		if !h.methodAllowed(w, r, http.MethodPut) {
			return
		}
		h.putChunk(w, r, parts[0], parts[2])
	default:
		h.fail(w, noSuchUpload())
	}
}

// relative strips the handler's base path, and reports false when the
// request belongs to a route outside the handler.
func (h *Handler) relative(path string) (string, bool) {
	if path == h.base {
		return "", true
	}
	if !strings.HasPrefix(path, h.base+"/") {
		return "", false
	}
	return strings.TrimPrefix(path, h.base), true
}

// methodAllowed reports whether the request carries the method a route
// allows, answering 405 and naming the allowed method when it does not.
func (h *Handler) methodAllowed(w http.ResponseWriter, r *http.Request, allow string) bool {
	if r.Method == allow {
		return true
	}
	w.Header().Set("Allow", allow)
	h.fail(w, &apiError{
		status:  http.StatusMethodNotAllowed,
		code:    codeInvalidRequest,
		message: "The method is not allowed for this route.",
	})
	return false
}

// begin opens an upload and answers with its state.
func (h *Handler) begin(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	owner := strings.TrimSpace(req.Owner)
	if owner == "" || len(owner) > maxNameBytes {
		h.fail(w, badRequest("owner", "must be a non-empty name of at most 256 bytes"))
		return
	}
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" || len(contentType) > maxNameBytes {
		h.fail(w, badRequest("content_type", "must be a non-empty media type of at most 256 bytes"))
		return
	}
	if len(req.Group) > maxNameBytes {
		h.fail(w, badRequest("group", "must be at most 256 bytes"))
		return
	}
	visibility, err := parseVisibility(req.Visibility)
	if err != nil {
		h.fail(w, err)
		return
	}
	chunkSize := req.ChunkSize
	if chunkSize == 0 {
		chunkSize = defaultChunkSize
	}
	if chunkSize < 0 || chunkSize > h.maxUp {
		h.fail(w, badRequest("chunk_size", "must be positive and no larger than the upload byte limit"))
		return
	}
	var size int64
	sizeKnown := req.SizeBytes != nil
	if sizeKnown {
		size = *req.SizeBytes
		if size < 0 {
			h.fail(w, badRequest("size_bytes", "must not be negative"))
			return
		}
		if size > h.maxUp {
			h.fail(w, overLimit(limitUpload, h.maxUp, size))
			return
		}
	}
	// Bound the chunk files one upload can make the handler account for,
	// which for an undeclared size is what the byte limit allows.
	projected := size
	if !sizeKnown {
		projected = h.maxUp
	}
	if int((projected+chunkSize-1)/chunkSize) > h.maxChunk {
		h.fail(w, badRequest("chunk_size", "would need more chunks than the chunk limit allows"))
		return
	}
	id, err := h.newID()
	if err != nil {
		h.fail(w, fmt.Errorf("upload: generate id: %w", err))
		return
	}
	if err := h.root.Mkdir(id, 0o700); err != nil {
		h.fail(w, fmt.Errorf("upload: create upload directory: %w", err))
		return
	}
	now := h.now()
	up := &upload{
		id:          id,
		owner:       owner,
		group:       req.Group,
		contentType: contentType,
		visibility:  visibility,
		chunkSize:   chunkSize,
		sizeBytes:   size,
		sizeKnown:   sizeKnown,
		storedBytes: 0,
		highest:     -1,
		expiresAt:   now.Add(h.ttl),
		chunks:      make(map[int64]chunk),
	}
	if sizeKnown {
		up.chunkCount = int((size + chunkSize - 1) / chunkSize)
	}
	h.mu.Lock()
	if sizeKnown && h.ownerBytesLocked(owner)+size > h.maxOwner {
		held := h.ownerBytesLocked(owner) + size
		h.mu.Unlock()
		h.discardDir(id)
		h.fail(w, overLimit(limitOwner, h.maxOwner, held))
		return
	}
	h.uploads[id] = up
	out := up.snapshot()
	h.mu.Unlock()
	h.writeJSON(w, http.StatusCreated, out)
}

// discardDir removes an upload directory an upload did not claim, logging a
// failure it cannot act on.
func (h *Handler) discardDir(id string) {
	if err := h.root.RemoveAll(id); err != nil {
		h.log.Error("upload: upload directory not removed", "err", err.Error())
	}
}

// state answers with an upload's state.
func (h *Handler) state(w http.ResponseWriter, r *http.Request, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	up, ok := h.liveLocked(id)
	if !ok {
		h.fail(w, noSuchUpload())
		return
	}
	h.writeJSON(w, http.StatusOK, up.snapshot())
}

// putChunk stores one chunk of an upload.
func (h *Handler) putChunk(w http.ResponseWriter, r *http.Request, id, indexText string) {
	index, err := strconv.ParseInt(indexText, 10, 32)
	if err != nil || index < 0 {
		h.fail(w, badRequest("index", "must be a non-negative integer"))
		return
	}
	declared := strings.ToLower(strings.TrimSpace(r.Header.Get(chunkDigestHeader)))
	if !validDigest(declared) {
		h.fail(w, badRequest(chunkDigestHeader, "must be the chunk's SHA-256, 64 hex digits"))
		return
	}

	h.mu.Lock()
	up, ok := h.liveLocked(id)
	if !ok {
		h.mu.Unlock()
		h.fail(w, noSuchUpload())
		return
	}
	if index > h.maxIndex(up) {
		h.mu.Unlock()
		h.fail(w, badRequest("index", "is beyond the upload's last chunk index"))
		return
	}
	// A chunk the upload already holds is sent again when a client does
	// not know whether its first attempt arrived. Its body is still
	// checked against the digest the client declares for it, but it needs
	// no room, because the bytes it replaces are already counted.
	_, repeat := up.chunks[index]
	chunkSize := up.chunkSize
	sizeKnown, chunkCount, sizeBytes := up.sizeKnown, up.chunkCount, up.sizeBytes
	usedUpload := up.storedBytes
	usedOwner := h.ownerBytesLocked(up.owner)
	roomUpload := h.maxUp - usedUpload
	roomOwner := h.maxOwner - usedOwner
	if roomUpload <= 0 && !repeat {
		h.mu.Unlock()
		h.fail(w, overLimit(limitUpload, h.maxUp, usedUpload))
		return
	}
	if roomOwner <= 0 && !repeat {
		h.mu.Unlock()
		h.fail(w, overLimit(limitOwner, h.maxOwner, usedOwner))
		return
	}
	h.mu.Unlock()

	// The read stops one byte past the limit, so a body that would not fit
	// is refused while it arrives rather than after it is on disk.
	limit := chunkSize
	if !repeat {
		limit = min(chunkSize, min(roomUpload, roomOwner))
	}
	staged, err := h.stageChunk(id, index, r.Body, limit)
	if err != nil {
		h.fail(w, fmt.Errorf("upload: stage chunk: %w", err))
		return
	}
	want := chunkLength(sizeBytes, chunkSize, chunkCount, index)
	switch {
	case staged.sizeBytes > chunkSize:
		h.discard(staged.name)
		h.fail(w, badRequest("body", "is larger than the chunk size"))
		return
	case staged.sizeBytes > roomUpload && !repeat:
		h.discard(staged.name)
		h.fail(w, overLimit(limitUpload, h.maxUp, usedUpload+staged.sizeBytes))
		return
	case staged.sizeBytes > roomOwner && !repeat:
		h.discard(staged.name)
		h.fail(w, overLimit(limitOwner, h.maxOwner, usedOwner+staged.sizeBytes))
		return
	case staged.sizeBytes == 0:
		h.discard(staged.name)
		h.fail(w, badRequest("body", "must not be empty"))
		return
	case sizeKnown && staged.sizeBytes != want:
		h.discard(staged.name)
		h.fail(w, badRequest("body", "is not the length the upload declares for this chunk"))
		return
	case staged.sha256 != declared:
		h.discard(staged.name)
		h.fail(w, mismatch(index, declared, staged.sha256))
		return
	}

	h.mu.Lock()
	up, ok = h.liveLocked(id)
	if !ok {
		h.mu.Unlock()
		h.discard(staged.name)
		h.fail(w, noSuchUpload())
		return
	}
	if have, ok := up.chunks[index]; ok {
		// The upload took this chunk while the body was streaming, or the
		// client is sending one it already holds. The bytes on disk stand
		// and the client is told what the upload holds.
		out := up.snapshot()
		h.mu.Unlock()
		h.discard(staged.name)
		if have.sha256 != staged.sha256 {
			h.fail(w, mismatch(index, declared, have.sha256))
			return
		}
		h.writeJSON(w, http.StatusOK, out)
		return
	}
	// Another request for the same owner may have taken the budget while
	// this chunk was streaming.
	usedUpload = up.storedBytes
	usedOwner = h.ownerBytesLocked(up.owner)
	switch {
	case usedUpload+staged.sizeBytes > h.maxUp:
		h.mu.Unlock()
		h.discard(staged.name)
		h.fail(w, overLimit(limitUpload, h.maxUp, usedUpload+staged.sizeBytes))
		return
	case usedOwner+staged.sizeBytes > h.maxOwner:
		h.mu.Unlock()
		h.discard(staged.name)
		h.fail(w, overLimit(limitOwner, h.maxOwner, usedOwner+staged.sizeBytes))
		return
	}
	if err := h.root.Rename(staged.name, chunkName(id, index)); err != nil {
		h.mu.Unlock()
		h.discard(staged.name)
		h.fail(w, fmt.Errorf("upload: store chunk: %w", err))
		return
	}
	up.chunks[index] = chunk{sizeBytes: staged.sizeBytes, sha256: staged.sha256}
	if index > up.highest {
		up.highest = index
	}
	up.storedBytes += staged.sizeBytes
	up.expiresAt = h.now().Add(h.ttl)
	out := up.snapshot()
	h.mu.Unlock()
	h.writeJSON(w, http.StatusOK, out)
}

// complete assembles an upload's chunks, checks the digest declared for the
// whole file, and stores the bytes.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request, id string) {
	var req completeRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	declared := strings.ToLower(strings.TrimSpace(req.SHA256))
	if !validDigest(declared) {
		h.fail(w, badRequest("sha256", "must be the upload's SHA-256, 64 hex digits"))
		return
	}
	h.mu.Lock()
	up, ok := h.liveLocked(id)
	if !ok {
		h.mu.Unlock()
		h.fail(w, noSuchUpload())
		return
	}
	if missing := up.missing(); len(missing) > 0 {
		h.mu.Unlock()
		h.fail(w, incomplete(missing))
		return
	}
	count := up.span()
	owner, contentType, visibility := up.owner, up.contentType, up.visibility
	delete(h.uploads, id)
	h.mu.Unlock()

	actual, size, err := h.digestUpload(id, count)
	if err != nil {
		h.restore(up)
		h.fail(w, fmt.Errorf("upload: read chunk: %w", err))
		return
	}
	if actual != declared {
		h.restore(up)
		h.fail(w, &apiError{
			status:  http.StatusConflict,
			code:    codeHashMismatch,
			message: "The assembled upload does not match the digest declared for it.",
			detail:  digestDetail{Declared: declared, Actual: actual},
		})
		return
	}
	if err := h.persist(r.Context(), up, count); err != nil {
		h.restore(up)
		h.fail(w, storeFailure(err))
		return
	}
	if err := h.root.RemoveAll(id); err != nil {
		h.log.Error("upload: staged chunks not removed", "err", err.Error())
	}
	h.writeJSON(w, http.StatusCreated, completeResponse{
		ID:          id,
		Owner:       owner,
		ContentType: contentType,
		Visibility:  visibilityName(visibility),
		SizeBytes:   size,
		SHA256:      actual,
	})
}

// parseVisibility resolves a request's visibility to the store's.
func parseVisibility(s string) (mediastore.Visibility, error) {
	switch s {
	case "", "private":
		return mediastore.Private, nil
	case "public":
		return mediastore.Public, nil
	default:
		return "", badRequest("visibility", `must be "private" or "public"`)
	}
}

// visibilityName names a visibility the way a request declares it.
func visibilityName(v mediastore.Visibility) string {
	if v == mediastore.Public {
		return "public"
	}
	return "private"
}
