package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"jradziejewski/linko/internal/store"

	pkgerr "github.com/pkg/errors"
)

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

func newServer(store store.Store, port int, cancel context.CancelFunc, logger *slog.Logger) *server {
	mux := http.NewServeMux()

	s := &server{
		store:  store,
		logger: logger,
		cancel: cancel,
	}

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: requestLogger(logger)(mux),
	}

	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)

	return s
}

func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}

	s.logger.Debug("Linko is running",
		slog.String("url", fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)))

	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

const logContextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	http.Error(w, err.Error(), status)
}

type httpInternalError struct {
	internalErr error
	publicMsg   string
}

func (e *httpInternalError) Error() string {
	return e.publicMsg
}

func (e *httpInternalError) StackTrace() pkgerr.StackTrace {
	if tracer, ok := e.internalErr.(stackTracer); ok {
		return tracer.StackTrace()
	}
	return nil
}

func (e *httpInternalError) Unwrap() error {
	return e.internalErr
}

func internalError(internalErr error, publicMsg string) error {
	if publicMsg == "" {
		publicMsg = "Internal Server Error"
	}

	return &httpInternalError{
		internalErr: internalErr,
		publicMsg:   publicMsg,
	}
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n

	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			sr := &spyReadCloser{ReadCloser: r.Body}
			r.Body = sr
			logCtx := &LogContext{}
			r = r.WithContext(context.WithValue(r.Context(), logContextKey, logCtx))

			sw := &spyResponseWriter{ResponseWriter: w}

			next.ServeHTTP(sw, r)

			statusCode := sw.statusCode
			if sw.statusCode == 0 {
				statusCode = http.StatusOK
			}

			args := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", r.RemoteAddr,
				slog.Duration("duration", time.Since(start)),
				"request_body_bytes", sr.bytesRead,
				"response_status", statusCode,
				"response_body_bytes", sw.bytesWritten,
			}

			if logCtx.Username != "" {
				args = append(args, "user", logCtx.Username)
			}

			if logCtx.Error != nil {
				args = append(args, "error", logCtx.Error)
			}

			logger.Info("Served request", args...)

			if logCtx.Username != "" {
				logger.Info("Served user", "username", logCtx.Username)
			}
		})
	}
}

func (s *server) shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
}
