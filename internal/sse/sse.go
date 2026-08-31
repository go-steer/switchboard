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

// Package sse decodes a text/event-stream body into events.
//
// It exists so switchboard has one event-stream reader rather than one per
// route. Core-agent serves two streams switchboard reads — the session's
// /events and the permission broker's /perms/stream — and they are the same
// wire format carrying different payloads; a second hand-rolled parser is a
// second place for the boundary rules to drift.
//
// Only the subset core-agent sends is decoded: the `event:` and `data:` fields,
// dispatched on the blank line that ends a record. Comment lines (`:`), `id:`
// and `retry:` are ignored, which is what the spec asks of a client that does
// not use them.
package sse

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Event is one record from the stream: the event name and its payload,
// both already trimmed of the field prefix and surrounding space.
type Event struct {
	Type string
	Data string
}

const (
	// initialBuffer is what the scanner starts with. Most frames are far
	// smaller; this only saves regrowth on the ones that are not.
	initialBuffer = 64 * 1024

	// maxLine bounds a single line, and against core-agent it is the cap that
	// binds: both of that daemon's writers emit one `data:` line per record
	// and neither truncates what the agent produced, so a tool result larger
	// than this ends the subscription with a scanner error. Raising it is a
	// judgement about how large a frame is worth buffering, not a bug fix —
	// nothing downstream can relay a megabyte into a chat message either way.
	maxLine = 1024 * 1024

	// maxRecord bounds a whole record, which is a separate question from
	// maxLine: data accumulates across every `data:` line until the blank
	// one, so a per-line cap alone leaves the total set by how many lines the
	// far end feels like sending. No core-agent build spreads a record over
	// lines, so this never binds today; it is here because the decoder's
	// contract is with the wire format rather than with one writer, and
	// without it a conforming stream is a way to make switchboard allocate
	// without limit.
	maxRecord = 4 * 1024 * 1024
)

// Scan reads events from r and calls fn for each one, in order, on the
// calling goroutine. It returns when the stream ends (nil), when reading
// fails, or when fn returns an error — which is returned unwrapped, so a
// caller can use its own sentinel to stop early.
//
// A record is dispatched on the blank line that terminates it. A record
// carrying neither a type nor data is dropped rather than delivered: that
// is what a keep-alive comment or a stray blank line looks like after the
// fields this decoder cares about have been filtered out, and fn should not
// have to tell those apart from a real frame.
//
// Multi-line data is joined with newlines. A truncated final record — the
// stream ends mid-frame, with no blank line — is NOT delivered, because the
// only thing distinguishing it from a complete one is the terminator it is
// missing, and handing a caller half a JSON payload to parse is worse than
// dropping a frame it will see again when it resumes.
//
// A record whose data exceeds maxRecord, or a line exceeding maxLine, ends
// the scan with an error rather than growing to meet it.
//
// Field values are trimmed of surrounding whitespace. The spec strips exactly
// one leading space and keeps the rest; this is lossier, and matches what the
// reader here has always done. It costs nothing against core-agent, which
// writes compact single-line JSON, but it is not spec-conformant and a stream
// whose payloads carry significant leading or trailing space would notice.
func Scan(r io.Reader, fn func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, initialBuffer), maxLine)

	// Data accumulates as bytes rather than by string concatenation: a record
	// spread over many lines would otherwise recopy the whole payload per
	// line, turning a large frame into quadratic work.
	var (
		typ  string
		data []byte
	)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if typ != "" || len(data) != 0 {
				if err := fn(Event{Type: typ, Data: string(data)}); err != nil {
					return err
				}
			}
			typ, data = "", data[:0]
		case strings.HasPrefix(line, "event:"):
			typ = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimSpace(line[len("data:"):])
			// The separator is counted only when one will be written, so the
			// cap is the size of the record this produces rather than one
			// less on the first line.
			want := len(data) + len(d)
			if len(data) != 0 {
				want++
			}
			if want > maxRecord {
				return fmt.Errorf("sse: record data exceeds %d bytes", maxRecord)
			}
			if len(data) != 0 {
				data = append(data, '\n')
			}
			data = append(data, d...)
		}
	}
	return sc.Err()
}
