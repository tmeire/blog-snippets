package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
)

var tracer trace.Tracer

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// Extract the context from the HTTP request
	ctx := r.Context()

	// Create a span
	ctx, span := tracer.Start(ctx, "handleRequest")
	defer span.End()

	// Add attributes to the span
	span.SetAttributes(
		attribute.String("http.method", r.Method),
		attribute.String("http.url", r.URL.String()),
	)

	// Simulate some work
	processRequest(ctx)

	// Record an event
	span.AddEvent("request processed")

	w.Write([]byte("Hello, world!"))
}

func processRequest(ctx context.Context) {
	// Create a child span
	_, span := tracer.Start(ctx, "processRequest")
	defer span.End()

	// Simulate work
	// ...

	// Record an error if something goes wrong
	// span.RecordError(err)
	// span.SetStatus(codes.Error, "processing failed")
}

func initTracer() func() {
	// Create a resource describing the service
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String("my-service"),
			semconv.ServiceVersionKey.String("1.0.0"),
		),
	)
	if err != nil {
		log.Fatalf("Failed to create resource: %v", err)
	}

	// Set up a connection to the collector
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create an exporter that connects to the collector
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint("localhost:4317"),
		otlptracegrpc.WithInsecure())
	if err != nil {
		log.Fatalf("Failed to create exporter: %v", err)
	}

	// Create a trace provider
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

func main() {
	// Initialize tracer (from previous example)
	cleanup := initTracer()
	defer cleanup()

	tracer = otel.Tracer("my-service")

	http.HandleFunc("/", handleRequest)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
