// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/trace"

import (
	"context"

	"go.opentelemetry.io/otel/trace/embedded"
)

// Tracer is the creator of Spans.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Tracer interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Tracer

	// Start creates a span and a context.Context containing the newly-created span.
	//
	// If the context.Context provided in `ctx` contains a Span then the newly-created
	// Span will be a child of that span, otherwise it will be a root span. This behavior
	// can be overridden by providing `WithNewRoot()` as a SpanOption, causing the
	// newly-created Span to be a root span even if `ctx` contains a Span.
	//
	// When creating a Span it is recommended to provide all known span attributes using
	// the `WithAttributes()` SpanOption as samplers will only have access to the
	// attributes provided when a Span is created.
	//
	// Any Span that is created MUST also be ended. This is the responsibility of the user.
	// Implementations of this API may leak memory or other resources if Spans are not ended.
	Start(ctx context.Context, spanName string, opts ...SpanStartOption) (context.Context, Span)

	// StartSpan creates a span without requiring a context.
	// This is useful for manual instrumentation where context propagation is not available.
	//
	// To create a child span, use WithParentTraceID and WithParentSpanID options.
	// If no parent IDs are specified, a new root span will be created.
	//
	// Example creating a root span:
	//   span := tracer.StartSpan("operation-name")
	//
	// Example creating a child span:
	//   span := tracer.StartSpan("child-operation",
	//     trace.WithParentTraceID(parentTraceID),
	//     trace.WithParentSpanID(parentSpanID))
	//
	// The span must be ended by calling End() when the operation is complete.
	StartSpan(spanName string, opts ...SpanStartOption) Span
}
