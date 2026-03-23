// Copyright 2026 The Kubernetes Authors.
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

package environment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentStop_NilReceiver(t *testing.T) {
	t.Parallel()

	var env *Environment
	require.NoError(t, env.Stop())
}

func TestEnvironmentStop_DoesNotBlockAfterManagerExitObserved(t *testing.T) {
	t.Parallel()

	env := &Environment{
		managerReady: make(chan struct{}),
		managerDone:  make(chan struct{}),
	}
	env.managerErr = errors.New("manager boom")
	close(env.managerDone)

	err := env.waitForManagerReady()
	require.EqualError(t, err, "manager exited before becoming ready: manager boom")

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- env.Stop()
	}()

	select {
	case stopErr := <-stopDone:
		require.EqualError(t, stopErr, "manager boom")
	case <-time.After(time.Second):
		t.Fatal("Stop blocked after manager exit was already observed")
	}
}

func TestEnvironmentStop_IgnoresContextCanceledManagerExit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	env := &Environment{
		context:     ctx,
		cancel:      cancel,
		managerDone: make(chan struct{}),
	}
	env.managerErr = context.Canceled
	close(env.managerDone)

	assert.NoError(t, env.Stop())
}
