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
)

// shutdownGrace is how long in-flight requests get to finish after ctx is
// cancelled. It sits just above platformTimeout so a request already waiting
// on the chat platform can complete, and comfortably inside Kubernetes'
// default 30s termination grace so the drain is not cut short by SIGKILL.
const shutdownGrace = platformTimeout + 5*time.Second

// serveHTTP binds addr, serves h until ctx is cancelled, then shuts the server
// down gracefully. Shared by switchboard's two optional listeners — the
// metrics/health surface and the outbound ingress — so both fail and drain the
// same way; name prefixes errors so a failure says which one it was.
//
// Binding is synchronous: a port already in use returns before the goroutine
// starts, which is what lets serve turn a bind failure into a non-zero exit
// rather than running on without a surface a deployment depends on.
func serveHTTP(ctx context.Context, name, addr string, h http.Handler) error {
	server := &http.Server{
		Handler: h,
		// Every phase of a request is bounded, so no client can park a
		// goroutine by trickling one: headers, then the body (which the ingress
		// also caps by size), then the handler itself. WriteTimeout must stay
		// comfortably above platformTimeout — the ingress spends most of a
		// request waiting on the chat platform, and cutting the response short
		// would tell a caller its post failed when it did not.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      2 * platformTimeout,
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s: listen %s: %w", name, addr, err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()
	select {
	case <-ctx.Done():
		// Shutdown lets in-flight requests finish. If they do not finish in
		// time they are cut off mid-flight — for the ingress that can mean a
		// post whose outcome the caller never learns, so say so rather than
		// reporting a clean stop.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("%s: shutdown: %w", name, err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("%s: %w", name, err)
	}
}
