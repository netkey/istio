// Copyright 2018 Istio Authors
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

package mixer

import (
	"fmt"
	"net"
	"strings"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	http_conn "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	"github.com/gogo/protobuf/types"

	meshconfig "istio.io/api/mesh/v1alpha1"
	mpb "istio.io/api/mixer/v1"
	mccpb "istio.io/api/mixer/v1/config/client"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/proxy/envoy/v1"
	"istio.io/istio/pkg/log"
)

// Plugin is a mixer plugin.
type Plugin struct{}

// NewPlugin returns an ptr to an initialized mixer.Plugin.
func NewPlugin() plugin.Plugin {
	return Plugin{}
}

// OnOutboundListener implements the Callbacks interface method.
func (Plugin) OnOutboundListener(in *plugin.InputParams, mutable *plugin.MutableObjects) error {
	env := in.Env
	node := in.Node
	proxyInstances := in.ProxyInstances

	switch in.ListenerType {
	case plugin.ListenerTypeHTTP:
		for cnum := range mutable.FilterChains {
			m := buildMixerHTTPFilter(env, node, proxyInstances, true)
			if m != nil {
				mutable.FilterChains[cnum].HTTP = append(mutable.FilterChains[cnum].HTTP, m)
			}
		}
		return nil
	case plugin.ListenerTypeTCP:
		return nil
	}

	return fmt.Errorf("unknown listener type %v in mixer.OnOutboundListener", in.ListenerType)
}

// OnInboundListener implements the Callbacks interface method.
func (Plugin) OnInboundListener(in *plugin.InputParams, mutable *plugin.MutableObjects) error {
	env := in.Env
	node := in.Node
	proxyInstances := in.ProxyInstances
	instance := in.ServiceInstance

	switch in.ListenerType {
	case plugin.ListenerTypeHTTP:
		for cnum := range mutable.FilterChains {
			mutable.FilterChains[cnum].HTTP = append(mutable.FilterChains[cnum].HTTP, buildMixerHTTPFilter(env, node, proxyInstances, false))
		}
		return nil
	case plugin.ListenerTypeTCP:
		for cnum := range mutable.FilterChains {
			m := buildMixerInboundTCPFilter(env, node, instance)
			if m != nil {
				mutable.FilterChains[cnum].TCP = append(mutable.FilterChains[cnum].TCP, *m)
			}
		}
		return nil
	}

	return fmt.Errorf("unknown listener type %v in mixer.OnOutboundListener", in.ListenerType)
}

// OnOutboundCluster implements the Plugin interface method.
func (Plugin) OnOutboundCluster(env model.Environment, node model.Proxy, service *model.Service, servicePort *model.Port, cluster *xdsapi.Cluster) {
}

// OnInboundCluster implements the Plugin interface method.
func (Plugin) OnInboundCluster(env model.Environment, node model.Proxy, service *model.Service, servicePort *model.Port, cluster *xdsapi.Cluster) {
}

// OnOutboundRouteConfiguration implements the Plugin interface method.
func (Plugin) OnOutboundRouteConfiguration(in *plugin.InputParams, routeConfiguration *xdsapi.RouteConfiguration) {
}

// oc := BuildMixerConfig(node, serviceName, dest, proxyInstances, config, mesh.DisablePolicyChecks, false)
// func BuildMixerConfig(source model.Proxy, destName string, dest *model.Service, instances []*model.ServiceInstance, config model.IstioConfigStore,

// OnInboundRouteConfiguration implements the Plugin interface method.
func (Plugin) OnInboundRouteConfiguration(in *plugin.InputParams, routeConfiguration *xdsapi.RouteConfiguration) {
	forward := false
	if in.Node.Type == model.Ingress {
		forward = true
	}

	switch in.ListenerType {
	case plugin.ListenerTypeHTTP:
		var nvhs []route.VirtualHost
		for _, vh := range routeConfiguration.VirtualHosts {
			nvh := vh
			var nrs []route.Route
			for _, r := range vh.Routes {
				nr := r
				if nr.PerFilterConfig == nil {
					nr.PerFilterConfig = make(map[string]*types.Struct)
				}
				nr.PerFilterConfig[v1.MixerFilter] = util.MessageToStruct(
					buildMixerPerRouteConfig(in, false, forward, in.ServiceInstance.Service.Hostname.String()))
				nrs = append(nrs, nr)
			}
			nvh.Routes = nrs
			nvhs = append(nvhs, nvh)
		}
		routeConfiguration.VirtualHosts = nvhs

	case plugin.ListenerTypeTCP:
		// TODO: implement
	default:
		log.Warn("Unknown listener type in mixer#OnOutboundRouteConfiguration")
	}
}

func buildMixerPerRouteConfig(in *plugin.InputParams, outboundRoute bool, _ /*disableForward*/ bool, destinationService string) *mccpb.ServiceConfig {
	role := in.Node
	nodeInstances := in.ProxyInstances
	disableCheck := in.Env.Mesh.DisablePolicyChecks
	config := in.Env.IstioConfigStore

	out := serviceConfig(in.Service.Hostname.String(), in.ServiceInstance, config, disableCheck, false, role.Domain)
	// Report calls are never disabled. Disable forward is currently not in the proto.
	out.DisableCheckCalls = disableCheck

	if destinationService != "" {
		out.MixerAttributes = &mpb.Attributes{}
		out.MixerAttributes.Attributes = map[string]*mpb.Attributes_AttributeValue{
			v1.AttrDestinationService: {Value: &mpb.Attributes_AttributeValue_StringValue{StringValue: destinationService}},
		}
		addDestinationServiceAttributes(out.MixerAttributes.Attributes, destinationService, role.Domain)
	}

	var labels map[string]string
	// Note: instances are all running on mode.Node named 'role'
	// So instance labels are the workload / Node labels.
	if len(nodeInstances) > 0 {
		labels = nodeInstances[0].Labels
	}

	if !outboundRoute || role.Type == model.Router {
		// for outboundRoutes there are no default MixerAttributes except for gateway.
		// specific MixerAttributes are in per route configuration.
		v1.AddStandardNodeAttributes(out.MixerAttributes.Attributes, v1.AttrDestinationPrefix, role.IPAddress, role.ID, labels)
	}

	return out
}

// buildMixerHTTPFilter builds a filter with a v1 mixer config encapsulated as JSON in a proto.Struct for v2 consumption.
func buildMixerHTTPFilter(env *model.Environment, node *model.Proxy,
	proxyInstances []*model.ServiceInstance, outbound bool) *http_conn.HttpFilter {
	mesh := env.Mesh
	config := env.IstioConfigStore
	if mesh.MixerCheckServer == "" && mesh.MixerReportServer == "" {
		return nil
	}

	c := buildHTTPMixerFilterConfig(mesh, *node, proxyInstances, outbound, config)
	return &http_conn.HttpFilter{
		Name:   v1.MixerFilter,
		Config: util.MessageToStruct(c),
	}
}

// buildMixerInboundTCPFilter builds a filter with a v1 mixer config encapsulated as JSON in a proto.Struct for v2 consumption.
func buildMixerInboundTCPFilter(env *model.Environment, node *model.Proxy, instance *model.ServiceInstance) *listener.Filter {
	mesh := env.Mesh
	if mesh.MixerCheckServer == "" && mesh.MixerReportServer == "" {
		return nil
	}

	c := buildTCPMixerFilterConfig(mesh, *node, instance)
	return &listener.Filter{
		Name:   v1.MixerFilter,
		Config: util.MessageToStruct(c),
	}
}

// defined in install/kubernetes/helm/istio/charts/mixer/templates/service.yaml
const (
	//mixerPortName       = "grpc-mixer"
	mixerPortNumber = 9091
	//mixerMTLSPortName   = "grpc-mixer-mtls"
	mixerMTLSPortNumber = 15004
)

// buildHTTPMixerFilterConfig builds a mixer HTTP filter config. Mixer filter uses outbound configuration by default
// (forward attributes, but not invoke check calls)  ServiceInstances belong to the Node.
func buildHTTPMixerFilterConfig(mesh *meshconfig.MeshConfig, role model.Proxy, nodeInstances []*model.ServiceInstance, outboundRoute bool, config model.IstioConfigStore) *mccpb.HttpClientConfig { // nolint: lll
	mcs, _, _ := net.SplitHostPort(mesh.MixerCheckServer)
	mrs, _, _ := net.SplitHostPort(mesh.MixerReportServer)

	port := mixerPortNumber
	if mesh.AuthPolicy == meshconfig.MeshConfig_MUTUAL_TLS {
		port = mixerMTLSPortNumber
	}

	// TODO: derive these port types.
	transport := &mccpb.TransportConfig{
		CheckCluster:  model.BuildSubsetKey(model.TrafficDirectionOutbound, "", model.Hostname(mcs), port),
		ReportCluster: model.BuildSubsetKey(model.TrafficDirectionOutbound, "", model.Hostname(mrs), port),
	}

	mxConfig := &mccpb.HttpClientConfig{
		MixerAttributes: &mpb.Attributes{
			Attributes: map[string]*mpb.Attributes_AttributeValue{
				"context.reporter.uid": {
					Value: &mpb.Attributes_AttributeValue_StringValue{StringValue: "kubernetes://" + role.ID},
				},
			},
		},
		ServiceConfigs: map[string]*mccpb.ServiceConfig{},
		Transport:      transport,
	}

	var labels map[string]string
	// Note: instances are all running on mode.Node named 'role'
	// So instance labels are the workload / Node labels.
	if len(nodeInstances) > 0 {
		labels = nodeInstances[0].Labels
		mxConfig.DefaultDestinationService = nodeInstances[0].Service.Hostname.String()
	}

	if !outboundRoute || role.Type == model.Router {
		// for outboundRoutes there are no default MixerAttributes except for gateway.
		// specific MixerAttributes are in per route configuration.
		v1.AddStandardNodeAttributes(mxConfig.MixerAttributes.Attributes, v1.AttrDestinationPrefix, role.IPAddress, role.ID, labels)
		mxConfig.MixerAttributes.Attributes["context.reporter.local"] = &mpb.Attributes_AttributeValue{
			Value: &mpb.Attributes_AttributeValue_BoolValue{BoolValue: true},
		}
	}

	if role.Type == model.Sidecar && !outboundRoute {
		// Don't forward mixer attributes to the app from inbound sidecar routes
	} else {
		mxConfig.ForwardAttributes = &mpb.Attributes{
			Attributes: map[string]*mpb.Attributes_AttributeValue{},
		}
		addStandardNodeAttributes(mxConfig.ForwardAttributes.Attributes, v1.AttrSourcePrefix, role.IPAddress, role.ID, labels)
	}

	// gateway case is special because upstream listeners are considered outbound, however we don't want to
	// automatically disable policy / report.
	disablePolicy := (outboundRoute && role.Type != model.Router) || mesh.DisablePolicyChecks
	disableReport := outboundRoute && role.Type != model.Router

	for _, instance := range nodeInstances {
		mxConfig.ServiceConfigs[instance.Service.Hostname.String()] =
			serviceConfig(instance.Service.Hostname.String(), instance, config, disablePolicy, disableReport, role.Domain)
	}

	return mxConfig
}

// buildTCPMixerFilterConfig builds a TCP filter config for inbound requests.
func buildTCPMixerFilterConfig(mesh *meshconfig.MeshConfig, role model.Proxy, instance *model.ServiceInstance) *mccpb.TcpClientConfig {
	attrs := v1.StandardNodeAttributes(v1.AttrDestinationPrefix, role.IPAddress, role.ID, nil)
	addDestinationServiceAttributes(attrs, instance.Service.Hostname.String(), role.Domain)
	attrs["context.reporter.uid"] = attrStringValue("kubernetes://" + role.ID)
	attrs["context.reporter.local"] = &mpb.Attributes_AttributeValue{
		Value: &mpb.Attributes_AttributeValue_BoolValue{BoolValue: true},
	}

	mcs, _, _ := net.SplitHostPort(mesh.MixerCheckServer)
	mrs, _, _ := net.SplitHostPort(mesh.MixerReportServer)

	port := mixerPortNumber
	if mesh.AuthPolicy == meshconfig.MeshConfig_MUTUAL_TLS {
		port = mixerMTLSPortNumber
	}

	transport := &mccpb.TransportConfig{
		CheckCluster:  model.BuildSubsetKey(model.TrafficDirectionOutbound, "", model.Hostname(mcs), port),
		ReportCluster: model.BuildSubsetKey(model.TrafficDirectionOutbound, "", model.Hostname(mrs), port),
	}

	mxConfig := &mccpb.TcpClientConfig{
		MixerAttributes: &mpb.Attributes{
			Attributes: attrs,
		},
		Transport:         transport,
		DisableCheckCalls: mesh.DisablePolicyChecks,
	}

	return mxConfig
}

// addStandardNodeAttributes add standard node attributes with the given prefix
func addStandardNodeAttributes(attr map[string]*mpb.Attributes_AttributeValue, prefix string, IPAddress string, ID string, labels map[string]string) {
	if len(IPAddress) > 0 {
		attr[prefix+"."+v1.AttrIPSuffix] = &mpb.Attributes_AttributeValue{
			Value: &mpb.Attributes_AttributeValue_BytesValue{net.ParseIP(IPAddress)},
		}
	}

	attr[prefix+"."+v1.AttrUIDSuffix] = &mpb.Attributes_AttributeValue{
		Value: &mpb.Attributes_AttributeValue_StringValue{"kubernetes://" + ID},
	}

	if len(labels) > 0 {
		attr[prefix+"."+v1.AttrLabelsSuffix] = &mpb.Attributes_AttributeValue{
			Value: &mpb.Attributes_AttributeValue_StringMapValue{
				StringMapValue: &mpb.Attributes_StringMap{Entries: labels},
			},
		}
	}
}

// borrows heavily from v1.ServiceConfig (which this replaces)
func serviceConfig(serviceHostname string, dest *model.ServiceInstance, config model.IstioConfigStore, disableCheck, disableReport bool, proxyDomain string) *mccpb.ServiceConfig { // nolint: lll
	sc := &mccpb.ServiceConfig{
		MixerAttributes: &mpb.Attributes{
			Attributes: map[string]*mpb.Attributes_AttributeValue{
				"destination.service": attrStringValue(serviceHostname),
			},
		},
		DisableCheckCalls:  disableCheck,
		DisableReportCalls: disableReport,
	}

	if len(dest.Labels) > 0 {
		sc.MixerAttributes.Attributes["destination.labels"] = &mpb.Attributes_AttributeValue{
			Value: &mpb.Attributes_AttributeValue_StringMapValue{
				StringMapValue: &mpb.Attributes_StringMap{Entries: dest.Labels},
			},
		}
	}

	apiSpecs := config.HTTPAPISpecByDestination(dest)
	model.SortHTTPAPISpec(apiSpecs)
	for _, config := range apiSpecs {
		sc.HttpApiSpec = append(sc.HttpApiSpec, config.Spec.(*mccpb.HTTPAPISpec))
	}

	quotaSpecs := config.QuotaSpecByDestination(dest)
	model.SortQuotaSpec(quotaSpecs)
	for _, config := range quotaSpecs {
		sc.QuotaSpec = append(sc.QuotaSpec, config.Spec.(*mccpb.QuotaSpec))
	}

	addDestinationServiceAttributes(sc.MixerAttributes.Attributes, serviceHostname, proxyDomain)
	return sc
}

func addDestinationServiceAttributes(attrs map[string]*mpb.Attributes_AttributeValue, destinationHostname, domain string) {
	svcName, svcNamespace := nameAndNamespace(destinationHostname, domain)
	attrs["destination.service"] = attrStringValue(destinationHostname) // DEPRECATED. Remove when fully out of use.
	attrs["destination.service.host"] = attrStringValue(destinationHostname)
	attrs["destination.service.uid"] = attrStringValue(fmt.Sprintf("istio://%s/services/%s", svcNamespace, svcName))
	attrs["destination.service.name"] = attrStringValue(svcName)
	if len(svcNamespace) > 0 {
		attrs["destination.service.namespace"] = attrStringValue(svcNamespace)
	}
}

func nameAndNamespace(serviceHostname, domain string) (name, namespace string) {
	domainParts := strings.SplitN(domain, ".", 2)
	if !strings.HasSuffix(serviceHostname, domainParts[1]) {
		return serviceHostname, ""
	}

	parts := strings.Split(serviceHostname, ".")
	if len(parts) > 1 {
		return parts[0], parts[1]
	}

	return serviceHostname, ""
}

func attrStringValue(value string) *mpb.Attributes_AttributeValue {
	return &mpb.Attributes_AttributeValue{Value: &mpb.Attributes_AttributeValue_StringValue{StringValue: value}}
}
