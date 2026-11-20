package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"dataanalyst/api/internal/contract"
	"dataanalyst/api/internal/datasets"
	"dataanalyst/api/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// SessionView is the GET /api/sessions/{id} response.
type SessionView struct {
	ID       string           `json:"id"`
	Datasets []contract.Table `json:"datasets"`
	Messages []MessageView    `json:"messages"`
}

type MessageView struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.q.CreateSession(r.Context(), uuid.New())
	if err != nil {
		s.internalError(w, "create session", err)
		return
	}
	if err := s.Layout.EnsureSession(sess.ID.String()); err != nil {
		s.internalError(w, "create session dirs", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": sess.ID.String()})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	ds, err := s.q.ListDatasets(ctx, sess.ID)
	if err != nil {
		s.internalError(w, "list datasets", err)
		return
	}
	msgs, err := s.q.ListMessages(ctx, sess.ID)
	if err != nil {
		s.internalError(w, "list messages", err)
		return
	}
	view := SessionView{ID: sess.ID.String(), Datasets: []contract.Table{}, Messages: []MessageView{}}
	for _, d := range ds {
		view.Datasets = append(view.Datasets, datasetTable(d))
	}
	for _, m := range msgs {
		view.Messages = append(view.Messages, MessageView{
			ID: m.ID.String(), Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt.Time,
		})
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) uploadDataset(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	// Allow multipart overhead on top of the file itself.
	r.Body = http.MaxBytesReader(w, r.Body, s.Config.MaxUploadBytes+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, datasets.ErrTooLarge.Error())
			return
		}
		writeError(w, http.StatusBadRequest, `expected a multipart form with a "file" field`)
		return
	}
	defer file.Close()

	ctx := r.Context()
	existing, err := s.q.ListDatasets(ctx, sess.ID)
	if err != nil {
		s.internalError(w, "list datasets", err)
		return
	}
	names := make([]string, len(existing))
	for i, d := range existing {
		names[i] = d.TableName
	}
	table, err := s.loader.Load(ctx, sess.ID.String(), header.Filename, file, names)
	switch {
	case errors.Is(err, datasets.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case errors.Is(err, datasets.ErrUnsupported):
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	case errors.Is(err, datasets.ErrEmpty), errors.Is(err, datasets.ErrBadFile):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, datasets.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, "load dataset", err)
		return
	}
	cols, _ := json.Marshal(table.Columns)
	if _, err := s.q.CreateDataset(ctx, db.CreateDatasetParams{
		ID: uuid.New(), SessionID: sess.ID, TableName: table.Name,
		OriginalFilename: header.Filename, Rows: table.Rows, ColumnsJson: cols,
	}); err != nil {
		s.internalError(w, "record dataset", err)
		return
	}
	_ = s.q.TouchSession(ctx, sess.ID)
	writeJSON(w, http.StatusCreated, map[string]contract.Table{"table": table})
}

func datasetTable(d db.Dataset) contract.Table {
	t := contract.Table{Name: d.TableName, Rows: d.Rows, Columns: []contract.Column{}}
	_ = json.Unmarshal(d.ColumnsJson, &t.Columns)
	return t
}

func pgTime(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
