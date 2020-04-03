// Copyright 2019 Istio Authors. All Rights Reserved.
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

package outboundtrafficpolicy

import (
	"bytes"
	"fmt"
	"html/template"
	"reflect"
	"testing"
	"time"

	envoyAdmin "github.com/envoyproxy/go-control-plane/envoy/admin/v3"

	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/echoboot"
	"istio.io/istio/pkg/test/framework/components/environment/kube"
	"istio.io/istio/pkg/test/framework/components/galley"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/framework/components/pilot"
	"istio.io/istio/pkg/test/framework/components/prometheus"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/pkg/test/util/structpath"
	util "istio.io/istio/tests/integration/mixer"
)

const (
	ServiceEntry = `
apiVersion: networking.istio.io/v1alpha3
kind: ServiceEntry
metadata:
  name: http
spec:
  hosts:
  - istio.io
  location: MESH_EXTERNAL
  ports:
  - name: http
    number: 80
    protocol: HTTP
  - name: http-for-https
    number: 443
    protocol: HTTP
  - name: http-tcp
    number: 9090
    protocol: HTTP
  resolution: DNS
`
	SidecarScope = `
apiVersion: networking.istio.io/v1alpha3
kind: Sidecar
metadata:
  name: restrict-to-service-entry-namespace
spec:
  egress:
  - hosts:
    - "{{.ImportNamespace}}/*"
    - "istio-system/*"
  outboundTrafficPolicy:
    mode: "{{.TrafficPolicyMode}}"
`

	Gateway = `
apiVersion: networking.istio.io/v1alpha3
kind: Gateway
metadata:
  name: istio-egressgateway
spec:
  selector:
    istio: egressgateway
  servers:
  - port:
      number: 80
      name: http
      protocol: HTTP
    hosts:
    - "some-external-site.com"
  - port:
      number: 443
      name: https
      protocol: TLS
    hosts:
    - "some-external-site.com"
    tls:
      mode: PASSTHROUGH

---
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: route-via-egressgateway
spec:
  hosts:
    - "some-external-site.com"
  gateways:
  - istio-egressgateway
  - mesh
  http:
    - match:
      - gateways:
        - mesh # from sidecars, route to egress gateway service
        port: 80
      route:
      - destination:
          host: istio-egressgateway.istio-system.svc.cluster.local
          port:
            number: 80
        weight: 100
    - match:
      - gateways:
        - istio-egressgateway
        port: 80
      route:
      - destination:
          host: destination.{{.AppNamespace}}.svc.cluster.local
          port:
            number: 80
        weight: 100
      headers:
        request:
          add:
            handled-by-egress-gateway: "true"
    - match:
      - gateways:
        - mesh # from sidecars, route to egress gateway service
        port: 443
      route:
      - destination:
          host: istio-egressgateway.istio-system.svc.cluster.local
          port:
            number: 443
        weight: 100
    - match:
      - gateways:
        - istio-egressgateway
        port: 443
      route:
      - destination:
          host: destination.{{.AppNamespace}}.svc.cluster.local
          port:
            number: 443
        weight: 100
      headers:
        request:
          add:
            handled-by-egress-gateway: "true"
`
)

// TestCase represents what is being tested
type TestCase struct {
	Name     string
	PortName string
	Host     string
	Gateway  bool
	Scheme   scheme.Instance
	Expected Expected
}

// Expected contains the metric and query to run against
// prometheus to validate that expected telemetry information was gathered;
// as well as the http response code
type Expected struct {
	Metric          string
	PromQueryFormat string
	ResponseCode    []string
}

// TrafficPolicy is the mode of the outbound traffic policy to use
// when configuring the sidecar for the client
type TrafficPolicy string

const (
	AllowAny     TrafficPolicy = "ALLOW_ANY"
	RegistryOnly TrafficPolicy = "REGISTRY_ONLY"
)

// String implements fmt.Stringer
func (t TrafficPolicy) String() string {
	return string(t)
}

// We want to test "external" traffic. To do this without actually hitting an external endpoint,
// we can import only the service namespace, so the apps are not known
func createSidecarScope(t *testing.T, tPolicy TrafficPolicy, appsNamespace namespace.Instance, serviceNamespace namespace.Instance, g galley.Instance) {
	tmpl, err := template.New("SidecarScope").Parse(SidecarScope)
	if err != nil {
		t.Errorf("failed to create template: %v", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"ImportNamespace": serviceNamespace.Name(), "TrafficPolicyMode": tPolicy.String()}); err != nil {
		t.Errorf("failed to create template: %v", err)
	}
	if err := g.ApplyConfig(appsNamespace, buf.String()); err != nil {
		t.Errorf("failed to apply service entries: %v", err)
	}
}

// We want to test "external" traffic. To do this without actually hitting an external endpoint,
// we can import only the service namespace, so the apps are not known
func createGateway(t *testing.T, appsNamespace namespace.Instance, serviceNamespace namespace.Instance, g galley.Instance) {
	tmpl, err := template.New("Gateway").Parse(Gateway)
	if err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"AppNamespace": appsNamespace.Name()}); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}
	if err := g.ApplyConfig(serviceNamespace, buf.String()); err != nil {
		t.Fatalf("failed to apply gateway: %v. template: %v", err, buf.String())
	}
}

// TODO support native environment for registry only/gateway. Blocked by #13177 because the listeners for native use static
// routes and this test relies on the dynamic routes sent through pilot to allow external traffic.

func RunExternalRequest(cases []*TestCase, prometheus prometheus.Instance, mode TrafficPolicy, t *testing.T) {

	// Testing of Blackhole and Passthrough clusters:
	// Setup of environment:
	// 1. client and destination are deployed to app-1-XXXX namespace
	// 2. client is restricted to talk to destination via Sidecar scope where outbound policy is set (ALLOW_ANY, REGISTRY_ONLY)
	//    and clients' egress can only be to service-2-XXXX/* and istio-system/*
	// 3. a namespace service-2-YYYY is created
	// 4. A gateway is put in service-2-YYYY where its host is set for some-external-site.com on port 80 and 443
	// 3. a VirtualService is also created in service-2-XXXX to:
	//    a) route requests for some-external-site.com to the istio-egressgateway
	//       * if the request on port 80, then it will add an http header `handled-by-egress-gateway`
	//    b) from the egressgateway it will forward the request to the destination pod deployed in the app-1-XXX
	//       namespace

	// Test cases:
	// 1. http case:
	//    client -------> Hits listener 0.0.0.0_80 cluster
	//    Metric is istio_requests_total i.e. HTTP
	//
	// 2. https case:
	//    client ----> Hits listener 0.0.0.0_443
	//    Metric is istio_tcp_connections_closed_total i.e. TCP
	//
	// 3. http_egress
	//    client ) ---HTTP request (Host: some-external-site.com----> Hits listener 0.0.0.0_80 ->
	//      VS Routing (add Egress Header) --> Egress Gateway --> destination
	//    Metric is istio_requests_total i.e. HTTP with destination as destination
	//
	// 4. TCP
	//    client ---TCP request at port 9090----> Hits listener 0.0.0.0_9090 -> 0.0.0.0_150001 -> ALLOW_ANY/REGISTRY_ONLY
	//    Metric is istio_tcp_connections_closed_total i.e. TCP
	//
	framework.
		NewTest(t).
		Run(func(ctx framework.TestContext) {
			client, dest := setupEcho(t, ctx, mode)

			for _, tc := range cases {
				t.Run(tc.Name, func(t *testing.T) {
					if _, kube := ctx.Environment().(*kube.Environment); !kube && tc.Gateway {
						t.Skip("Cannot run gateway in native environment.")
					}
					retry.UntilSuccessOrFail(t, func() error {
						resp, err := client.Call(echo.CallOptions{
							Target:   dest,
							PortName: tc.PortName,
							Scheme:   tc.Scheme,
							Headers: map[string][]string{
								"Host": {tc.Host},
							},
						})

						// the expected response from a blackhole test case will have err
						// set; use the length of the expected code to ignore this condition
						if err != nil && len(tc.Expected.ResponseCode) != 0 {
							return fmt.Errorf("request failed: %v", err)
						}

						codes := make([]string, 0, len(resp))
						for _, r := range resp {
							codes = append(codes, r.Code)
						}
						if !reflect.DeepEqual(codes, tc.Expected.ResponseCode) {
							return fmt.Errorf("got codes %q, expected %q", codes, tc.Expected.ResponseCode)
						}

						for _, r := range resp {
							if _, f := r.RawResponse["Handled-By-Egress-Gateway"]; tc.Gateway && !f {
								return fmt.Errorf("expected to be handled by gateway. response: %+v", r.RawResponse)
							}
						}
						return nil
					}, retry.Delay(time.Second), retry.Timeout(20*time.Second))

					if tc.Expected.Metric != "" {
						util.ValidateMetric(t, prometheus, tc.Expected.PromQueryFormat, tc.Expected.Metric, 1)
					}
				})
			}
		})
}

func setupEcho(t *testing.T, ctx resource.Context, mode TrafficPolicy) (echo.Instance, echo.Instance) {
	g := galley.NewOrFail(t, ctx, galley.Config{})
	p := pilot.NewOrFail(t, ctx, pilot.Config{Galley: g})

	appsNamespace := namespace.NewOrFail(t, ctx, namespace.Config{
		Prefix: "app",
		Inject: true,
	})
	serviceNamespace := namespace.NewOrFail(t, ctx, namespace.Config{
		Prefix: "service",
		Inject: true,
	})

	var client, dest echo.Instance
	echoboot.NewBuilderOrFail(t, ctx).
		With(&client, echo.Config{
			Service:   "client",
			Namespace: appsNamespace,
			Subsets:   []echo.SubsetConfig{{}},
			Pilot:     p,
			Galley:    g,
		}).
		With(&dest, echo.Config{
			Service:   "destination",
			Namespace: appsNamespace,
			Subsets:   []echo.SubsetConfig{{}},
			Pilot:     p,
			Galley:    g,
			Ports: []echo.Port{
				{
					Name:         "http",
					Protocol:     protocol.HTTP,
					InstancePort: 8090,
					ServicePort:  80,
				},
				{
					Name:         "https",
					Protocol:     protocol.HTTPS,
					InstancePort: 8091,
					ServicePort:  443,
				},
				{
					Name:         "tcp",
					Protocol:     protocol.TCP,
					InstancePort: 8092,
					ServicePort:  9090,
				},
			},
		}).BuildOrFail(t)

	// External traffic should work even if we have service entries on the same ports
	createSidecarScope(t, mode, appsNamespace, serviceNamespace, g)
	if err := g.ApplyConfig(serviceNamespace, ServiceEntry); err != nil {
		t.Errorf("failed to apply service entries: %v", err)
	}

	if _, kube := ctx.Environment().(*kube.Environment); kube {
		createGateway(t, appsNamespace, serviceNamespace, g)
	}
	if err := WaitUntilNotCallable(client, dest); err != nil {
		t.Fatalf("failed to apply sidecar, %v", err)
	}
	return client, dest
}

func clusterName(target echo.Instance, port echo.Port) string {
	cfg := target.Config()
	return fmt.Sprintf("outbound|%d||%s.%s.svc.%s", port.ServicePort, cfg.Service, cfg.Namespace.Name(), cfg.Domain)
}

// Wait for the destination to NOT be callable by the client. This allows us to simulate external traffic.
// This essentially just waits for the Sidecar to be applied, without sleeping.
func WaitUntilNotCallable(c echo.Instance, dest echo.Instance) error {
	accept := func(cfg *envoyAdmin.ConfigDump) (bool, error) {
		validator := structpath.ForProto(cfg)
		for _, port := range dest.Config().Ports {
			clusterName := clusterName(dest, port)
			// Ensure that we have an outbound configuration for the target port.
			err := validator.NotExists("{.configs[*].dynamicActiveClusters[?(@.cluster.Name == '%s')]}", clusterName).Check()
			if err != nil {
				return false, err
			}
		}

		return true, nil
	}

	workloads, _ := c.Workloads()
	// Wait for the outbound config to be received by each workload from Pilot.
	for _, w := range workloads {
		if w.Sidecar() != nil {
			if err := w.Sidecar().WaitForConfig(accept, retry.Timeout(time.Second*10)); err != nil {
				return err
			}
		}
	}

	return nil
}