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

// Package axapi holds generated client stubs for the ax control-plane API
// (ax.v1alpha1), used by pkg/ax. ax's own Go module names
// github.com/google/ax while the code switchboard runs against lives in a
// fork, and it needs a newer Go toolchain than switchboard; so, as
// k8s-lookout does (internal/axapi there), switchboard carries a pinned copy
// of ax.proto instead of importing ax.
package axapi

//go:generate buf generate --template {"version":"v2","plugins":[{"local":"protoc-gen-go","out":".","opt":"paths=source_relative"},{"local":"protoc-gen-go-grpc","out":".","opt":"paths=source_relative"}]} --path ax.proto
