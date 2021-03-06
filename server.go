package mpdsub

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fhs/gompd/mpd"
)

// A Server is a HTTP server which exposes an emulated Subsonic API in front
// of an MPD server.  It enables Subsonic clients to read information from
// MPD's database and stream files from the local filesystem.
type Server struct {
	db  database
	fs  filesystem
	cfg *Config
	ll  *log.Logger

	mux *http.ServeMux

	cancel context.CancelFunc
	wg     *sync.WaitGroup
}

// Config specifies configuration for a Server.
type Config struct {
	// Credentials which Subsonic clients must provide to authenticate
	// to the Server.
	SubsonicUser     string
	SubsonicPassword string

	// MusicDirectory specifies the root music directory for the MPD server.
	// This must match the value specified in MPD's configuration to enable
	// streaming media through the Server.
	//
	// TODO(mdlayher): perhaps enable parsing this via:
	//  - MPD 'config' command, if over UNIX socket
	//  - MPD configuration file
	MusicDirectory string

	// Verbose specifies if the server should enable verbose logging.
	Verbose bool

	// Keepalive specifies an optional duration for how often keepalive messages
	// should be sent to MPD from the Server.  If Keepalive is set to 0,
	// no keepalive messages will be sent to MPD.
	Keepalive time.Duration

	// Logger specifies an optional logger for the Server.  If Logger is
	// nil, Server logs will be sent to stdout.
	Logger *log.Logger
}

// NewServer creates a new Server using the input MPD client and Config.
func NewServer(c *mpd.Client, cfg *Config) *Server {
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stdout, "", log.Ldate|log.Ltime)
	}

	return newServer(c, &osFilesystem{}, cfg)
}

// newServer is the internal constructor for Server.  It enables swapping in
// arbitrary database implementations for testing.  It also sets up all Subsonic
// API routes.
func newServer(db database, fs filesystem, cfg *Config) *Server {
	s := &Server{
		db:  db,
		fs:  fs,
		cfg: cfg,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/rest/getLicense.view", s.getLicense)
	mux.HandleFunc("/rest/getIndexes.view", s.getIndexes)
	mux.HandleFunc("/rest/getMusicDirectory.view", s.getMusicDirectory)
	mux.HandleFunc("/rest/getMusicFolders.view", s.getMusicFolders)
	mux.HandleFunc("/rest/ping.view", s.ping)
	mux.HandleFunc("/rest/stream.view", s.stream)

	s.mux = mux

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg = new(sync.WaitGroup)

	if cfg.Keepalive > 0 {
		s.wg.Add(1)
		go s.keepalive(ctx)
	}

	return s
}

// keepalive sends keepalive messages to the database at regular intervals,
// to keep connections open.
func (s *Server) keepalive(ctx context.Context) {
	defer s.wg.Done()

	tick := time.NewTicker(s.cfg.Keepalive)
	for {
		if err := s.db.Ping(); err != nil {
			s.logf("failed to send keepalive message: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Close closes any background goroutines started by the Server, such as the
// keepalive functionality.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Verbose {
		s.logf("%s -> %s %s", r.RemoteAddr, r.Method, r.URL.String())
	}

	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Connection", "close")

	rctx, ok := parseRequestContext(r)
	if !ok {
		// Subsonic API returns HTTP 200 on missing parameters
		writeXML(w, errMissingParameter)
		return
	}

	if !s.authenticate(rctx) {
		// Subsonic API returns HTTP 200 on invalid authentication
		writeXML(w, errUnauthorized)
		return
	}

	s.mux.ServeHTTP(w, r)
}

// logf is a convenience function to create a formatted log entry using the
// Server's configured logger.
func (s *Server) logf(format string, v ...interface{}) {
	s.cfg.Logger.Printf(format, v...)
}

// An authMethod is an authentication method supported by the Server.
type authMethod int

const (
	// authMethodPassword is the legacy Subsonic authentication method,
	// using a username and password parameter with each request.
	authMethodPassword authMethod = iota

	// authMethodTokenSalt is the recommended Subsonic authentication method,
	// using a token and salt parameter with each request.
	authMethodTokenSalt
)

// authenticate attempts to authenticate a user using the input requestContext.
// It returns true if authentication is successful, or false if not.
func (s *Server) authenticate(rctx *requestContext) bool {
	if rctx.User != s.cfg.SubsonicUser {
		return false
	}

	switch rctx.authMethod {
	case authMethodPassword:
		return rctx.Password == s.cfg.SubsonicPassword
	case authMethodTokenSalt:
		// From Subsonic documentation:
		// http://www.subsonic.org/pages/api.jsp
		//   token = md5(password + salt)
		h := md5.New()
		_, _ = io.WriteString(h, s.cfg.SubsonicPassword+rctx.Salt)
		return rctx.Token == hex.EncodeToString(h.Sum(nil))
	default:
		return false
	}
}

// A requestContext is the requestContext for a request, parsed from the HTTP request.
type requestContext struct {
	User     string
	Password string
	Token    string
	Salt     string
	Client   string
	Version  string

	authMethod authMethod
}

// parseRequestContext parses parameters from a HTTP request into a requestContext.
// If any mandatory parameters are missing, it returns false.
func parseRequestContext(r *http.Request) (*requestContext, bool) {
	q := r.URL.Query()

	user := q.Get("u")
	if user == "" {
		return nil, false
	}

	client := q.Get("c")
	if client == "" {
		return nil, false
	}

	version := q.Get("v")
	if version == "" {
		return nil, false
	}

	// Password may be encoded, so transparently decode it, if needed
	pass := decodePassword(q.Get("p"))
	if pass != "" {
		// Password not empty, authenticate using password method
		return &requestContext{
			User:     user,
			Password: pass,
			Client:   client,
			Version:  version,

			authMethod: authMethodPassword,
		}, true
	}

	// If password was empty, check for newer token and salt method
	token := q.Get("t")
	if token == "" {
		return nil, false
	}

	salt := q.Get("s")
	if salt == "" {
		return nil, false
	}

	// Token and salt not empty, authenticate using token and salt method
	return &requestContext{
		User:    user,
		Token:   token,
		Salt:    salt,
		Client:  client,
		Version: version,

		authMethod: authMethodTokenSalt,
	}, true
}

// decodePassword decodes a password, if necessary, from its encoded hex
// format.  If the password is not encoded, the input string is returned.
func decodePassword(p string) string {
	const prefix = "enc:"

	if !strings.HasPrefix(p, prefix) {
		return p
	}

	// Treat invalid hex as "empty password"
	b, _ := hex.DecodeString(strings.TrimPrefix(p, prefix))
	return string(b)
}
