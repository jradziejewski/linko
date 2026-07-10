package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"github.com/natefinch/lumberjack"
	pkgerr "github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"jradziejewski/linko/internal/build"
	linkoerr "jradziejewski/linko/internal/linkoerr"
	"jradziejewski/linko/internal/store"
)

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	logger, closeLogger, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)
	if err != nil {
		slog.Error("failed to create store", "error", err)
		os.Exit(1)
	}

	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to close logger: %v\n", err)
		}
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error("failed to create store", "error", err)
	}

	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger.Debug("Linko is shutting down!")

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown server", "error", err)
		return 1
	}
	if serverErr != nil {
		logger.Error("server error", "serverError", serverErr)
		return 1
	}

	return 0
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

var httpRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests",
	},
	[]string{"method", "path", "status"},
)

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(rec, r)

		path := r.URL.Path
		method := r.Method
		status := strconv.Itoa(rec.status)

		httpRequestsTotal.
			WithLabelValues(method, path, status).
			Inc()
	})
}

type closeFunc func() error

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	var stderrHandler slog.Handler
	if isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()) {
		stderrHandler = tint.NewHandler(os.Stderr, &tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
		})
	} else {
		stderrHandler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
		})
	}

	handlers := []slog.Handler{
		stderrHandler,
	}
	closers := []closeFunc{}

	if logFile != "" {
		jackLogger := &lumberjack.Logger{
			Filename: logFile,
			MaxSize:  1,
			Compress: true,
		}

		close := jackLogger.Close

		handlers = append(handlers, slog.NewJSONHandler(jackLogger, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		}))
		closers = append(closers, close)
	}

	closer := func() error {
		var errs []error

		for _, close := range closers {
			if err := close(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	return slog.New(slog.NewMultiHandler(handlers...)), closer, nil
}

var sensitiveKeys = map[string]bool{
	"password":     true,
	"key":          true,
	"apikey":       true,
	"secret":       true,
	"pin":          true,
	"user":         true,
	"creditcardno": true,
}

var (
	urlPasswordRegex = regexp.MustCompile(`(?i)([a-z0-9+.-]+://[^/:]+:)([^@/]+)(@)`)
	kvPasswordRegex  = regexp.MustCompile(`(?i)\b(password|key|apikey|secret|pin|user|creditcardno)\b\s*=\s*([^&?\s"';]+)`)
)

func isSensitiveKey(key string) bool {
	return sensitiveKeys[strings.ToLower(key)]
}

func redactEmbeddedSecrets(val string) string {
	val = urlPasswordRegex.ReplaceAllString(val, "${1}[REDACTED]${3}")
	val = kvPasswordRegex.ReplaceAllString(val, "${1}=[REDACTED]")
	return val
}

func sanitizeAttr(a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) && a.Value.Kind() != slog.KindGroup {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindString {
		str := a.Value.String()
		redacted := redactEmbeddedSecrets(str)
		if redacted != str {
			return slog.String(a.Key, redacted)
		}
	} else if a.Value.Kind() == slog.KindAny {
		if str, ok := a.Value.Any().(string); ok {
			redacted := redactEmbeddedSecrets(str)
			if redacted != str {
				return slog.String(a.Key, redacted)
			}
		}
	}
	return a
}

func sanitizeAttrs(attrs []slog.Attr) []slog.Attr {
	sanitized := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		sanitized[i] = sanitizeAttr(a)
	}
	return sanitized
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return sanitizeAttr(a)
		}
		if multiErr, ok := errors.AsType[multiError](err); ok {
			var subAttrs []slog.Attr
			for i, subErr := range multiErr.Unwrap() {
				key := fmt.Sprintf("error_%d", i)
				subAttrs = append(subAttrs, slog.GroupAttrs(key, sanitizeAttrs(errorToAttrs(subErr))...))
			}
			return slog.GroupAttrs("errors", subAttrs...)
		}
		return slog.GroupAttrs("error", sanitizeAttrs(errorToAttrs(err))...)
	}
	return sanitizeAttr(a)
}

func errorToAttrs(err error) []slog.Attr {
	var attrs []slog.Attr
	attrs = append(attrs, slog.Attr{
		Key:   "message",
		Value: slog.StringValue(err.Error()),
	})

	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
		})
	}
	extraAttrs := linkoerr.Attrs(err)
	attrs = append(attrs, extraAttrs...)
	return attrs
}
