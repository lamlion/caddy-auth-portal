package portal

import (
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/greenpau/caddy-auth-jwt"
	"github.com/greenpau/caddy-auth-portal/pkg/cache"
	"github.com/greenpau/caddy-auth-portal/pkg/cookies"
	"github.com/greenpau/caddy-auth-portal/pkg/handlers"
	"github.com/greenpau/caddy-auth-portal/pkg/registration"
	"github.com/greenpau/caddy-auth-portal/pkg/ui"
	"github.com/greenpau/caddy-auth-portal/pkg/utils"
	"github.com/greenpau/go-identity"
	"github.com/satori/go.uuid"
	"go.uber.org/zap"
)

const (
	redirectToToken = "AUTH_PORTAL_REDIRECT_URL"
)

// PortalPool is the global authentication provider pool.
// It provides access to all instances of authentication portal plugin.
var PortalPool *AuthPortalPool

var sessionCache *cache.SessionCache

func init() {
	sessionCache = cache.NewSessionCache()
	PortalPool = &AuthPortalPool{}
	caddy.RegisterModule(AuthPortal{})
}

// AuthPortal implements Form-Based, Basic, Local, LDAP,
// OpenID Connect, OAuth 2.0, SAML Authentication.
type AuthPortal struct {
	Name                     string                     `json:"-"`
	Provisioned              bool                       `json:"-"`
	ProvisionFailed          bool                       `json:"-"`
	PrimaryInstance          bool                       `json:"primary,omitempty"`
	Context                  string                     `json:"context,omitempty"`
	AuthURLPath              string                     `json:"auth_url_path,omitempty"`
	UserInterface            *UserInterfaceParameters   `json:"ui,omitempty"`
	UserRegistration         *registration.Registration `json:"registration,omitempty"`
	UserRegistrationDatabase *identity.Database         `json:"-"`
	Cookies                  *cookies.Cookies           `json:"cookies,omitempty"`
	Backends                 []Backend                  `json:"backends,omitempty"`
	TokenProvider            *jwt.CommonTokenConfig     `json:"jwt,omitempty"`
	EnableSourceIPTracking   bool                       `json:"source_ip_tracking,omitempty"`
	TokenValidator           *jwt.TokenValidator        `json:"-"`
	logger                   *zap.Logger
	uiFactory                *ui.UserInterfaceFactory
	startedAt                time.Time
	loginOptions             map[string]interface{}
}

// CaddyModule returns the Caddy module information.
func (AuthPortal) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.auth_portal",
		New: func() caddy.Module { return new(AuthPortal) },
	}
}

// Provision provisions authentication portal provider
func (m *AuthPortal) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger(m)
	m.startedAt = time.Now().UTC()
	if err := PortalPool.Register(m); err != nil {
		return fmt.Errorf(
			"authentication provider registration error, instance %s, error: %s",
			m.Name, err,
		)
	}
	if !m.PrimaryInstance {
		if err := PortalPool.Provision(m.Name); err != nil {
			return fmt.Errorf(
				"authentication provider provisioning error, instance %s, error: %s",
				m.Name, err,
			)
		}
	}
	m.logger.Info(
		"provisioned plugin instance",
		zap.String("instance_name", m.Name),
		zap.Time("started_at", m.startedAt),
	)
	return nil
}

// Validate implements caddy.Validator.
func (m *AuthPortal) Validate() error {
	m.logger.Info(
		"validated plugin instance",
		zap.String("instance_name", m.Name),
	)
	return nil
}

// ServeHTTP authorizes access based on the presense and content of JWT token.
func (m AuthPortal) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	reqID := GetRequestID(r)
	log := m.logger
	opts := make(map[string]interface{})
	opts["request_id"] = reqID
	opts["content_type"] = utils.GetContentType(r)
	opts["authenticated"] = false
	opts["auth_backend_found"] = false
	opts["auth_credentials_found"] = false
	opts["logger"] = log
	opts["auth_url_path"] = m.AuthURLPath
	opts["ui"] = m.uiFactory
	opts["cookies"] = m.Cookies
	opts["cookie_names"] = []string{redirectToToken, m.TokenProvider.TokenName}
	opts["token_provider"] = m.TokenProvider
	if m.UserInterface.Title != "" {
		opts["ui_title"] = m.UserInterface.Title
	}
	opts["redirect_token_name"] = redirectToToken

	urlPath := strings.TrimPrefix(r.URL.Path, m.AuthURLPath)
	urlPath = strings.TrimPrefix(urlPath, "/")

	// Find JWT tokens, if any, and validate them.
	if claims, authOK, err := m.TokenValidator.Authorize(r, nil); authOK {
		opts["authenticated"] = true
		opts["user_claims"] = claims
	} else {
		if err != nil {
			switch err.Error() {
			case "[Token is expired]":
				return handlers.ServeSessionLoginRedirect(w, r, opts)
			case "no token found":
			default:
				log.Warn("Authorization failed",
					zap.String("request_id", opts["request_id"].(string)),
					zap.Any("error", err.Error()),
					zap.String("src_ip_address", utils.GetSourceAddress(r)),
				)
			}
		}
	}

	// Handle requests based on query parameters.
	if r.Method == "GET" {
		q := r.URL.Query()
		foundQueryOptions := false
		if redirectURL, exists := q["redirect_url"]; exists {
			w.Header().Set("Set-Cookie", redirectToToken+"="+redirectURL[0]+";"+m.Cookies.GetAttributes())
			foundQueryOptions = true
		}
		if !strings.HasPrefix(urlPath, "saml") && !strings.HasPrefix(urlPath, "x509") && !strings.HasPrefix(urlPath, "oauth2") {
			if foundQueryOptions {
				w.Header().Set("Location", m.AuthURLPath)
				w.WriteHeader(302)
				return nil
			}
		}
	}

	// Perform request routing
	switch {
	case strings.HasPrefix(urlPath, "register"):
		if m.UserRegistration.Disabled {
			opts["flow"] = "unsupported_feature"
			return handlers.ServeGeneric(w, r, opts)
		}
		if m.UserRegistration.Dropbox == "" {
			opts["flow"] = "unsupported_feature"
			return handlers.ServeGeneric(w, r, opts)
		}
		opts["flow"] = "register"
		opts["registration"] = m.UserRegistration
		opts["registration_db"] = m.UserRegistrationDatabase
		return handlers.ServeRegister(w, r, opts)
	case strings.HasPrefix(urlPath, "recover"),
		strings.HasPrefix(urlPath, "forgot"):
		// opts["flow"] = "recover"
		opts["flow"] = "unsupported_feature"
		return handlers.ServeGeneric(w, r, opts)
	case strings.HasPrefix(urlPath, "logout"),
		strings.HasPrefix(urlPath, "logoff"):
		opts["flow"] = "logout"
		return handlers.ServeSessionLogoff(w, r, opts)
	case strings.HasPrefix(urlPath, "assets"):
		opts["flow"] = "assets"
		return handlers.ServeStaticAssets(w, r, opts)
	case strings.HasPrefix(urlPath, "whoami"):
		opts["flow"] = "whoami"
		return handlers.ServeWhoami(w, r, opts)
	case strings.HasPrefix(urlPath, "settings"):
		opts["flow"] = "settings"
		return handlers.ServeSettings(w, r, opts)
	case strings.HasPrefix(urlPath, "portal"):
		opts["flow"] = "portal"
		return handlers.ServePortal(w, r, opts)
	case strings.HasPrefix(urlPath, "saml"), strings.HasPrefix(urlPath, "x509"), strings.HasPrefix(urlPath, "oauth2"):
		urlPathParts := strings.Split(urlPath, "/")
		if len(urlPathParts) < 2 {
			opts["status_code"] = 400
			opts["flow"] = "malformed_backend"
			opts["authenticated"] = false
			return handlers.ServeGeneric(w, r, opts)
		}
		reqBackendMethod := urlPathParts[0]
		reqBackendRealm := urlPathParts[1]
		opts["flow"] = reqBackendMethod
		for _, backend := range m.Backends {
			if backend.GetRealm() != reqBackendRealm {
				continue
			}
			if backend.GetMethod() != reqBackendMethod {
				continue
			}
			opts["request"] = r
			opts["request_path"] = path.Join(m.AuthURLPath, reqBackendMethod, reqBackendRealm)
			resp, err := backend.Authenticate(opts)
			if err != nil {
				opts["flow"] = "auth_failed"
				opts["authenticated"] = false
				opts["message"] = "Authentication failed"
				opts["status_code"] = resp["code"].(int)
				log.Warn("Authentication failed",
					zap.String("request_id", reqID),
					zap.String("auth_method", reqBackendMethod),
					zap.String("auth_realm", reqBackendRealm),
					zap.String("error", err.Error()),
				)
				return handlers.ServeGeneric(w, r, opts)
			}
			if v, exists := resp["redirect_url"]; exists {
				// Redirect to external provider
				http.Redirect(w, r, v.(string), http.StatusPermanentRedirect)
				return nil
			}
			if _, exists := resp["claims"]; !exists {
				opts["flow"] = "auth_failed"
				opts["authenticated"] = false
				opts["message"] = "Authentication failed"
				opts["status_code"] = resp["code"].(int)
				log.Warn("Authentication failed",
					zap.String("request_id", reqID),
					zap.String("auth_method", reqBackendMethod),
					zap.String("auth_realm", reqBackendRealm),
					zap.String("error", err.Error()),
				)
				return handlers.ServeGeneric(w, r, opts)
			}

			claims := resp["claims"].(*jwt.UserClaims)
			claims.Issuer = utils.GetCurrentURL(r)
			if m.EnableSourceIPTracking {
				claims.Address = utils.GetSourceAddress(r)
			}
			if claims.ID == "" {
				claims.ID = reqID
			}
			sessionCache.Add(claims.ID, map[string]interface{}{
				"claims":         claims,
				"backend_name":   backend.GetName(),
				"backend_realm":  backend.GetRealm(),
				"backend_method": backend.GetMethod(),
			})
			opts["authenticated"] = true
			opts["user_claims"] = claims
			opts["status_code"] = 200
			log.Debug("Authentication succeeded",
				zap.String("request_id", reqID),
				zap.String("auth_method", reqBackendMethod),
				zap.String("auth_realm", reqBackendRealm),
				zap.Any("user", claims),
			)
			return handlers.ServeLogin(w, r, opts)
		}
		opts["status_code"] = 400
		opts["flow"] = "backend_not_found"
		opts["authenticated"] = false
		return handlers.ServeGeneric(w, r, opts)
	case strings.HasPrefix(urlPath, "login"), urlPath == "":
		opts["flow"] = "login"
		opts["login_options"] = m.loginOptions
		if opts["authenticated"].(bool) {
			opts["authorized"] = true
		} else {
			// Authenticating the request
			if credentials, err := utils.ParseCredentials(r); err == nil {
				if credentials != nil {
					opts["auth_credentials_found"] = true
					for _, backend := range m.Backends {
						if backend.GetRealm() != credentials["realm"] {
							continue
						}
						opts["auth_backend_found"] = true
						opts["auth_credentials"] = credentials
						if resp, err := backend.Authenticate(opts); err != nil {
							opts["message"] = "Authentication failed"
							opts["status_code"] = resp["code"].(int)
							log.Warn("Authentication failed",
								zap.String("request_id", reqID),
								zap.String("error", err.Error()),
							)
						} else {
							claims := resp["claims"].(*jwt.UserClaims)
							claims.Issuer = utils.GetCurrentURL(r)
							if m.EnableSourceIPTracking {
								claims.Address = utils.GetSourceAddress(r)
							}
							if claims.ID == "" {
								claims.ID = reqID
							}
							sessionCache.Add(claims.ID, map[string]interface{}{
								"claims":         claims,
								"backend_name":   backend.GetName(),
								"backend_realm":  backend.GetRealm(),
								"backend_method": backend.GetMethod(),
							})
							opts["user_claims"] = claims
							opts["authenticated"] = true
							opts["status_code"] = 200
							log.Debug("Authentication succeeded",
								zap.String("request_id", reqID),
								zap.Any("user", claims),
							)
						}
					}
					if !opts["auth_backend_found"].(bool) {
						opts["status_code"] = 500
						log.Warn("Authentication failed",
							zap.String("request_id", reqID),
							zap.String("error", "no matching auth backend found"),
						)
					}
				}
			} else {
				opts["message"] = "Authentication failed"
				opts["status_code"] = 400
				log.Warn("Authentication failed",
					zap.String("request_id", reqID),
					zap.String("error", err.Error()),
				)
			}
		}
		return handlers.ServeLogin(w, r, opts)
	default:
		opts["flow"] = "not_found"
		return handlers.ServeGeneric(w, r, opts)
	}
}

// GetRequestID returns request ID.
func GetRequestID(r *http.Request) string {
	rawRequestID := caddyhttp.GetVar(r.Context(), "request_id")
	if rawRequestID == nil {
		requestID := uuid.NewV4().String()
		caddyhttp.SetVar(r.Context(), "request_id", requestID)
		return requestID
	}
	return rawRequestID.(string)
}

// Interface guards
var (
	_ caddy.Provisioner           = (*AuthPortal)(nil)
	_ caddy.Validator             = (*AuthPortal)(nil)
	_ caddyhttp.MiddlewareHandler = (*AuthPortal)(nil)
)
