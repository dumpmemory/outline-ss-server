// Copyright 2020 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"testing"
	"time"

	"github.com/Shadowsocks-NET/outline-ss-server/service/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

func TestRunSSServer(t *testing.T) {
	m := metrics.NewPrometheusShadowsocksMetrics(nil, prometheus.DefaultRegisterer)
	server, err := RunSSServer("config_example.yml", 30*time.Second, m, 10000, false, false, false)
	if err != nil {
		t.Fatalf("RunSSServer() error = %v", err)
	}
	if err := server.Stop(); err != nil {
		t.Errorf("Error while stopping server: %v", err)
	}
}
