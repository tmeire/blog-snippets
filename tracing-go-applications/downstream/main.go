package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
)

var tracer trace.Tracer

func initTracer() func() {
	// Create a resource describing the service
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String("my-client"),
			semconv.ServiceVersionKey.String("1.0.0"),
		),
	)
	if err != nil {
		log.Fatalf("Failed to create resource: %v", err)
	}

	// Set up a connection to the collector
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create an exporter that connects to the collector. If you don't provide an endpoint with
	// otlptracegrpc.WithEndpoint, it will fall back to localhost:4317. You can then overwrite it using the
	// OTEL_EXPORTER_OTLP_ENDPOINT environment variable.
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint("localhost:4317"),
		otlptracegrpc.WithInsecure(),
	)
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

func callDownstreamService(ctx context.Context) {
	// Create a span for the outgoing request
	ctx, span := tracer.Start(ctx, "callDownstream")
	defer span.End()

	data := url.Values{}
	data.Set("id", "1")
	data.Set("password", "foobar")

	br := bytes.NewBufferString(data.Encode())

	// Create an HTTP request with the current context
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost:8080/user/auth", br)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// The otelhttp client will automatically inject trace context into the request headers
	client := http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		return
	}
	defer resp.Body.Close()

	// Process response
	io.Copy(os.Stdout, resp.Body)
}

func main() {
	ctx := context.Background()
	cleanup := initTracer()
	defer cleanup()

	tracer = otel.Tracer("my-client")

	callDownstreamService(ctx)
}
