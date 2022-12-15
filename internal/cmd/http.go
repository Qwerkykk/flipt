package cmd

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/fatih/color"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.flipt.io/flipt/internal/config"
	"go.flipt.io/flipt/internal/gateway"
	"go.flipt.io/flipt/internal/info"
	"go.flipt.io/flipt/rpc/flipt"
	"go.flipt.io/flipt/swagger"
	"go.flipt.io/flipt/ui"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// HTTPServer is a wrapper around the construction and registration of Flipt's HTTP server.
type HTTPServer struct {
	*http.Server

	logger *zap.Logger

	listenAndServe func() error
}

// NewHTTPServer constructs and configures the HTTPServer instance.
// The HTTPServer depends upon a running gRPC server instance which is why
// it explicitly requires and established gRPC connection as an argument.
func NewHTTPServer(
	ctx context.Context,
	logger *zap.Logger,
	cfg *config.Config,
	conn *grpc.ClientConn,
	info info.Flipt,
) (*HTTPServer, error) {
	logger = logger.With(zap.Stringer("server", cfg.Server.Protocol))

	var (
		server = &HTTPServer{
			logger: logger,
		}
		isConsole = cfg.Log.Encoding == config.LogEncodingConsole

		r        = chi.NewRouter()
		api      = gateway.NewGatewayServeMux()
		httpPort = cfg.Server.HTTPPort
	)

	if cfg.Server.Protocol == config.HTTPS {
		httpPort = cfg.Server.HTTPSPort
	}

	if err := flipt.RegisterFliptHandler(ctx, api, conn); err != nil {
		return nil, fmt.Errorf("registering grpc gateway: %w", err)
	}

	if cfg.Cors.Enabled {
		cors := cors.New(cors.Options{
			AllowedOrigins:   cfg.Cors.AllowedOrigins,
			AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
			AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
			ExposedHeaders:   []string{"Link"},
			AllowCredentials: true,
			MaxAge:           300,
		})

		r.Use(cors.Handler)
		logger.Info("CORS enabled", zap.Strings("allowed_origins", cfg.Cors.AllowedOrigins))
	}

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Heartbeat("/health"))
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// checking Values as map[string][]string also catches ?pretty and ?pretty=
			// r.URL.Query().Get("pretty") would not.
			if _, ok := r.URL.Query()["pretty"]; ok {
				r.Header.Set("Accept", "application/json+pretty")
			}
			h.ServeHTTP(w, r)
		})
	})
	r.Use(middleware.Compress(gzip.DefaultCompression))
	r.Use(middleware.Recoverer)
	r.Mount("/debug", middleware.Profiler())
	r.Mount("/metrics", promhttp.Handler())
	r.Mount("/api/v1", api)

	if err := authenticationHTTPMount(ctx, cfg.Authentication, r, conn); err != nil {
		return nil, err
	}

	r.Route("/meta", func(r chi.Router) {
		r.Use(middleware.SetHeader("Content-Type", "application/json"))
		r.Handle("/info", info)
		r.Handle("/config", cfg)
	})

	if cfg.UI.Enabled {
		s := http.FS(swagger.Docs)
		r.Mount("/docs", http.StripPrefix("/docs/", http.FileServer(s)))

		u, err := fs.Sub(ui.UI, "dist")
		if err != nil {
			return nil, fmt.Errorf("mounting UI: %w", err)
		}

		r.Mount("/", http.FileServer(http.FS(u)))
	}

	server.Server = &http.Server{
		Addr:           fmt.Sprintf("%s:%d", cfg.Server.Host, httpPort),
		Handler:        r,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   30 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	logger.Debug("starting http server")

	var (
		apiAddr = fmt.Sprintf("%s://%s:%d/api/v1", cfg.Server.Protocol, cfg.Server.Host, httpPort)
		uiAddr  = fmt.Sprintf("%s://%s:%d", cfg.Server.Protocol, cfg.Server.Host, httpPort)
	)

	if isConsole {
		color.Green("\nAPI: %s", apiAddr)

		if cfg.UI.Enabled {
			color.Green("UI: %s", uiAddr)
		}

		fmt.Println()
	} else {
		logger.Info("api available", zap.String("address", apiAddr))

		if cfg.UI.Enabled {
			logger.Info("ui available", zap.String("address", uiAddr))
		}
	}

	if cfg.Server.Protocol != config.HTTPS {
		server.listenAndServe = server.ListenAndServe
		return server, nil
	}

	server.Server.TLSConfig = &tls.Config{
		MinVersion:               tls.VersionTLS12,
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		},
	}

	server.Server.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))

	server.listenAndServe = func() error {
		return server.ListenAndServeTLS(cfg.Server.CertFile, cfg.Server.CertKey)
	}

	return server, nil
}

// Run starts listening and serving the Flipt HTTP API.
// It blocks until the server is shutdown.
func (h *HTTPServer) Run() error {
	if err := h.listenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}

	return nil
}

// Shutdown triggers the shutdown operation of the HTTP API.
func (h *HTTPServer) Shutdown(ctx context.Context) error {
	h.logger.Info("shutting down HTTP server...")

	return h.Server.Shutdown(ctx)
}