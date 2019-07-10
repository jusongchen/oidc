package oidcserver

import (
	"context"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/heroku/deci/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"gopkg.in/square/go-jose.v2"
)

// LocalConnector is the local passwordDB connector which is an internal
// connector maintained by the server.
const LocalConnector = "local"

// Signer is used for signing the identity tokens
type Signer interface {
	// PublicKeys returns a keyset of all valid signer public keys considered
	// valid for signed tokens
	PublicKeys(ctx context.Context) (*jose.JSONWebKeySet, error)
	// SignerAlg returns the algorithm the signer uses
	SignerAlg(ctx context.Context) (jose.SignatureAlgorithm, error)
	// Sign the provided data
	Sign(ctx context.Context, data []byte) (signed []byte, err error)
	// VerifySignature verifies the signature given token against the current signers
	VerifySignature(ctx context.Context, jwt string) (payload []byte, err error)
}

// ClientSource can be queried to get information about an oauth2 client.
type ClientSource interface {
	// GetClient returns information about the given client ID. It will be
	// called for each lookup. If the client is not found but no other error
	// occurred, an ErrNoSuchClient should be returned
	GetClient(id string) (*Client, error)
}

// ErrNoSuchClient indicates that the requested client does not exist
type ErrNoSuchClient interface {
	NoSuchClient()
}

func isNoSuchClientErr(err error) bool {
	_, ok := err.(ErrNoSuchClient)
	return ok
}

// Client represents an OAuth2 client.
//
// For further reading see:
//   * Trusted peers: https://developers.google.com/identity/protocols/CrossClientAuth
//   * Public clients: https://developers.google.com/api-client-library/python/auth/installed-app
type Client struct {
	// Client ID and secret used to identify the client.
	ID     string `json:"id" yaml:"id"`
	Secret string `json:"secret" yaml:"secret"`

	// A registered set of redirect URIs. When redirecting from dex to the client, the URI
	// requested to redirect to MUST match one of these values, unless the client is "public".
	RedirectURIs []string `json:"redirectURIs" yaml:"redirectURIs"`

	// TrustedPeers are a list of peers which can issue tokens on this client's behalf using
	// the dynamic "oauth2:server:client_id:(client_id)" scope. If a peer makes such a request,
	// this client's ID will appear as the ID Token's audience.
	//
	// Clients inherently trust themselves.
	TrustedPeers []string `json:"trustedPeers" yaml:"trustedPeers"`

	// Public clients must use either use a redirectURL 127.0.0.1:X or "urn:ietf:wg:oauth:2.0:oob"
	Public bool `json:"public" yaml:"public"`

	// Name and LogoURL used when displaying this client to the end user.
	Name    string `json:"name" yaml:"name"`
	LogoURL string `json:"logoURL" yaml:"logoURL"`
}

// Server is the top level object.
type Server struct {
	issuerURL url.URL

	// Map of connector IDs to connectors.
	connectors map[string]Connector

	clients ClientSource

	storage storage.Storage

	mux http.Handler

	templates *templates

	// If enabled, don't prompt user for approval after logging in through connector.
	skipApproval bool

	supportedResponseTypes map[string]bool

	now func() time.Time

	authRequestsValidFor time.Duration
	idTokensValidFor     time.Duration

	signer Signer

	logger logrus.FieldLogger

	registry *prometheus.Registry

	allowedOrigins []string
}

// ServerOption defines optional configuration items for the OIDC server.
type ServerOption func(s *Server) error

// WithSupportedResponseTypes valid values are "code" to enable the code flow
// and "token" to enable the implicit flow. If no response types are supplied
// this value defaults to "code".
func WithSupportedResponseTypes(responseTypes []string) ServerOption {
	return ServerOption(func(s *Server) error {
		supported := make(map[string]bool)
		for _, respType := range responseTypes {
			switch respType {
			case responseTypeCode, responseTypeIDToken, responseTypeToken:
			default:
				return fmt.Errorf("unsupported response_type %q", respType)
			}
			supported[respType] = true
		}
		s.supportedResponseTypes = supported
		return nil
	})
}

// WithIDTokenValidity sets how long issued ID tokens are valid for
func WithIDTokenValidity(validFor time.Duration) ServerOption {
	return ServerOption(func(s *Server) error {
		s.idTokensValidFor = validFor
		return nil
	})
}

// WithAuthRequestValidity sets how long an authorization flow is considered
// valid.
func WithAuthRequestValidity(validFor time.Duration) ServerOption {
	return ServerOption(func(s *Server) error {
		s.authRequestsValidFor = validFor
		return nil
	})
}

// WithSkipApprovalScreen can be used to set skipping the approval screen on a
// global level
func WithSkipApprovalScreen(skip bool) ServerOption {
	return ServerOption(func(s *Server) error {
		s.skipApproval = skip
		return nil
	})
}

// WithLogger sets a logger on the server, otherwise no output will be logged
func WithLogger(logger logrus.FieldLogger) ServerOption {
	return ServerOption(func(s *Server) error {
		s.logger = logger
		return nil
	})
}

// WithTemplates will use the provided template items for rendering these pages,
// over the built-in. See ./web/templates for examples.
func WithTemplates(loginTemplate, approvalTemplate, oobTemplate, errorTemplate *template.Template) ServerOption {
	return ServerOption(func(s *Server) error {
		tmpls, err := loadTemplates(s.issuerURL.String(), "", s.issuerURL.String(), loginTemplate, approvalTemplate, oobTemplate, errorTemplate)
		if err != nil {
			return fmt.Errorf("server: failed to load web templates: %v", err)
		}
		s.templates = tmpls
		return nil
	})
}

func WithPrometheusRegistry(registry *prometheus.Registry) ServerOption {
	return ServerOption(func(s *Server) error {
		s.registry = registry
		return nil
	})
}

// WithAllowedOrigins is a List of allowed origins for CORS requests on
// discovery, token and keys endpoint. If none are indicated, CORS requests are
// disabled. Passing in "*" will allow any domain.
func WithAllowedOrigins(origins []string) ServerOption {
	return ServerOption(func(s *Server) error {
		s.allowedOrigins = append(s.allowedOrigins, origins...)
		return nil
	})
}

func New(issuer string, storage storage.Storage, signer Signer, connectors map[string]Connector, clients ClientSource, opts ...ServerOption) (*Server, error) {
	issURL, err := url.Parse(issuer)
	if err != nil {
		return nil, fmt.Errorf("server: can't parse issuer URL")
	}

	logger := logrus.New()
	logger.Out = ioutil.Discard

	reg := prometheus.NewRegistry()

	s := &Server{
		issuerURL:              *issURL,
		connectors:             connectors,
		storage:                storage,
		idTokensValidFor:       24 * time.Hour,
		authRequestsValidFor:   24 * time.Hour,
		now:                    time.Now,
		logger:                 logger,
		registry:               reg,
		supportedResponseTypes: map[string]bool{responseTypeCode: true},
		signer:                 signer,
		clients:                clients,
	}

	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}

	if s.templates == nil {
		tmpls, err := loadTemplates(s.issuerURL.String(), "", s.issuerURL.String(), nil, nil, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("server: failed to load web templates: %v", err)
		}
		s.templates = tmpls
	}

	requestCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Count of all HTTP requests.",
	}, []string{"handler", "code", "method"})

	err = s.registry.Register(requestCounter)
	if err != nil {
		return nil, fmt.Errorf("server: Failed to register Prometheus HTTP metrics: %v", err)
	}

	instrumentHandlerCounter := func(handlerName string, handler http.Handler) http.HandlerFunc {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := httpsnoop.CaptureMetrics(handler, w, r)
			requestCounter.With(prometheus.Labels{"handler": handlerName, "code": strconv.Itoa(m.Code), "method": r.Method}).Inc()
		})
	}

	r := mux.NewRouter()
	handle := func(p string, h http.Handler) {
		r.Handle(path.Join(s.issuerURL.Path, p), instrumentHandlerCounter(p, h))
	}
	handleFunc := func(p string, h http.HandlerFunc) {
		handle(p, h)
	}
	handlePrefix := func(p string, h http.Handler) {
		prefix := path.Join(s.issuerURL.Path, p)
		r.PathPrefix(prefix).Handler(http.StripPrefix(prefix, h))
	}
	handleWithCORS := func(p string, h http.HandlerFunc) {
		var handler http.Handler = h
		if len(s.allowedOrigins) > 0 {
			corsOption := handlers.AllowedOrigins(s.allowedOrigins)
			handler = handlers.CORS(corsOption)(handler)
		}
		r.Handle(path.Join(s.issuerURL.Path, p), instrumentHandlerCounter(p, handler))
	}
	r.NotFoundHandler = http.HandlerFunc(http.NotFound)

	discoveryHandler, err := s.discoveryHandler()
	if err != nil {
		return nil, err
	}
	handleWithCORS("/.well-known/openid-configuration", discoveryHandler)

	// TODO(ericchiang): rate limit certain paths based on IP.
	handleWithCORS("/token", s.handleToken)
	handleWithCORS("/keys", s.handlePublicKeys)
	handleWithCORS("/userinfo", s.handleUserInfo)
	handleFunc("/auth", s.handleAuthorization)
	handleFunc("/auth/{connector}", s.handleConnectorLogin)
	handleFunc("/approval", s.handleApproval)
	handlePrefix("/static", http.FileServer(webStatic))
	s.mux = r

	if err := s.initConnectors(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Server) initConnectors() error {
	for _, c := range s.connectors {
		if err := c.Initialize(&authenticator{s: s}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) absPath(pathItems ...string) string {
	paths := make([]string, len(pathItems)+1)
	paths[0] = s.issuerURL.Path
	copy(paths[1:], pathItems)
	return path.Join(paths...)
}

func (s *Server) absURL(pathItems ...string) string {
	u := s.issuerURL
	u.Path = s.absPath(pathItems...)
	return u.String()
}