package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/bcrypt"

	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

type User struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	password []byte
}

func setupOpenTelemetry(ctx context.Context) func(context.Context) error {
	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("users-api"),
		semconv.ServiceVersion("1.4.2"),
		attribute.String("deployment.environment", "prod"),
	))

	// ---- Tracing
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint("localhost:4317"),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		log.Fatalf("Failed to create exporter: %v", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(tp)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// ---- Metrics
	exp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint("localhost:4317"),
		otlpmetricgrpc.WithInsecure(),
	) // or use Prometheus exporter
	if err != nil {
		log.Fatal(err)
	}

	reader := sdkmetric.NewPeriodicReader(exp)

	// Register a view to:
	//	- collect the http.server.duration metric with a different histogram aggregation that may be
	//	  more suitable for this use case. The default bucket boundaries are:
	//	  0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000
	//	- filter out the high-cardinality "user_id" attribute from the http.server.request.duration metric.

	view := sdkmetric.NewView(
		sdkmetric.Instrument{Name: "http.server.request.duration"},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
			},
			AttributeFilter: func(k attribute.KeyValue) bool { return k.Key != "user_id" },
		},
	)

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(view),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOnFilter),
	)
	otel.SetMeterProvider(mp)

	err = runtime.Start(
		runtime.WithMeterProvider(mp),
		// Collect memory metrics every second, the default is every 15s. This is included as an example in case you
		// would ever need this level of granularity, but it is NOT RECOMMENDED to do this in production environments.
		// The underlying system calls are expensive and could negatively impact your application performance.
		runtime.WithMinimumReadMemStatsInterval(time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}

	// ---- Cleanup of both the metric and trace providers
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		var err error

		if tpErr := tp.Shutdown(ctx); tpErr != nil {
			slog.Warn("Failed to shut down tracer provider", "error", tpErr)
			err = errors.Join(err, tpErr)
		}
		if mpErr := mp.Shutdown(ctx); mpErr != nil {
			slog.Warn("Failed to shut down metric provider", "error", mpErr)
			err = errors.Join(err, mpErr)
		}
		return err
	}
}

func initDB() {
	// Register the sqlite driver with otelsql
	driverName, err := otelsql.Register("sqlite3",
		otelsql.WithAttributes(semconv.DBSystemSqlite),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			Ping:     true,
			RowsNext: true,
		}),
	)
	if err != nil {
		log.Fatalf("Failed to register otelsql driver: %v", err)
	}

	// Open a database connection using the instrumented driver
	db, err = sql.Open(driverName, "users.db")
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	if err = db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}
}

var hashLatency metric.Float64Histogram
var hashError metric.Int64Counter

func signinHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Extract user ID from query parameters
	userID := r.FormValue("id")
	if userID == "" {
		http.Error(w, "Missing user ID", http.StatusBadRequest)
		return
	}

	// Get user from database
	user, err := getUserFromDB(ctx, userID)
	if err != nil {
		http.Error(w, "Failed to get user", http.StatusInternalServerError)
		return
	}

	ctx, span := otel.Tracer("test").Start(ctx, "password_check")
	defer span.End()

	start := time.Now()
	err = bcrypt.CompareHashAndPassword(user.password, []byte(r.FormValue("password")))
	hashLatency.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.Bool("correct", err == nil)))

	if errors.Is(err, bcrypt.ErrHashTooShort) {
		hashError.Add(ctx, 1)
	}

	if err != nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	json.NewEncoder(w).Encode(struct{ Status string }{"OK"})
}

func getUserHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Extract user ID from query parameters
	userID := r.URL.Query().Get("id")
	if userID == "" {
		http.Error(w, "Missing user ID", http.StatusBadRequest)
		return
	}

	// Get user from database
	user, err := getUserFromDB(ctx, userID)
	if err != nil {
		http.Error(w, "Failed to get user", http.StatusInternalServerError)
		return
	}

	// Return user as JSON
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(user)
}

func getUserFromDB(ctx context.Context, userID string) (*User, error) {
	userIDInt, err := strconv.Atoi(userID)
	if err != nil {
		return nil, err
	}

	// No need for manual span creation - otelsql handles it automatically
	var user User
	var password sql.NullString
	err = db.QueryRowContext(ctx, "SELECT id, name, email, password FROM users WHERE id = $1", userIDInt).Scan(
		&user.ID, &user.Name, &user.Email, &password,
	)
	if err != nil {
		// Get the current span from context to record the error
		span := trace.SpanFromContext(ctx)
		span.RecordError(err)
		return nil, err
	}

	if password.Valid != false {
		// Decode password hash from hex string
		pwHash, err := hex.DecodeString(password.String)
		if err != nil {
			return nil, err
		}
		user.password = pwHash
	}

	return &user, nil
}

func main() {
	ctx := context.Background()

	cleanup := setupOpenTelemetry(ctx)
	defer func() {
		err := cleanup(ctx)
		log.Fatal(err)
	}()

	hashLatency, _ = otel.GetMeterProvider().Meter("users-api").Float64Histogram("user.auth.password_check.latency") // seconds
	hashError, _ = otel.GetMeterProvider().Meter("users-api").Int64Counter("user.auth.password_check.errors")

	initDB()

	getUserHandlerFunc := http.HandlerFunc(getUserHandler)
	http.Handle("/user", otelhttp.NewHandler(getUserHandlerFunc, "http.get_user"))

	signinHandlerFunc := http.HandlerFunc(signinHandler)
	http.Handle("/user/auth", otelhttp.NewHandler(signinHandlerFunc, "http.auth_user"))

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
