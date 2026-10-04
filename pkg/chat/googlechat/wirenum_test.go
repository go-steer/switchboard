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

package googlechat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestNormalizeNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"float to int", `{"offset":-3.6E6}`, `{"offset":-3600000}`},
		{"zero float", `{"startIndex":0.0}`, `{"startIndex":0}`},
		{"float under a string-tagged key is quoted", `{"commandId":1.0}`, `{"commandId":"1"}`},
		{"int under a string-tagged key is quoted", `{"commandId":7}`, `{"commandId":"7"}`},
		{"quoted stays quoted", `{"commandId":"1"}`, `{"commandId":"1"}`},
		{"nested in arrays", `{"a":[{"length":9.0}],"b":[2.0]}`, `{"a":[{"length":9}],"b":[2]}`},
		{"fraction left alone", `{"x":1.5}`, `{"x":1.5}`},
		{"out of range left alone", `{"x":1e20}`, `{"x":1e20}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(normalizeNumbers([]byte(tc.in))); got != tc.want {
				t.Errorf("normalizeNumbers(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// A payload with nothing to rewrite must come back as the same bytes — that is
// the guarantee that Pub/Sub traffic decodes exactly as it did before — and so
// must one that is not JSON, so the caller reports the error it always did.
func TestNormalizeNumbersUnchanged(t *testing.T) {
	for _, in := range []string{
		`{"a": 1, "commandId": "1", "s": "1.0"}`,
		`{not json`,
		`{"a":1.0} garbage`,
		`{"a":1.0}{"b":2}`,
		``,
	} {
		if got := string(normalizeNumbers([]byte(in))); got != in {
			t.Errorf("normalizeNumbers(%q) = %q, want it unchanged", in, got)
		}
	}
}

// Every Pub/Sub fixture in the corpus is left byte-for-byte alone. Pub/Sub
// sends ints and quotes the ",string" fields, so a rewrite here would mean the
// pass reached traffic it was never meant to touch.
func TestNormalizeNumbersLeavesPubSubCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "events", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(f, "-http-") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := normalizeNumbers(b); string(got) != string(b) {
			t.Errorf("%s: rewritten, want it untouched", f)
		}
	}
}

// TestQuotedIntKeysMatchChatV1 walks every type the decoder reaches from
// wireEvent and checks quotedIntKeys against it both ways. Every integer field
// carrying the ",string" tag must be listed, or a float under it fails decode
// over HTTP as every event did live; and no listed key may also name a plain
// integer field, because the rewrite quotes by key name at any depth and a
// quoted value would fail that field instead. A google.golang.org/api bump
// that breaks either shows up here.
func TestQuotedIntKeysMatchChatV1(t *testing.T) {
	quoted, plain := map[string]bool{}, map[string]bool{}
	seen := map[reflect.Type]bool{}
	isInt := func(k reflect.Kind) bool {
		switch k {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return true
		}
		return false
	}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			parts := strings.Split(f.Tag.Get("json"), ",")
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if isInt(ft.Kind()) && parts[0] != "" && parts[0] != "-" {
				if slices.Contains(parts[1:], "string") {
					quoted[parts[0]] = true
				} else {
					plain[parts[0]] = true
				}
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(wireEvent{}))

	var missing, clash []string
	for k := range quoted {
		if !quotedIntKeys[k] {
			missing = append(missing, k)
		}
	}
	for k := range quotedIntKeys {
		if plain[k] {
			clash = append(clash, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(clash)
	if len(missing) > 0 {
		t.Errorf("chat/v1 integer fields tagged \",string\" not in quotedIntKeys: %v", missing)
	}
	if len(clash) > 0 {
		t.Errorf("quotedIntKeys entries that also name a plain integer field, which quoting would break: %v", clash)
	}
	for _, k := range []string{"commandId", "msSinceEpoch", "valueMsEpoch"} {
		if !quoted[k] {
			t.Errorf("walk did not reach %s; found %v", k, quoted)
		}
	}
}

// The live HTTP slash command decodes to the command it is. Before the
// normalization pass it failed outright on commonEventObject.timeZone.offset.
func TestDecodeLiveHTTPSlashCommand(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "events", "addon-live-http-slash-command.json"))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Common struct {
			TimeZone struct {
				Offset json.Number `json:"offset"`
			} `json:"timeZone"`
		} `json:"commonEventObject"`
	}
	if err := json.Unmarshal(b, &probe); err != nil || !strings.ContainsAny(probe.Common.TimeZone.Offset.String(), ".eE") {
		t.Fatalf("fixture no longer carries a float offset (%q, %v); it must keep Chat's spelling", probe.Common.TimeZone.Offset, err)
	}
	in, err := decodeEvent(b)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if in.kind != kindCommand || in.cmdID != 1 {
		t.Errorf("decoded kind %v cmdID %d, want a command with ID 1", in.kind, in.cmdID)
	}
}
