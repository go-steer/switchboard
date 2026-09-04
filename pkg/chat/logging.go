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

package chat

import "github.com/go-steer/switchboard/internal/logging"

// Logf is the logging hook an Adapter takes, so every adapter's output is
// stamped, levelled and formatted the same way as the router's. The zero
// value discards, and the Infof/Warnf/Errorf methods are nil-safe, so an
// adapter needs no guard of its own.
//
// Aliases rather than definitions, for the same reason CallerMode is one: the
// renderers live in internal/logging, and only an alias lets an adapter
// outside this module name the type it has to supply. A separate definition
// here would be a distinct type from the one the binary builds — Go's
// assignability rules require identical parameter types, not merely the same
// underlying ones — and an external implementer could pass nothing but nil.
type (
	Logf  = logging.Logf
	Level = logging.Level
)

// The levels, aliased for the same reason. See logging.Level for the rubric
// the call sites were classified against.
const (
	LevelInfo  = logging.LevelInfo
	LevelWarn  = logging.LevelWarn
	LevelError = logging.LevelError
)
