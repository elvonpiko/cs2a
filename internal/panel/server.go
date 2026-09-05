package panel

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cs2a/internal/cs2"
	"cs2a/internal/panel/web"
	"cs2a/internal/version"
)

// Server is the panel web application.
type Server struct {
	cfg   Config
	store *Store
	agent *AgentClient
	log   *slog.Logger
	// throttle slows password guessing on /login (the panel is the only
	// internet-facing component).
	throttle *loginThrottle
}

// NewServer wires the panel.
func NewServer(cfg Config, store *Store, agent *AgentClient, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: store, agent: agent, log: log, throttle: newLoginThrottle()}
}

// ctxKey is the context key type for request-scoped values.
type ctxKey int

const ctxUser ctxKey = 1

// userFromCtx extracts the authenticated user.
func userFromCtx(r *http.Request) *User {
	if v, ok := r.Context().Value(ctxUser).(*User); ok {
		return v
	}
	return nil
}

const sessionCookie = "cs2a_session"

// ServeHTTP lets *Server be used directly as an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// Handler builds the panel's http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// static
	mux.HandleFunc("GET /static/", s.handleStatic)

	// auth
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginPost)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /setup", s.handleSetupPage)
	mux.HandleFunc("POST /setup", s.handleSetupPost)

	// pages
	mux.HandleFunc("GET /{$}", s.auth(s.handleServerPage))
	mux.HandleFunc("GET /partials/status-card", s.auth(s.handleStatusCardPartial))
	// The journal can contain connection details, so it stays admin-only — the
	// same rule the page-level log card follows.
	mux.HandleFunc("GET /partials/server-logs", s.admin(s.handleServerLogsPartial))
	mux.HandleFunc("GET /plugins", s.admin(s.handlePluginsPage))
	mux.HandleFunc("GET /partials/plugin-jobs", s.admin(s.handlePluginJobsPartial))
	mux.HandleFunc("GET /plugins/{id}/config", s.admin(s.handlePluginConfigPage))
	mux.HandleFunc("POST /plugins/{id}/config", s.admin(s.handlePluginConfigPost))
	mux.HandleFunc("POST /plugins/{id}/install", s.admin(s.handlePluginInstall))
	mux.HandleFunc("POST /plugins/{id}/uninstall", s.admin(s.handlePluginUninstall))
	mux.HandleFunc("GET /access", s.admin(s.handleAccessPage))
	mux.HandleFunc("POST /access/password", s.admin(s.handleAccessPassword))
	mux.HandleFunc("POST /access/whitelist", s.admin(s.handleAccessWhitelist))
	mux.HandleFunc("POST /access/whitelist/toggle", s.admin(s.handleAccessWhitelistToggle))
	mux.HandleFunc("POST /access/whitelist/add-user", s.admin(s.handleAccessWhitelistAddUser))
	mux.HandleFunc("GET /users", s.admin(s.handleUsersPage))
	mux.HandleFunc("POST /users/create", s.admin(s.handleUserCreate))
	mux.HandleFunc("POST /users/delete", s.admin(s.handleUserDelete))
	mux.HandleFunc("GET /loadout", s.auth(s.handleLoadoutPage))
	mux.HandleFunc("POST /loadout", s.auth(s.handleLoadoutPost))

	// server actions
	mux.HandleFunc("POST /do/start", s.admin(s.handleServerAction("start")))
	mux.HandleFunc("POST /do/stop", s.admin(s.handleServerAction("stop")))
	mux.HandleFunc("POST /do/restart", s.admin(s.handleServerAction("restart")))
	mux.HandleFunc("POST /do/rcon-repair", s.admin(s.handleRCONRepair))
	mux.HandleFunc("POST /do/map", s.auth(s.handleMapChange))

	return s.logMiddleware(s.csrfMiddleware(mux))
}

// --- middleware ---------------------------------------------------------

// statusRecorder captures the response status for logging. It forwards the
// optional writer interfaces (Flush, Unwrap) so streaming responses and
// htmx polling are not buffered by the wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush lets handlers stream (http.Flusher is not promoted through embedding
// when the wrapper is passed as a plain ResponseWriter).
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// csrfMiddleware rejects cross-site state-changing requests. Go's
// CrossOriginProtection checks Sec-Fetch-Site (with an Origin fallback), which
// covers every browser the panel targets and needs no per-form tokens.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	p := http.NewCrossOriginProtection()
	// The panel is commonly reached both directly and through a reverse proxy
	// hostname; trust the configured public origin when one is set.
	for _, origin := range s.cfg.TrustedOrigins() {
		if err := p.AddTrustedOrigin(origin); err != nil {
			s.log.Warn("ignoring invalid trusted origin", "origin", origin, "err", err)
		}
	}
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("cross-origin request denied", "path", r.URL.Path, "origin", r.Header.Get("Origin"))
		http.Error(w, "cross-origin request denied — open the panel from its own address and try again", http.StatusForbidden)
	}))
	return p.Handler(next)
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

// auth wraps pages that any signed-in role may access.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(next, "admin", "player")
}

// admin wraps admin-only pages/actions.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(next, "admin")
}

func (s *Server) requireRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		u, err := s.store.GetSessionUser(HashToken(cookie.Value))
		if err != nil || u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		allowed := false
		for _, role := range roles {
			if u.Role == role {
				allowed = true
				break
			}
		}
		if !allowed {
			redirectFlash(w, r, "/", "err", "You do not have access to that page — admins only.")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	}
}

// --- static -------------------------------------------------------------

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if strings.Contains(name, "..") || name == "" {
		http.NotFound(w, r)
		return
	}
	b, err := web.Static.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// --- auth handlers ------------------------------------------------------

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if userFromCtx(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, "")
}

func (s *Server) renderLogin(w http.ResponseWriter, errMsg string) {
	s.renderLoginBody(w, errMsg)
}

// renderLoginBody writes the login page. It is separate from renderLogin so a
// throttled response can set its status code before the body is written.
func (s *Server) renderLoginBody(w http.ResponseWriter, errMsg string) {
	comp := web.Bare("Sign in", web.Login(errMsg))
	if err := comp.Render(context.Background(), w); err != nil {
		s.log.Error("render login", "err", err)
	}
}

// waitLabel renders a retry delay the way a person would say it.
func waitLabel(d time.Duration) string {
	if d < time.Minute {
		secs := int(d.Seconds()) + 1
		return strconv.Itoa(secs) + " seconds"
	}
	mins := int(d.Minutes()) + 1
	if mins == 1 {
		return "a minute"
	}
	return strconv.Itoa(mins) + " minutes"
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		s.renderLogin(w, "Enter a username and password.")
		return
	}
	addr := clientAddr(r)
	// Guessing is throttled before the hash is even computed, so a locked-out
	// attacker cannot use bcrypt's cost as a CPU amplifier either.
	if wait := s.throttle.retryAfter(addr, username); wait > 0 {
		s.store.Audit(username, "auth.login.throttled", addr)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		s.renderLoginBody(w, "Too many failed sign-in attempts. Try again in "+waitLabel(wait)+".")
		return
	}
	u, err := s.store.GetUserByUsername(username)
	if err != nil || !CheckPassword(u.PasswordHash, password) {
		s.throttle.failed(addr, username)
		s.store.Audit(username, "auth.login.failed", addr)
		s.renderLogin(w, "Wrong username or password.")
		return
	}
	s.throttle.succeeded(addr, username)
	tok, err := NewToken()
	if err != nil {
		s.renderLogin(w, "Could not create session, try again.")
		return
	}
	if _, err := s.store.CreateSession(HashToken(tok), u.ID, SessionTTL); err != nil {
		s.renderLogin(w, "Could not create session, try again.")
		return
	}
	// A successful login is the natural moment to drop stale rows: the sessions
	// table is append-only otherwise, and expiry alone never deleted anything.
	_ = s.store.DeleteExpiredSessions()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
	s.store.Audit(u.Username, "auth.login", "")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		_ = s.store.DeleteSession(HashToken(cookie.Value))
	}
	// The clearing cookie must carry the same attributes as the one it replaces,
	// or a browser that stored a Secure cookie keeps it: Set-Cookie only matches
	// on name/domain/path, but a Secure cookie is not overwritten by a
	// non-Secure one on an HTTPS origin, which left the (now deleted) session
	// token in the browser.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- setup (first admin) -------------------------------------------------

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if users, _ := s.store.ListUsers(); len(users) > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_, ok := s.cfg.SetupToken()
	if !ok {
		// no token file: either setup already done or misconfigured
		if users, _ := s.store.ListUsers(); len(users) == 0 {
			comp := web.Bare("Setup", web.Setup(true, "No setup token found — check /etc/cs2a/panel-setup-token."))
			_ = comp.Render(r.Context(), w)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	comp := web.Bare("Setup", web.Setup(false, ""))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render setup", "err", err)
	}
}

func (s *Server) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	users, _ := s.store.ListUsers()
	if len(users) > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	wantTok, ok := s.cfg.SetupToken()
	gotTok := strings.TrimSpace(r.FormValue("token"))
	// Constant-time: the setup token is the one secret that grants admin on a
	// brand-new install, and it is compared against attacker-supplied input.
	if !ok || !ConstantTimeEqual(gotTok, wantTok) {
		s.renderSetup(w, "Invalid setup token — check /etc/cs2a/panel-setup-token on the server.")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	steamID, _ := cs2.NormalizeSteamID(strings.TrimSpace(r.FormValue("steamid")))
	if l := len(username); l < 2 || l > 32 {
		s.renderSetup(w, "Username must be 2–32 characters.")
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		s.renderSetup(w, err.Error())
		return
	}
	if _, err := s.store.CreateUser(username, hash, "admin", steamID); err != nil {
		s.renderSetup(w, "Could not create admin: "+err.Error())
		return
	}
	s.cfg.ConsumeSetupToken()
	s.store.Audit(username, "setup.admin_created", "")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) renderSetup(w http.ResponseWriter, errMsg string) {
	comp := web.Bare("Setup", web.Setup(errMsg != "", errMsg))
	_ = comp.Render(context.Background(), w)
}

// --- helpers -------------------------------------------------------------

// navUser builds the layout nav model for the current user.
func navFor(u *User, active string) *web.NavUser {
	if u == nil {
		return nil
	}
	return &web.NavUser{Name: u.Username, Role: u.Role, SteamID: u.SteamID64, Active: active}
}

// flash reads the query-string flash message (PRG pattern).
func flash(r *http.Request) *web.Toast {
	ok := r.URL.Query().Get("ok")
	err := r.URL.Query().Get("err")
	switch {
	case ok != "":
		return &web.Toast{Kind: "ok", Message: ok}
	case err != "":
		return &web.Toast{Kind: "err", Message: err}
	default:
		return nil
	}
}

// redirectFlash redirects with a flash message.
func redirectFlash(w http.ResponseWriter, r *http.Request, path, kind, msg string) {
	target := path
	if u, err := url.Parse(path); err == nil {
		q := u.Query()
		q.Set(kind, msg)
		u.RawQuery = q.Encode()
		target = u.String()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

var panelVersion = version.Version
