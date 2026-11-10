package admin

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/config"
	"github.com/ZhenForest/xsbot-core/internal/logging"
)

//go:embed index.html
var assets embed.FS

type Backend struct {
	PluginConfiguration       func(context.Context, string) (any, error)
	SavePluginConfiguration   func(context.Context, string, string, string) error
	PluginConfigurationAction func(context.Context, string, string, string, string) (string, error)
	State                     func() any
	CoreDraft                 func() (any, error)
	SaveCore                  func([]byte) error
	ApplyCore                 func() error
	RebaseCore                func(string, string) error
	RuleDraft                 func(string) (config.Draft, error)
	RuleCurrent               func(string) (any, error)
	RuleRevision              func() string
	SaveRule                  func(string, config.Draft) error
	ApplyRules                func(string, string) error
	PreviewRules              func(string) (config.RulePreview, error)
	RebaseRule                func(string, string, string) error
	RuleForm                  func(string) (any, error)
	EncodeRule                func([]byte) (string, error)
	Restart                   func()
	Initializing              func() bool
	Logger                    *logging.Logger
}
type Server struct {
	HTTP     *http.Server
	Identity *Identity
	Backend  Backend
	port     int
}

func NewServer(identity *Identity, b Backend, port int, publicIP ...string) *Server {
	s := &Server{Identity: identity, Backend: b, port: port}
	host := "0.0.0.0"
	if len(publicIP) > 0 {
		ip := net.ParseIP(publicIP[0])
		if ip != nil && ip.To4() == nil {
			host = "::"
		}
	}
	s.HTTP = &http.Server{Addr: net.JoinHostPort(host, strconv.Itoa(port)), Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	return s
}
func (s *Server) Start(cert, key string) (<-chan error, error) {
	listener, e := net.Listen("tcp", s.HTTP.Addr)
	if e != nil {
		return nil, e
	}
	done := make(chan error, 1)
	go func() { done <- s.HTTP.ServeTLS(listener, cert, key) }()
	return done, nil
}
func (s *Server) Close(ctx context.Context) error { return s.HTTP.Shutdown(ctx) }
func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(config.WebValue(value))
}
func problem(w http.ResponseWriter, e error) { reply(w, 400, map[string]any{"error": e.Error()}) }
func requestJSON(r *http.Request, out any) error {
	data, e := requestBody(r)
	if e != nil {
		return e
	}
	return config.DecodeWebJSON(data, out)
}
func requestBody(r *http.Request) ([]byte, error) {
	data, e := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
	if e == nil && len(data) > 4<<20 {
		e = fmt.Errorf("request body exceeds 4 MiB")
	}
	return data, e
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "https://"+r.Host
}
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
	if r.URL.Path == "/" && r.Method == "GET" {
		page, _ := assets.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
		return
	}
	if r.URL.Path == "/api/login" && r.Method == "POST" {
		if !sameOrigin(r) {
			http.Error(w, "origin rejected", 403)
			return
		}
		var body struct{ Username, Password string }
		if e := requestJSON(r, &body); e != nil {
			problem(w, e)
			return
		}
		session, csrf, temporary, e := s.Identity.Login(r.RemoteAddr, body.Username, body.Password)
		if e != nil {
			problem(w, e)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "core_session", Value: session, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		reply(w, 200, map[string]any{"csrf": csrf, "temporary": temporary})
		return
	}
	cookie, e := r.Cookie("core_session")
	if e != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	valid, temporary, csrf := s.Identity.Check(cookie.Value)
	if !valid {
		http.Error(w, "session expired", 401)
		return
	}
	if r.Method != "GET" {
		if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
			http.Error(w, "origin/session check failed", 403)
			return
		}
	}
	if temporary && r.URL.Path != "/api/account" && r.URL.Path != "/api/logout" {
		http.Error(w, "change temporary password first", 403)
		return
	}
	switch {
	case r.URL.Path == "/api/plugin-config" || r.URL.Path == "/api/plugin-action":
		s.pluginConfiguration(w, r)
	case r.URL.Path == "/api/account" && r.Method == "POST":
		var body struct{ Username, Password string }
		if e = requestJSON(r, &body); e == nil {
			e = s.Identity.Change(cookie.Value, body.Username, body.Password)
		}
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]bool{"relogin": true})
	case r.URL.Path == "/api/logout" && r.Method == "POST":
		s.Identity.Logout()
		http.SetCookie(w, &http.Cookie{Name: "core_session", Value: "", MaxAge: -1, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		reply(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/api/session" && r.Method == "GET":
		reply(w, 200, map[string]any{"csrf": csrf, "temporary": temporary})
	case r.URL.Path == "/api/state" && r.Method == "GET":
		reply(w, 200, s.Backend.State())
	case r.URL.Path == "/api/core" && r.Method == "GET":
		value, e := s.Backend.CoreDraft()
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, value)
	case r.URL.Path == "/api/core" && r.Method == "POST":
		data, e := requestBody(r)
		if e == nil {
			e = s.Backend.SaveCore(data)
		}
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]bool{"saved": true})
	case r.URL.Path == "/api/core/apply" && r.Method == "POST":
		if s.Backend.Initializing() {
			http.Error(w, "initializing; drafts only", 409)
			return
		}
		if e = s.Backend.ApplyCore(); e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]bool{"restart_required": true})
	case r.URL.Path == "/api/core/rebase" && r.Method == "POST":
		var body struct{ From, Current string }
		if e = requestJSON(r, &body); e == nil {
			e = s.Backend.RebaseCore(body.From, body.Current)
		}
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]bool{"reviewed": true})
	case r.URL.Path == "/api/rule/rebase" && r.Method == "POST":
		var body struct{ ID, From, Current string }
		if e = requestJSON(r, &body); e == nil {
			e = s.Backend.RebaseRule(body.ID, body.From, body.Current)
		}
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]bool{"reviewed": true})
	case r.URL.Path == "/api/rule" && r.Method == "GET":
		id := r.URL.Query().Get("id")
		draft, e := s.Backend.RuleDraft(id)
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]any{"Schema": draft.Schema, "Base": draft.Base, "Text": draft.Text, "CurrentBase": s.Backend.RuleRevision()})
	case r.URL.Path == "/api/rule/formal" && r.Method == "GET":
		value, e := s.Backend.RuleCurrent(r.URL.Query().Get("id"))
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, value)
	case r.URL.Path == "/api/rule" && r.Method == "POST":
		var body struct {
			ID    string
			Draft config.Draft
		}
		if e = requestJSON(r, &body); e == nil {
			e = s.Backend.SaveRule(body.ID, body.Draft)
		}
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]bool{"saved": true})
	case r.URL.Path == "/api/rule/form" && r.Method == "POST":
		data, e := requestBody(r)
		var text string
		if e == nil {
			text, e = s.Backend.EncodeRule(data)
		}
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]string{"text": text})
	case r.URL.Path == "/api/rule/form" && r.Method == "GET":
		value, e := s.Backend.RuleForm(r.URL.Query().Get("id"))
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, value)
	case r.URL.Path == "/api/rule/preview" && r.Method == "POST":
		var body struct{ Base string }
		if e = requestJSON(r, &body); e != nil {
			problem(w, e)
			return
		}
		preview, e := s.Backend.PreviewRules(body.Base)
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, preview)
	case r.URL.Path == "/api/rule/apply" && r.Method == "POST":
		if s.Backend.Initializing() {
			http.Error(w, "initializing; drafts only", 409)
			return
		}
		var body struct {
			Base   string
			Review string
		}
		if e = requestJSON(r, &body); e == nil {
			e = s.Backend.ApplyRules(body.Base, body.Review)
		}
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]bool{"applied": true})
	case r.URL.Path == "/api/restart" && r.Method == "POST":
		if s.Backend.Initializing() {
			http.Error(w, "initializing", 409)
			return
		}
		reply(w, 200, map[string]bool{"restarting": true})
		go s.Backend.Restart()
	case r.URL.Path == "/api/logs/download" && r.Method == "GET":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=core-logs.txt")
		if e = s.Backend.Logger.Download(w); e != nil {
			fmt.Fprintln(w, "\nlog snapshot error:", e)
		}
	case r.URL.Path == "/api/logs" && r.Method == "GET":
		q := r.URL.Query()
		offset := 0
		if value := q.Get("offset"); value != "" {
			var parseError error
			offset, parseError = strconv.Atoi(value)
			if parseError != nil || offset < 0 {
				problem(w, fmt.Errorf("invalid log page offset"))
				return
			}
		}
		rows, more, snapshot, e := s.Backend.Logger.ReadPage(logging.Filter{Level: q.Get("level"), Plugin: q.Get("plugin"), Keyword: q.Get("keyword"), Offset: offset, Limit: 100}, q.Get("snapshot"))
		if e != nil {
			problem(w, e)
			return
		}
		reply(w, 200, map[string]any{"rows": rows, "more": more, "snapshot": snapshot})
	default:
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unknown endpoint", 404)
		} else {
			http.NotFound(w, r)
		}
	}
}
