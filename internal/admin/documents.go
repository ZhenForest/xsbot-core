package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"github.com/ZhenForest/xsbot-core/sdk"
)

// Authentication, temporary-password and CSRF checks precede this handler.
// Large document bodies have independent limits and admission; existing config
// and action limits remain unchanged. Responses are text, not double-encoded JSON.
func (s *Server) pluginDocument(w http.ResponseWriter, r *http.Request) {
	if s.Backend.Initializing() {
		http.Error(w, "initializing or closing", 409)
		return
	}
	select {
	case s.documentSlots <- struct{}{}:
		defer func() { <-s.documentSlots }()
	default:
		http.Error(w, "文档操作繁忙，请稍后重试", 429)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	q := r.URL.Query()
	var out sdk.Document
	var err error
	switch {
	case r.URL.Path == "/api/plugin-document-editors" && r.Method == "GET":
		var editors []sdk.DocumentEditor
		editors, err = s.Backend.DocumentEditors(ctx, q.Get("id"))
		if err == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(editors)
			return
		}
	case r.URL.Path == "/api/plugin-document/read" && r.Method == "POST":
		var b []byte
		b, err = io.ReadAll(io.LimitReader(r.Body, sdk.MaxDocumentSelectorBytes+1))
		if err == nil && len(b) > sdk.MaxDocumentSelectorBytes {
			err = fmt.Errorf("文档选择参数过大")
		}
		if err == nil {
			out, err = s.Backend.ReadDocument(ctx, q.Get("id"), q.Get("editor"), string(b))
		}
	case r.URL.Path == "/api/plugin-document/save" && r.Method == "POST":
		var b []byte
		if !strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]), "text/plain") {
			err = fmt.Errorf("文档正文须为 UTF-8 文本")
		} else {
			b, err = io.ReadAll(io.LimitReader(r.Body, sdk.MaxDocumentBytes+1))
			if err == nil && len(b) > sdk.MaxDocumentBytes {
				err = fmt.Errorf("文档内容超过 8 MiB")
			}
		}
		if err == nil {
			out, err = s.Backend.SaveDocument(ctx, q.Get("id"), q.Get("editor"), q.Get("key"), r.Header.Get("X-Document-Revision"), string(b))
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sdk.ErrDocumentConflict) {
			status = http.StatusConflict
		}
		reply(w, status, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Document-Key", out.Key)
	w.Header().Set("X-Document-Revision", out.Revision)
	w.Write([]byte(out.Content))
}
