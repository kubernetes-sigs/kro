// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package fieldpath

import (
	"fmt"
	"strconv"
)

// Segment represents a single part of a path
type Segment struct {
	Name  string // Field name without quotes
	Index int    // -1 if not an array access
}

// NewNamedSegment creates a new named segment.
func NewNamedSegment(name string) Segment {
	return Segment{Name: name, Index: -1}
}

// NewIndexedSegment creates a new indexed segment.
func NewIndexedSegment(index int) Segment {
	return Segment{Index: index}
}

// Parse parses a path string into segments. Assumes dictionary
// access is always quoted.
//
// example paths:
// - spec["my.field.name"].items[0]["other.field"]
// - simple.path["quoted.field"][0]
func Parse(path string) ([]Segment, error) {
	p := &parser{
		input: path,
		pos:   0,
		len:   len(path),
	}
	return p.parse()
}

// parser is a helper struct for parsing a path string. It keeps
// track of the current position in the input string.
type parser struct {
	input string
	pos   int
	len   int
}

// parse parses the input string and returns a slice of segments.
func (p *parser) parse() ([]Segment, error) {
	var segments []Segment

	for p.pos < p.len {
		// NOTE(a-hilaly): unescape quoted fields? not sure if we need to do this.
		// This is a very low level parser, and the paths we'll be dealing with
		// are mainly produced by the `internal/typesystem/parser` package.

		// check if we're starting with a quoted field.
		if p.pos+1 < p.len && p.input[p.pos] == '[' && p.input[p.pos+1] == '"' {
			field, err := p.parseQuotedField()
			if err != nil {
				return nil, err
			}
			segments = append(segments, Segment{Name: field, Index: -1})

		} else if p.pos+1 < p.len && p.input[p.pos] != '[' {
			// Parse unquoted field until we hit a [ or .
			field, err := p.parseUnquotedField()
			if err != nil {
				return nil, err
			}
			segments = append(segments, Segment{Name: field, Index: -1})
		}

		// Check for array index
		if p.pos < p.len && p.input[p.pos] == '[' && (p.pos+1 >= p.len || p.input[p.pos+1] != '"') {
			idx, err := p.parseArrayIndex()
			if err != nil {
				return nil, err
			}
			segments = append(segments, Segment{Name: "", Index: idx})
		}

		// Skip dot if present
		if p.pos < p.len && p.input[p.pos] == '.' {
			p.pos++
		}
	}

	return segments, nil
}

// parseQuotedField parses a quoted field. It assumes the current
// position is at the opening bracket and quote.
//
// e.g ["my.field.name"]
func (p *parser) parseQuotedField() (string, error) {
	// Skip [ and opening quote. Note that we already checked the index
	// bounds in the parse function.
	p.pos += 2

	start := p.pos
	for p.pos < p.len {
		if p.input[p.pos] != '"' {
			p.pos++
			continue
		}
		field := p.input[start:p.pos]
		p.pos++ // skip closing quote

		if p.pos < p.len && p.input[p.pos] == ']' {
			p.pos++ // skip closing bracket
			return field, nil
		}
		return "", fmt.Errorf("expected closing bracket after quote at position %d", p.pos)
	}
	return "", fmt.Errorf("unterminated quoted string starting at position %d", start)
}

// parseUnquotedField parses an unquoted field. It assumes the current
// position is at the start of the field.
//
// e.g my.field.name
func (p *parser) parseUnquotedField() (string, error) {
	start := p.pos
	for p.pos < p.len {
		if p.input[p.pos] == '.' || p.input[p.pos] == '[' {
			break
		}
		p.pos++
	}

	if start == p.pos {
		return "", fmt.Errorf("empty field name at position %d", start)
	}
	return p.input[start:p.pos], nil
}

// parseArrayIndex parses an array index. It assumes the current
// position is at the opening bracket.
func (p *parser) parseArrayIndex() (int, error) {
	p.pos++ // skip [

	start := p.pos
	for p.pos < p.len && p.input[p.pos] != ']' {
		p.pos++
	}

	if p.pos >= p.len {
		return -1, fmt.Errorf("unterminated array index at position %d", start)
	}

	idxStr := p.input[start:p.pos]
	p.pos++ // skip ]

	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		return -1, fmt.Errorf("invalid array index '%s' at position %d", idxStr, start)
	}

	return idx, nil
}
