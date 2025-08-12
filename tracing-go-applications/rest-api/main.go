package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"

	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

type User struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func initTracer() func() {
	// Create a resource describing the service
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String("user-service"),
			semconv.ServiceVersionKey.String("1.0.0"),
		),
	)
	if err != nil {
		log.Fatalf("Failed to create resource: %v", err)
	}

	// Set up a connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create an exporter to the collector
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint("localhost:4317"),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		log.Fatalf("Failed to create exporter: %v", err)
	}

	// Create a trace provider with a batch span processor
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(tp)

	// Set up propagation
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			log.Fatalf("Failed to shut down tracer provider: %v", err)
		}
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

func getUserHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Extract user ID from query parameters
	userID := r.URL.Query().Get("id")
	if userID == "" {
		http.Error(w, "Missing user ID", http.StatusBadRequest)
		return
	}

	// Add the user ID as an attribute to the current span
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("user.id", userID))

	// Get user from database
	user, err := getUserFromDB(ctx, userID)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "Failed to get user", http.StatusInternalServerError)
		return
	}

	// Return user as JSON
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(user)
}

func getUserFromDB(ctx context.Context, userID string) (*User, error) {
	// No need for manual span creation - otelsql handles it automatically
	var user User
	err := db.QueryRowContext(ctx, "SELECT id, name, email FROM users WHERE id = $1", userID).Scan(
		&user.ID, &user.Name, &user.Email,
	)
	if err != nil {
		// Get the current span from context to record the error
		span := trace.SpanFromContext(ctx)
		span.RecordError(err)
		return nil, err
	}

	return &user, nil
}

func main() {
	// Initialize tracer
	cleanup := initTracer()
	defer cleanup()

	// Initialize database
	initDB()

	// Set up HTTP server with OpenTelemetry instrumentation
	getUserHandlerFunc := http.HandlerFunc(getUserHandler)
	http.Handle("/user", otelhttp.NewHandler(getUserHandlerFunc, "http.get_user"))

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
