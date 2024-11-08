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

package parser

import (
	"testing"
)

func TestParseReadyWhen(t *testing.T) {
	testCases := []struct {
		name          string
		expression    []string
		expectedError string
	}{
		{
			name:          "Two expressions",
			expression:    []string{"${hello}${goodbye}"},
			expectedError: "only standalone expressions are allowed",
		},
		{
			name:          "With Postfix",
			expression:    []string{"${hello}-world"},
			expectedError: "only standalone expressions are allowed",
		},
		{
			name:          "With Prefix",
			expression:    []string{"hello-${world}"},
			expectedError: "only standalone expressions are allowed",
		},
		{
			name:          "Standalone expression",
			expression:    []string{"${hello}"},
			expectedError: "",
		},
		{
			name:          "Complex standalone expression that works",
			expression:    []string{"${hello + world - someone = whoever[else]isNone dum dum {pop}out-here}"},
			expectedError: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConditionExpressions(tc.expression)

			if tc.expectedError == "" {
				if err != nil {
					t.Error("Expected no error, but got '%w'", err)
				}
			} else {
				if err == nil {
					t.Errorf("Expected error '%s', but got nil", tc.expectedError)
				} else if err.Error() != tc.expectedError {
					t.Errorf("Expected error: %s\nError we got: %s", tc.expectedError, err.Error())
				}
			}
		})
	}
}
