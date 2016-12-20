// Copyright 2014 The fleet Authors
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

package client

import (
	"testing"

	"github.com/coreos/fleet/registry"
	"github.com/coreos/fleet/version"
	"github.com/coreos/go-semver/semver"
)

func TestCheckVersion(t *testing.T) {
	reg := newFakeRegistryForCheckVersion(version.Version)
	_, ok := checkVersion(reg)
	if !ok {
		t.Errorf("checkVersion failed but should have succeeded")
	}
	reg = newFakeRegistryForCheckVersion("9.0.0")
	msg, ok := checkVersion(reg)
	if ok || msg == "" {
		t.Errorf("checkVersion succeeded but should have failed")
	}
}

func newFakeRegistryForCheckVersion(v string) registry.ClusterRegistry {
	sv, err := semver.NewVersion(v)
	if err != nil {
		panic(err)
	}

	return registry.NewFakeClusterRegistry(sv, 0)
}
