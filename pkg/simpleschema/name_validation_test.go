// Copyright 2026 The Kubernetes Authors
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

package simpleschema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStringSchemaFromMarkers(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "supported", value: `minLength=2 maxLength=30 pattern="^[a-z]" enum=alpha,beta validation="self != 'beta'"`},
		{name: "rejects a type declaration", value: "string | maxLength=30", wantErr: "invalid marker key"},
		{name: "rejects required", value: "required=true", wantErr: "not supported"},
		{name: "rejects defaults", value: "default=foo", wantErr: "not supported"},
		{name: "rejects unrelated markers", value: "description=foo", wantErr: "not supported"},
		{name: "rejects malformed length", value: "maxLength=abc", wantErr: "failed to parse maxLength"},
		{name: "rejects negative length", value: "minLength=-1", wantErr: "must not be negative"},
		{name: "rejects inverted range", value: "minLength=4 maxLength=3", wantErr: "must not exceed"},
		{name: "rejects malformed pattern", value: `pattern="["`, wantErr: "invalid pattern regex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema, err := StringSchemaFromMarkers(tt.value)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "string", schema.Type)
			require.NotNil(t, schema.MinLength)
			assert.EqualValues(t, 2, *schema.MinLength)
			require.NotNil(t, schema.MaxLength)
			assert.EqualValues(t, 30, *schema.MaxLength)
			assert.Equal(t, "^[a-z]", schema.Pattern)
			require.Len(t, schema.Enum, 2)
			require.Len(t, schema.XValidations, 1)
			assert.Equal(t, "self != 'beta'", schema.XValidations[0].Rule)
		})
	}
}
