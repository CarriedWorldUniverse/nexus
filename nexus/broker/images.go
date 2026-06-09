package broker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

// imageStore is a small in-memory, capped, ephemeral image store backing the
// chat attach feature (NEX-538 MVP). Screenshots for bug reports are transient,
// so this avoids standing up an object store; the durable R2-backed version is
// tracked separately. Bounded so a flood can't OOM the broker.
const (
	maxImageBytes = 6 << 20 // 6 MiB per image
	maxImageCount = 50      // keep the newest N
)

type storedImage struct {
	data []byte
	ct   string
}

type imageStore struct {
	mu    sync.Mutex
	imgs  map[string]storedImage
	order []string
}

func newImageStore() *imageStore {
	return &imageStore{imgs: map[string]storedImage{}}
}

func (s *imageStore) put(data []byte, ct string) string {
	id := imageID()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imgs[id] = storedImage{data: data, ct: ct}
	s.order = append(s.order, id)
	for len(s.order) > maxImageCount {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.imgs, old)
	}
	return id
}

func (s *imageStore) get(id string) (storedImage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	im, ok := s.imgs[id]
	return im, ok
}

func imageID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// handleImageUpload (POST /api/images, operator-gated): body = raw image bytes,
// Content-Type = the image type. Returns {"url": "/api/images/<id>"}.
func (b *Broker) handleImageUpload(w http.ResponseWriter, r *http.Request) {
	if b.images == nil {
		http.Error(w, "image store not configured", http.StatusServiceUnavailable)
		return
	}
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		http.Error(w, "expected an image/* Content-Type", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxImageBytes+1))
	if err != nil {
		http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(data) == 0 {
		http.Error(w, "empty image", http.StatusBadRequest)
		return
	}
	if len(data) > maxImageBytes {
		http.Error(w, "image too large (max 6 MiB)", http.StatusRequestEntityTooLarge)
		return
	}
	id := b.images.put(data, ct)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": "/api/images/" + id})
}

// handleImageGet (GET /api/images/{id}, ungated): the random id is a capability
// URL — the browser <img> tag can't send a bearer, so the unguessable id is the
// gate. Serves the stored bytes.
func (b *Broker) handleImageGet(w http.ResponseWriter, r *http.Request) {
	if b.images == nil {
		http.Error(w, "image store not configured", http.StatusServiceUnavailable)
		return
	}
	im, ok := b.images.get(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", im.ct)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(im.data)
}
