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
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
)

// quotedIntKeys are the JSON keys chat/v1 declares as int64 with the ",string"
// tag. encoding/json refuses a bare number for those, so normalizeNumbers
// quotes them; every other integer it leaves bare, which is what the plain
// int64 fields require. These are all of them in chat/v1 at the version
// go.mod pins — a new one is caught by TestQuotedIntKeysMatchChatV1.
var quotedIntKeys = map[string]bool{
	"commandId":    true,
	"msSinceEpoch": true,
	"valueMsEpoch": true,
}

// normalizeNumbers rewrites a Chat event so the generated chat/v1 types can
// decode it, whichever ingress it came from.
//
// The HTTP ingress delivers proto-JSON, which spells *every* number as a
// float: "offset": -3.6E6, "startIndex": 0.0, "commandId": 1.0. Pub/Sub sends
// the same fields as 100 and "1". chat/v1 decodes into int64, and
// encoding/json fails the whole event on the first float it meets — so before
// this, every HTTP event was dropped at decode, live, while hand-written
// fixtures with ints stayed green (docs/DESIGN.md §3.4 predicted this for
// appCommandId alone; commandID handles that one field, and this is the same
// fix for the rest of the payload).
//
// A whole number in int64 range becomes an integer literal, quoted under a
// quotedIntKeys key. Anything else — a fraction, an out-of-range value, a
// string — is left exactly as sent, and a payload with nothing to rewrite is
// returned unchanged, so Pub/Sub traffic decodes byte-for-byte as before.
// Invalid JSON is returned unchanged too, so the decode error a caller sees is
// the one it would have seen anyway.
func normalizeNumbers(data []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return data
	}
	// Decode reads one value; anything after it makes the payload invalid,
	// and re-marshalling just the first value would quietly repair it.
	if _, err := dec.Token(); err != io.EOF {
		return data
	}
	if !rewriteNumbers(v) {
		return data
	}
	out, err := json.Marshal(v)
	if err != nil {
		return data
	}
	return out
}

// rewriteNumbers walks a decoded JSON value in place and reports whether it
// changed anything.
func rewriteNumbers(v any) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if n, ok := child.(json.Number); ok {
				if r, ok := wholeNumber(n, quotedIntKeys[k]); ok {
					t[k] = r
					changed = true
				}
				continue
			}
			if rewriteNumbers(child) {
				changed = true
			}
		}
	case []any:
		for i, child := range t {
			if n, ok := child.(json.Number); ok {
				if r, ok := wholeNumber(n, false); ok {
					t[i] = r
					changed = true
				}
				continue
			}
			if rewriteNumbers(child) {
				changed = true
			}
		}
	}
	return changed
}

// wholeNumber returns the replacement for n, and false when n should stay as
// it is: already an integer literal (unless it must be quoted), or not a whole
// number in int64 range. The range guard is the one commandID uses.
func wholeNumber(n json.Number, quote bool) (any, bool) {
	s := n.String()
	isFloat := strings.ContainsAny(s, ".eE")
	if !isFloat && !quote {
		return nil, false
	}
	var i int64
	if isFloat {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != math.Trunc(f) || f < -maxInt64f || f >= maxInt64f {
			return nil, false
		}
		i = int64(f)
	} else {
		var err error
		if i, err = strconv.ParseInt(s, 10, 64); err != nil {
			return nil, false
		}
	}
	if quote {
		return strconv.FormatInt(i, 10), true
	}
	return json.Number(strconv.FormatInt(i, 10)), true
}
