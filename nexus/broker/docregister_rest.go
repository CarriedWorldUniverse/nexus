package broker

// Document register REST surface (M1 Unit 2, PHASE2-DESIGN.md §9): the
// operator+shadow shared workbench. Two trust tiers on the same underlying
// docregister.Register:
//
//   - /api/docs/*        — the shared workbench: create/get/list/revise/
//     submit-for-approval. Broker-authenticated (b.auth) so shadow can
//     draft/revise from a croft planning session, same as any other aspect
//     endpoint.
//   - /api/admin/docs/*  — verdicts: approve / approve-with-changes /
//     reject / supersede. requireAdmin — operator-only, the same
//     separation-of-duties lesson as the rest of admin.go (worker status,
//     credentials): drafting and deciding are different privilege levels
//     even though they operate on the same record.
//
// Registered only when cfg.DocRegister is configured; nil = both surfaces
// absent (404 from the mux), matching the WorkerStatusStore/Credentials
// convention elsewhere in this package.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/CarriedWorldUniverse/nexus/nexus/docregister"
)

// registerDocRegisterWorkbench wires the broker-authenticated /api/docs/*
// routes. Called from the general (non-admin) route section alongside
// /api/aspects, /api/images.
func (b *Broker) registerDocRegisterWorkbench(mux *http.ServeMux) {
	if b.cfg.DocRegister == nil {
		return
	}
	mux.Handle("POST /api/docs", b.auth(http.HandlerFunc(b.handleDocsCreate)))
	mux.Handle("GET /api/docs", b.auth(http.HandlerFunc(b.handleDocsList)))
	mux.Handle("GET /api/docs/{id}", b.auth(http.HandlerFunc(b.handleDocGet)))
	mux.Handle("GET /api/docs/{id}/content", b.auth(http.HandlerFunc(b.handleDocGetContent)))
	mux.Handle("POST /api/docs/{id}/revise", b.auth(http.HandlerFunc(b.handleDocRevise)))
	mux.Handle("POST /api/docs/{id}/submit", b.auth(http.HandlerFunc(b.handleDocSubmit)))
}

// registerDocRegisterVerdicts wires the requireAdmin /api/admin/docs/*
// routes. Called from registerAdmin alongside the rest of the admin
// surface — same "config gates the surface" convention, additionally
// gated on cfg.DocRegister (registerAdmin itself is already gated on
// cfg.Admin != nil, per the existing convention for every admin route).
func (b *Broker) registerDocRegisterVerdicts(mux *http.ServeMux) {
	if b.cfg.DocRegister == nil {
		return
	}
	mux.Handle("POST /api/admin/docs/{id}/approve", b.requireAdmin(http.HandlerFunc(b.handleDocApprove)))
	mux.Handle("POST /api/admin/docs/{id}/approve-with-changes", b.requireAdmin(http.HandlerFunc(b.handleDocApproveWithChanges)))
	mux.Handle("POST /api/admin/docs/{id}/reject", b.requireAdmin(http.HandlerFunc(b.handleDocReject)))
	mux.Handle("POST /api/admin/docs/{id}/supersede", b.requireAdmin(http.HandlerFunc(b.handleDocSupersede)))
}

// docPayload is the wire shape for a document, per PHASE2-DESIGN.md §9's
// {id, kind, title, version, status, work_item_id, cairn_ref, approvals[]}.
type docPayload struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Title      string            `json:"title"`
	Version    int               `json:"version"`
	Status     string            `json:"status"`
	WorkItemID string            `json:"work_item_id"`
	CairnRef   string            `json:"cairn_ref"`
	Approvals  []approvalPayload `json:"approvals,omitempty"`
	CreatedAt  int64             `json:"created_at_ms"`
	UpdatedAt  int64             `json:"updated_at_ms"`
}

type approvalPayload struct {
	By       string `json:"by"`
	Verdict  string `json:"verdict"`
	Comments string `json:"comments,omitempty"`
	At       int64  `json:"at_ms"`
}

func docToPayload(d docregister.Document) docPayload {
	out := docPayload{
		ID:         d.ID,
		Kind:       string(d.Kind),
		Title:      d.Title,
		Version:    d.Version,
		Status:     string(d.Status),
		WorkItemID: d.WorkItemID,
		CairnRef:   d.CairnRef,
		CreatedAt:  d.CreatedAt.UnixMilli(),
		UpdatedAt:  d.UpdatedAt.UnixMilli(),
	}
	for _, a := range d.Approvals {
		out.Approvals = append(out.Approvals, approvalPayload{
			By: a.By, Verdict: string(a.Verdict), Comments: a.Comments, At: a.At.UnixMilli(),
		})
	}
	return out
}

// writeDocRegisterErr maps docregister sentinel errors to REST status
// codes; anything else is a 500.
func writeDocRegisterErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, docregister.ErrNotFound):
		writeError(w, http.StatusNotFound, "document not found")
	case errors.Is(err, docregister.ErrInvalidKind):
		writeError(w, http.StatusBadRequest, "invalid kind")
	case errors.Is(err, docregister.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid status transition: "+err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB — MD bodies are text
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "request body malformed")
		return false
	}
	return true
}

// --- workbench handlers (broker-authenticated) ---

type docCreateBody struct {
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	WorkItemID string `json:"work_item_id"`
	MDContent  string `json:"md_content"`
}

func (b *Broker) handleDocsCreate(w http.ResponseWriter, r *http.Request) {
	var req docCreateBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.WorkItemID == "" {
		writeError(w, http.StatusBadRequest, "work_item_id required — every doc belongs to a work item")
		return
	}
	id, err := b.cfg.DocRegister.CreateDoc(r.Context(), docregister.Kind(req.Kind), req.Title, req.WorkItemID, req.MDContent)
	if err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	doc, err := b.cfg.DocRegister.GetDoc(r.Context(), id)
	if err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, docToPayload(doc))
}

type docsListResponse struct {
	Docs []docPayload `json:"docs"`
}

func (b *Broker) handleDocsList(w http.ResponseWriter, r *http.Request) {
	filter := docregister.ListFilter{
		Kind:   docregister.Kind(r.URL.Query().Get("kind")),
		Status: docregister.Status(r.URL.Query().Get("status")),
		Stream: r.URL.Query().Get("stream"),
	}
	docs, err := b.cfg.DocRegister.ListDocs(r.Context(), filter)
	if err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	out := docsListResponse{Docs: make([]docPayload, 0, len(docs))}
	for _, d := range docs {
		out.Docs = append(out.Docs, docToPayload(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (b *Broker) handleDocGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	doc, err := b.cfg.DocRegister.GetDoc(r.Context(), id)
	if err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docToPayload(doc))
}

type docContentResponse struct {
	Content string `json:"content"`
}

func (b *Broker) handleDocGetContent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	content, err := b.cfg.DocRegister.GetContent(r.Context(), id)
	if err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docContentResponse{Content: content})
}

type docReviseBody struct {
	MDContent string `json:"md_content"`
}

func (b *Broker) handleDocRevise(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req docReviseBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := b.cfg.DocRegister.Revise(r.Context(), id, req.MDContent); err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	doc, err := b.cfg.DocRegister.GetDoc(r.Context(), id)
	if err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docToPayload(doc))
}

func (b *Broker) handleDocSubmit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := b.cfg.DocRegister.SubmitForApproval(r.Context(), id); err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	doc, err := b.cfg.DocRegister.GetDoc(r.Context(), id)
	if err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docToPayload(doc))
}

// --- verdict handlers (requireAdmin — operator-only) ---

type docVerdictBody struct {
	By        string   `json:"by"`
	Comments  string   `json:"comments,omitempty"`
	Reasons   []string `json:"reasons,omitempty"`
	MDContent string   `json:"md_content,omitempty"` // approve-with-changes only
}

func (b *Broker) handleDocApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req docVerdictBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.By == "" {
		writeError(w, http.StatusBadRequest, "by required")
		return
	}
	if err := b.cfg.DocRegister.Approve(r.Context(), id, req.By, req.Comments); err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	b.writeDocAfter(w, r, id)
}

func (b *Broker) handleDocApproveWithChanges(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req docVerdictBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.By == "" {
		writeError(w, http.StatusBadRequest, "by required")
		return
	}
	if err := b.cfg.DocRegister.ApproveWithChanges(r.Context(), id, req.By, req.MDContent, req.Comments); err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	b.writeDocAfter(w, r, id)
}

func (b *Broker) handleDocReject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req docVerdictBody
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.By == "" {
		writeError(w, http.StatusBadRequest, "by required")
		return
	}
	if err := b.cfg.DocRegister.Reject(r.Context(), id, req.By, req.Reasons); err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	b.writeDocAfter(w, r, id)
}

func (b *Broker) handleDocSupersede(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := b.cfg.DocRegister.Supersede(r.Context(), id); err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	b.writeDocAfter(w, r, id)
}

func (b *Broker) writeDocAfter(w http.ResponseWriter, r *http.Request, id string) {
	doc, err := b.cfg.DocRegister.GetDoc(r.Context(), id)
	if err != nil {
		writeDocRegisterErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docToPayload(doc))
}
