package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/mistyuk/worldzero/internal/kernel/auth"
	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// SessionCookie is the browser credential's name. Deliberately host-prefixed:
// __Host- means the browser will only accept it over HTTPS, scoped to this exact
// host, with Path=/ and no Domain attribute — so a subdomain cannot set or
// overwrite it. That closes off session fixation from a neighbouring host, which
// is otherwise the easiest way in when several apps share a domain.
const SessionCookie = "__Host-wz_session"

// devSessionCookie is used when TLS is absent, because browsers refuse __Host-
// cookies over plain HTTP and a local `docker compose up` would silently never
// log anyone in. That failure mode — a fresh install that cannot reach a working
// state — has already bitten this project once.
const devSessionCookie = "wz_session"

const (
	ctxPrincipal = "worldzero.principal"
)

// principalOf returns the authenticated caller, if any.
func principalOf(c *gin.Context) (auth.Principal, bool) {
	v, ok := c.Get(ctxPrincipal)
	if !ok {
		return auth.Principal{}, false
	}
	p, ok := v.(auth.Principal)
	return p, ok
}

// MustPrincipal is for handlers mounted behind authenticate: reaching one
// without a principal is a wiring bug, not a request problem.
func MustPrincipal(c *gin.Context) auth.Principal {
	p, ok := principalOf(c)
	if !ok {
		panic("worldzero: handler requires authentication but is not mounted behind it")
	}
	return p
}

// credential pulls the presented credential out of the request.
//
// Transport is BOUND to kind, and that binding is a security property rather
// than tidiness. A session cookie is sent automatically by the browser on every
// request to this origin — ambient authority — while a bearer token is only ever
// sent by something that deliberately attached it. If a session token were
// accepted in an Authorization header, or a bearer token honoured from a cookie,
// the two threat models would collapse into each other and CSRF reasoning would
// stop being sound.
//
// Presenting both is refused outright. It is never legitimate, and choosing one
// silently is how confused-deputy bugs start.
func credential(c *gin.Context) (raw string, fromCookie bool, err error) {
	header := c.GetHeader("Authorization")

	var cookie string
	if v, cerr := c.Cookie(SessionCookie); cerr == nil {
		cookie = v
	} else if v, cerr := c.Cookie(devSessionCookie); cerr == nil {
		cookie = v
	}

	switch {
	case header == "" && cookie == "":
		return "", false, werr.New(werr.Unauthenticated, "no credential presented")

	case header != "" && cookie != "":
		return "", false, werr.New(werr.Unauthenticated, "present exactly one credential")

	case header != "":
		const prefix = "Bearer "
		if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
			return "", false, werr.New(werr.Unauthenticated, "authorization must be a bearer token")
		}
		return header[len(prefix):], false, nil

	default:
		return cookie, true, nil
	}
}

// authenticate verifies the caller and puts the principal in the context.
func authenticate(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, fromCookie, err := credential(c)
		if err != nil {
			unauthorized(c, d, err)
			return
		}

		p, err := d.Auth.Authenticate(c.Request.Context(), d.DB.Pool(), raw)
		if err != nil {
			unauthorized(c, d, err)
			return
		}

		// Enforce the transport binding against what the credential actually is,
		// not against what the caller implied by where they put it.
		if fromCookie != (p.Kind == auth.KindSession) {
			unauthorized(c, d, werr.New(werr.Unauthenticated, "invalid or expired credential"))
			return
		}

		// ADR-005. A bearer token proves you HAVE a secret; a signature proves
		// this particular request is the one you meant to send — so a token
		// captured from a log or a proxy is not enough on its own.
		//
		// Only when the credential asks for it. The citizen generated its own
		// keypair, so it is the right party to decide whether a stolen token
		// should be usable, and a scripted bot that does not care pays nothing.
		if p.RequiresSignature {
			if err := d.verifySignature(c, p); err != nil {
				unauthorized(c, d, err)
				return
			}
		}

		c.Set(ctxPrincipal, p)
		if p.AgentID != "" {
			// errors.go logs this on every rejection, which is what turns
			// ChaosBot's rejects into an audit trail.
			c.Set(ctxAgentID, p.AgentID)
		}
		c.Next()
	}
}

// unauthorized always sets WWW-Authenticate, so a client can tell a dead
// credential from a refusal without parsing prose.
func unauthorized(c *gin.Context, d Deps, err error) {
	if werr.CodeOf(err) == werr.Unauthenticated {
		c.Header("WWW-Authenticate", `Bearer realm="worldzero"`)
	}
	fail(c, d.Logger, err)
}

// requireScope gates a route on a capability.
//
// Scope is checked here rather than inside handlers so that adding a route
// without deciding its authority is a compile-time omission someone notices,
// not a silently public endpoint.
func requireScope(d Deps, s auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := MustPrincipal(c)
		if !p.Allows(s) {
			fail(c, d.Logger, werr.New(werr.InsufficientScope,
				"this credential does not carry the "+string(s)+" capability"))
			return
		}
		c.Next()
	}
}

// requireHuman restricts a route to account holders.
//
// Written as a positive allow-list, NOT as "not an agent". The negation is
// subtly dangerous: it makes every credential kind that does not yet exist a
// human principal by default, so the next kind added — a service credential, a
// foundation-agent credential — silently inherits access to every human route
// until someone remembers to exclude it. An allow-list fails closed instead.
//
// This is the mechanical form of the boundary ADR-015 draws: agent principals
// reach the actions endpoint and read surfaces, and nothing else. An agent that
// could mint credentials or claim citizens could grow its own authority, which
// is the one thing the whole scope model exists to prevent.
func requireHuman(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch MustPrincipal(c).Kind {
		case auth.KindSession, auth.KindUserKey:
			c.Next()
		default:
			fail(c, d.Logger, werr.New(werr.Forbidden, "only an account holder may do that"))
		}
	}
}

// requireAgent restricts a route to citizens, also as an allow-list.
func requireAgent(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if MustPrincipal(c).Kind != auth.KindAgentKey {
			fail(c, d.Logger, werr.New(werr.Forbidden, "only a citizen may do that"))
			return
		}
		c.Next()
	}
}

// setSessionCookie issues the browser credential.
//
// Secure is set only when the request arrived over TLS, and the cookie name
// changes with it, because a browser silently drops a __Host- cookie sent over
// plain HTTP — which would make local development look like a broken login
// rather than a missing certificate.
func setSessionCookie(c *gin.Context, token string, maxAgeSeconds int) {
	name, secure := devSessionCookie, false
	if isTLS(c) {
		name, secure = SessionCookie, true
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAgeSeconds,
		HttpOnly: true, // script cannot read it, so XSS cannot exfiltrate it
		Secure:   secure,
		SameSite: http.SameSiteLaxMode, // top-level navigations still work; cross-site POSTs do not
	})
}

func clearSessionCookie(c *gin.Context) {
	for _, name := range []string{SessionCookie, devSessionCookie} {
		http.SetCookie(c.Writer, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: isTLS(c), SameSite: http.SameSiteLaxMode,
		})
	}
}

func isTLS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	// Only consulted when a proxy is trusted; SetTrustedProxies(nil) means a
	// direct caller cannot fake this (see NewRouter).
	return strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}

// verifySignature checks a signed request, reading the body so the hash covers
// what the handler will actually see.
//
// The body is restored afterwards: consuming it here and leaving the handler
// with an empty reader would make every signed request silently fail to parse.
func (d Deps) verifySignature(c *gin.Context, p auth.Principal) error {
	var body []byte
	if c.Request.Body != nil {
		read, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return werr.New(werr.InvalidParams, "could not read the request body")
		}
		body = read
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
	}

	req := auth.SignedRequest{
		Method:    c.Request.Method,
		Path:      c.Request.URL.RequestURI(),
		Timestamp: c.GetHeader(auth.HeaderTimestamp),
		Nonce:     c.GetHeader(auth.HeaderNonce),
		Signature: c.GetHeader(auth.HeaderSignature),
		Body:      body,
	}

	// The nonce burn is a write, so it needs a transaction of its own — it must
	// commit even when the action that follows does not, or a failed action
	// would hand its signature back for reuse.
	return d.DB.Tx(c.Request.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return auth.VerifySignature(ctx, tx, d.Hasher, p.PublicKey, p.AgentID, req, d.Clock.Real())
	})
}
