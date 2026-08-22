package remote

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"iar/internal/accounts"
)

// userRow is one account on the Users page.
type userRow struct {
	accounts.User
	// Link is the account's pending invite or reset link, if any.
	Link *accounts.Link
	// Me marks the admin's own row (no self-delete control).
	Me bool
}

// usersData feeds the Users page.
type usersData struct {
	Nav     navData
	Users   []userRow
	Pending []accounts.Link
	Flash   flash
	Email   bool // whether links are also emailed
}

func (s *Server) handleUsersPage(w http.ResponseWriter, r *http.Request, u accounts.User) {
	links := s.users.Links()
	byEmail := map[string]*accounts.Link{}
	for i := range links {
		byEmail[links[i].Email] = &links[i]
	}
	var rows []userRow
	for _, acct := range s.users.List() {
		rows = append(rows, userRow{User: acct, Link: byEmail[acct.Email], Me: acct.Email == u.Email})
	}
	fl, _ := s.flashes.take(flashKey(r))
	s.render(w, http.StatusOK, "users.html", usersData{
		Nav: s.nav(u, "users"), Users: rows, Pending: links, Flash: fl, Email: s.emailConfigured(),
	})
}

// permsFromForm reads the four permission checkboxes.
func permsFromForm(r *http.Request) accounts.Permissions {
	on := func(name string) bool { return r.PostFormValue(name) != "" }
	return accounts.Permissions{Admin: on("admin"), Steer: on("steer"), NewPrompt: on("new_prompt"), Save: on("save")}
}

// finish stores the flash and returns to the Users page.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, fl flash) {
	s.flashes.put(flashKey(r), fl)
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

// linkBase is the origin the admin's browser is using, so the links it
// hands out are reachable by anyone on the same route in.
func linkBase(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// issueLink creates a fresh link for email, emails it when a mail server
// is configured, and builds the flash describing the outcome.
func (s *Server) issueLink(r *http.Request, actor accounts.User, email string, fl flash) flash {
	secret, link, err := s.users.IssueLink(email)
	if err != nil {
		fl.Error = err.Error()
		return fl
	}
	full := linkBase(r) + "/set-password/" + secret
	fl.Link = full
	fl.LinkEmail = link.Email
	fl.LinkKind = string(link.Kind)
	fl.LinkExpires = link.Expires
	if s.emailConfigured() {
		ctx, cancel := withTimeout(r.Context(), 20*time.Second)
		defer cancel()
		subject, body := linkMessage(link, full)
		if err := s.mailer.Send(ctx, link.Email, subject, body); err != nil {
			fl.EmailError = err.Error()
			s.log.Warn("link email failed", "event", "remote_link_email_failed", "user", link.Email, "error", err.Error())
		} else {
			fl.Emailed = true
		}
	}
	s.log.Info("remote link issued", "event", "remote_link_issued", "by", actor.Email, "user", link.Email,
		"kind", string(link.Kind), "emailed", fl.Emailed)
	return fl
}

// linkMessage composes the plain-text email for a link.
func linkMessage(link accounts.Link, full string) (subject, body string) {
	var b strings.Builder
	switch link.Kind {
	case accounts.LinkInvite:
		subject = "Your " + ProductName + " invitation"
		fmt.Fprintf(&b, "You have been invited to listen to %s.\n\n", ProductName)
		fmt.Fprintf(&b, "Open this link to choose your password and log in:\n\n%s\n\n", full)
		fmt.Fprintf(&b, "Your login is %s. The link works once and expires in %s.\n", link.Email, fmtUntil(link.Expires))
	default:
		subject = ProductName + " password reset"
		fmt.Fprintf(&b, "A password reset was requested for your %s account (%s).\n\n", ProductName, link.Email)
		fmt.Fprintf(&b, "Open this link to choose a new password:\n\n%s\n\n", full)
		fmt.Fprintf(&b, "The link works once and expires in %s. If you did not ask for this, ignore it.\n", fmtUntil(link.Expires))
	}
	return subject, b.String()
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request, u accounts.User) {
	email := textField(r, "email")
	acct, err := s.users.Create(email, permsFromForm(r))
	if err != nil {
		s.finish(w, r, flash{Error: err.Error()})
		return
	}
	s.log.Info("remote user created", "event", "remote_user_created", "by", u.Email, "user", acct.Email, "perms", acct.Perms)
	fl := s.issueLink(r, u, acct.Email, flash{Message: "Account created for " + acct.Email + "."})
	s.finish(w, r, fl)
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request, u accounts.User) {
	email := textField(r, "email")
	perms := permsFromForm(r)
	if err := s.users.SetPermissions(email, perms); err != nil {
		s.finish(w, r, flash{Error: err.Error()})
		return
	}
	s.log.Info("remote user updated", "event", "remote_user_updated", "by", u.Email, "user", strings.ToLower(email), "perms", perms)
	s.finish(w, r, flash{Message: "Permissions saved for " + strings.ToLower(email) + "."})
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request, u accounts.User) {
	email := textField(r, "email")
	if err := s.users.Delete(u.Email, email); err != nil {
		s.finish(w, r, flash{Error: err.Error()})
		return
	}
	s.sessions.RevokeUser(strings.ToLower(email), "")
	s.log.Info("remote user deleted", "event", "remote_user_deleted", "by", u.Email, "user", strings.ToLower(email))
	s.finish(w, r, flash{Message: "Deleted " + strings.ToLower(email) + "."})
}

// handleUserLink issues (or regenerates) an invite or reset link.
func (s *Server) handleUserLink(w http.ResponseWriter, r *http.Request, u accounts.User) {
	email := textField(r, "email")
	s.finish(w, r, s.issueLink(r, u, email, flash{}))
}

func (s *Server) handleUserRevokeLink(w http.ResponseWriter, r *http.Request, u accounts.User) {
	email := textField(r, "email")
	if err := s.users.RevokeLink(email); err != nil {
		s.finish(w, r, flash{Error: err.Error()})
		return
	}
	s.log.Info("remote link revoked", "event", "remote_link_revoked", "by", u.Email, "user", strings.ToLower(email))
	s.finish(w, r, flash{Message: "Link for " + strings.ToLower(email) + " revoked."})
}
