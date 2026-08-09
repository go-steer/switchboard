// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics bundles switchboard's Prometheus collectors behind a private
// registry. Kept as a struct (mirroring k8s-lookout's watch.metrics) so the
// wiring is testable — a test constructs an isolated registry, exercises the
// router, and asserts collector values without stringifying /metrics output.
// The record* helpers are nil-safe so the router can be driven without a
// metrics bundle in tests that do not care.
type metrics struct {
	registry *prometheus.Registry

	messages       *prometheus.CounterVec   // ["outcome"] inbound chat turns handled
	commands       prometheus.Counter       // chat control commands handled
	daemonRequests *prometheus.CounterVec   // ["op","outcome"] create|inject|wake
	daemonDuration *prometheus.HistogramVec // ["op"] daemon request latency
	repliesSent    *prometheus.CounterVec   // ["outcome"] outbound sends to the chat platform
	turnsRelayed   prometheus.Counter       // completed agent turns delivered to chat
	reconnects     prometheus.Counter       // SSE relay reconnects
	activeSessions prometheus.Gauge         // conversation→session entries currently held
}

// newMetrics registers switchboard's collectors against a fresh registry and
// returns the bundle. Tests use this with an isolated registry; serve passes
// the registry's handler to promhttp.
func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	m := &metrics{
		registry: reg,
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_messages_total",
			Help: "Inbound chat message turns handled, by outcome (ok|error).",
		}, []string{"outcome"}),
		commands: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchboard_commands_total",
			Help: "Chat control commands handled (never reach the daemon).",
		}),
		daemonRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_daemon_requests_total",
			Help: "Requests to the core-agent daemon contract, by verb (create|inject|wake) and outcome (ok|error).",
		}, []string{"op", "outcome"}),
		daemonDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "switchboard_daemon_request_duration_seconds",
			Help:    "Latency of core-agent daemon requests, by verb (create|inject|wake).",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		repliesSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_replies_sent_total",
			Help: "Outbound sends to the chat platform (replies, progress, activity), by outcome (ok|error).",
		}, []string{"outcome"}),
		turnsRelayed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchboard_agent_turns_relayed_total",
			Help: "Completed agent turns relayed from the SSE stream back to chat.",
		}),
		reconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchboard_stream_reconnects_total",
			Help: "SSE relay reconnects after a stream ended or errored.",
		}),
		activeSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "switchboard_active_sessions",
			Help: "Conversation→session entries currently held in the in-memory map.",
		}),
	}
	reg.MustRegister(
		m.messages,
		m.commands,
		m.daemonRequests,
		m.daemonDuration,
		m.repliesSent,
		m.turnsRelayed,
		m.reconnects,
		m.activeSessions,
	)
	return m
}

// outcome maps an error to the bounded label value used across the counters.
func outcome(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// The record* helpers are all nil-safe: a nil *metrics is a no-op, so the
// router can run uninstrumented (tests) without scattering nil checks.

func (m *metrics) recordMessage(err error) {
	if m == nil {
		return
	}
	m.messages.WithLabelValues(outcome(err)).Inc()
}

func (m *metrics) recordCommand() {
	if m == nil {
		return
	}
	m.commands.Inc()
}

func (m *metrics) recordDaemon(op string, dur time.Duration, err error) {
	if m == nil {
		return
	}
	m.daemonRequests.WithLabelValues(op, outcome(err)).Inc()
	m.daemonDuration.WithLabelValues(op).Observe(dur.Seconds())
}

func (m *metrics) recordReply(err error) {
	if m == nil {
		return
	}
	m.repliesSent.WithLabelValues(outcome(err)).Inc()
}

func (m *metrics) recordTurnRelayed() {
	if m == nil {
		return
	}
	m.turnsRelayed.Inc()
}

func (m *metrics) recordReconnect() {
	if m == nil {
		return
	}
	m.reconnects.Inc()
}

func (m *metrics) sessionOpened() {
	if m == nil {
		return
	}
	m.activeSessions.Inc()
}

// serveMetrics starts a small HTTP server exposing /metrics and /healthz on
// addr. It blocks until ctx is cancelled; callers start it in a goroutine and
// drive shutdown via ctx. When addr == "" the server is skipped entirely
// (collectors still accumulate in-process, just unexposed) — the default, so a
// bare `serve` binds no port and the distroless pod stays outbound-only unless
// a deployment opts in. Mirrors k8s-lookout's watch.serveMetrics.
func serveMetrics(ctx context.Context, addr string, m *metrics) error {
	if addr == "" {
		<-ctx.Done()
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	// Liveness probe with no /metrics dependency, so a K8s livenessProbe
	// tracks "the process is up" rather than "Prometheus is scraping."
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Bind synchronously so a port-in-use fails fast; then serve in a
	// goroutine and let ctx cancellation drive graceful shutdown.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics: listen %s: %w", addr, err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
