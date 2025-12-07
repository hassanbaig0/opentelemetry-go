// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/sdk/trace"

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/trace/internal/observ"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

type tracer struct {
	embedded.Tracer

	provider             *TracerProvider
	instrumentationScope instrumentation.Scope

	inst observ.Tracer
}

var _ trace.Tracer = &tracer{}

// Start starts a Span and returns it along with a context containing it.
//
// The Span is created with the provided name and as a child of any existing
// span context found in the passed context. The created Span will be
// configured appropriately by any SpanOption passed.
func (tr *tracer) Start(
	ctx context.Context,
	name string,
	options ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	config := trace.NewSpanStartConfig(options...)

	if ctx == nil {
		// Prevent trace.ContextWithSpan from panicking.
		ctx = context.Background()
	}

	// For local spans created by this SDK, track child span count.
	if p := trace.SpanFromContext(ctx); p != nil {
		if sdkSpan, ok := p.(*recordingSpan); ok {
			sdkSpan.addChild()
		}
	}

	s := tr.newSpan(ctx, name, &config)
	newCtx := trace.ContextWithSpan(ctx, s)
	if tr.inst.Enabled() {
		if o, ok := s.(interface{ setOrigCtx(context.Context) }); ok {
			// If this is a recording span, store the original context.
			// This allows later retrieval of baggage and other information
			// that may have been stored in the context at span start time and
			// to avoid the allocation of repeatedly calling
			// trace.ContextWithSpan.
			o.setOrigCtx(newCtx)
		}
		psc := trace.SpanContextFromContext(ctx)
		tr.inst.SpanStarted(newCtx, psc, s)
	}

	if rw, ok := s.(ReadWriteSpan); ok && s.IsRecording() {
		sps := tr.provider.getSpanProcessors()
		for _, sp := range sps {
			// Use original context.
			sp.sp.OnStart(ctx, rw)
		}
	}
	if rtt, ok := s.(runtimeTracer); ok {
		newCtx = rtt.runtimeTrace(newCtx)
	}

	return newCtx, s
}

type runtimeTracer interface {
	// runtimeTrace starts a "runtime/trace".Task for the span and
	// returns a context containing the task.
	runtimeTrace(ctx context.Context) context.Context
}

// newSpan returns a new configured span.
func (tr *tracer) newSpan(ctx context.Context, name string, config *trace.SpanConfig) trace.Span {
	// If told explicitly to make this a new root use a zero value SpanContext
	// as a parent which contains an invalid trace ID and is not remote.
	var psc trace.SpanContext
	if config.NewRoot() {
		ctx = trace.ContextWithSpanContext(ctx, psc)
	} else {
		psc = trace.SpanContextFromContext(ctx)
	}

	// If there is a valid parent trace ID, use it to ensure the continuity of
	// the trace. Always generate a new span ID so other components can rely
	// on a unique span ID, even if the Span is non-recording.
	var tid trace.TraceID
	var sid trace.SpanID
	if !psc.TraceID().IsValid() {
		tid, sid = tr.provider.idGenerator.NewIDs(ctx)
	} else {
		tid = psc.TraceID()
		sid = tr.provider.idGenerator.NewSpanID(ctx, tid)
	}

	samplingResult := tr.provider.sampler.ShouldSample(SamplingParameters{
		ParentContext: ctx,
		TraceID:       tid,
		Name:          name,
		Kind:          config.SpanKind(),
		Attributes:    config.Attributes(),
		Links:         config.Links(),
	})

	scc := trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceState: samplingResult.Tracestate,
	}
	if isSampled(samplingResult) {
		scc.TraceFlags = psc.TraceFlags() | trace.FlagsSampled
	} else {
		scc.TraceFlags = psc.TraceFlags() &^ trace.FlagsSampled
	}
	sc := trace.NewSpanContext(scc)

	if !isRecording(samplingResult) {
		return tr.newNonRecordingSpan(sc)
	}
	return tr.newRecordingSpan(ctx, psc, sc, name, samplingResult, config)
}

// newRecordingSpan returns a new configured recordingSpan.
func (tr *tracer) newRecordingSpan(
	ctx context.Context,
	psc, sc trace.SpanContext,
	name string,
	sr SamplingResult,
	config *trace.SpanConfig,
) *recordingSpan {
	startTime := config.Timestamp()
	if startTime.IsZero() {
		startTime = time.Now()
	}

	s := &recordingSpan{
		// Do not pre-allocate the attributes slice here! Doing so will
		// allocate memory that is likely never going to be used, or if used,
		// will be over-sized. The default Go compiler has been tested to
		// dynamically allocate needed space very well. Benchmarking has shown
		// it to be more performant than what we can predetermine here,
		// especially for the common use case of few to no added
		// attributes.

		parent:      psc,
		spanContext: sc,
		spanKind:    trace.ValidateSpanKind(config.SpanKind()),
		name:        name,
		startTime:   startTime,
		events:      newEvictedQueueEvent(tr.provider.spanLimits.EventCountLimit),
		links:       newEvictedQueueLink(tr.provider.spanLimits.LinkCountLimit),
		tracer:      tr,
	}

	for _, l := range config.Links() {
		s.AddLink(l)
	}

	s.SetAttributes(sr.Attributes...)
	s.SetAttributes(config.Attributes()...)

	if tr.inst.Enabled() {
		// Propagate any existing values from the context with the new span to
		// the measurement context.
		ctx = trace.ContextWithSpan(ctx, s)
		tr.inst.SpanLive(ctx, s)
	}

	return s
}

// newNonRecordingSpan returns a new configured nonRecordingSpan.
func (tr *tracer) newNonRecordingSpan(sc trace.SpanContext) nonRecordingSpan {
	return nonRecordingSpan{tracer: tr, sc: sc}
}

// StartSpan creates a span without requiring a context.
// This is useful for manual instrumentation where context propagation is not available.
// This method bypasses context usage for maximum performance in high-frequency scenarios.
func (tr *tracer) StartSpan(name string, options ...trace.SpanStartOption) trace.Span {
	config := trace.NewSpanStartConfig(options...)

	// Check if parent IDs were provided
	parentTraceID := config.ParentTraceID()
	parentSpanID := config.ParentSpanID()

	// Determine parent span context
	var psc trace.SpanContext
	if parentTraceID.IsValid() {
		// Create a parent span context from provided IDs
		psc = trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: parentTraceID,
			SpanID:  parentSpanID,
			Remote:  true,
		})
	}

	// Generate trace ID and span ID without context overhead
	// The default ID generator doesn't use the context parameter
	var tid trace.TraceID
	var sid trace.SpanID
	if parentTraceID.IsValid() && !config.NewRoot() {
		// Use parent trace ID and generate new span ID
		tid = parentTraceID
		sid = tr.provider.idGenerator.NewSpanID(nil, tid)
	} else {
		// Generate new trace ID and span ID for root span
		tid, sid = tr.provider.idGenerator.NewIDs(nil)
	}

	// Perform sampling decision
	samplingResult := tr.provider.sampler.ShouldSample(SamplingParameters{
		ParentContext: nil, // No context needed for manual instrumentation
		TraceID:       tid,
		Name:          name,
		Kind:          config.SpanKind(),
		Attributes:    config.Attributes(),
		Links:         config.Links(),
	})

	// Build span context
	scc := trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceState: samplingResult.Tracestate,
	}
	if isSampled(samplingResult) {
		scc.TraceFlags = psc.TraceFlags() | trace.FlagsSampled
	} else {
		scc.TraceFlags = psc.TraceFlags() &^ trace.FlagsSampled
	}
	sc := trace.NewSpanContext(scc)

	// Create the appropriate span type
	if !isRecording(samplingResult) {
		return tr.newNonRecordingSpan(sc)
	}

	// Create recording span directly without context
	return tr.newRecordingSpanDirect(psc, sc, name, samplingResult, &config)
}

// newRecordingSpanDirect creates a recording span without requiring context.
// This is used by StartSpan for context-free span creation.
func (tr *tracer) newRecordingSpanDirect(
	psc, sc trace.SpanContext,
	name string,
	sr SamplingResult,
	config *trace.SpanConfig,
) *recordingSpan {
	startTime := config.Timestamp()
	if startTime.IsZero() {
		startTime = time.Now()
	}

	s := &recordingSpan{
		parent:      psc,
		spanContext: sc,
		spanKind:    trace.ValidateSpanKind(config.SpanKind()),
		name:        name,
		startTime:   startTime,
		events:      newEvictedQueueEvent(tr.provider.spanLimits.EventCountLimit),
		links:       newEvictedQueueLink(tr.provider.spanLimits.LinkCountLimit),
		tracer:      tr,
	}

	for _, l := range config.Links() {
		s.AddLink(l)
	}

	s.SetAttributes(sr.Attributes...)
	s.SetAttributes(config.Attributes()...)

	// Notify span processors
	// OnStart implementations in standard processors (batch, simple) ignore the context parameter
	sps := tr.provider.getSpanProcessors()
	for _, sp := range sps {
		sp.sp.OnStart(nil, s)
	}

	return s
}
