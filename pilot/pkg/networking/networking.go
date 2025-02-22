// Copyright Istio Authors
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

package networking

import (
	"fmt"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_conn "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tls "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/pkg/log"
)

// ListenerProtocol is the protocol associated with the listener.
type ListenerProtocol int

const (
	// ListenerProtocolUnknown is an unknown type of listener.
	ListenerProtocolUnknown = iota
	// ListenerProtocolTCP is a TCP listener.
	ListenerProtocolTCP
	// ListenerProtocolHTTP is an HTTP listener.
	ListenerProtocolHTTP
	// ListenerProtocolAuto enables auto protocol detection
	ListenerProtocolAuto
)

const (
	// BlackHoleCluster to catch traffic from routes with unresolved clusters. Traffic arriving here goes nowhere.
	BlackHoleCluster = "BlackHoleCluster"
	// BlackHole is the name of the virtual host and route name used to block all traffic
	BlackHole = "block_all"
	// PassthroughCluster to forward traffic to the original destination requested. This cluster is used when
	// traffic does not match any listener in envoy.
	PassthroughCluster = "PassthroughCluster"
	// Passthrough is the name of the virtual host used to forward traffic to the
	// PassthroughCluster
	Passthrough = "allow_any"
)

// ModelProtocolToListenerProtocol converts from a config.Protocol to its corresponding plugin.ListenerProtocol
func ModelProtocolToListenerProtocol(p protocol.Instance,
	trafficDirection core.TrafficDirection) ListenerProtocol {
	switch p {
	case protocol.HTTP, protocol.HTTP2, protocol.HTTP_PROXY, protocol.GRPC, protocol.GRPCWeb:
		return ListenerProtocolHTTP
	case protocol.TCP, protocol.HTTPS, protocol.TLS,
		protocol.Mongo, protocol.Redis, protocol.MySQL:
		return ListenerProtocolTCP
	case protocol.UDP:
		return ListenerProtocolUnknown
	case protocol.Unsupported:
		// If protocol sniffing is not enabled, the default value is TCP
		switch trafficDirection {
		case core.TrafficDirection_INBOUND:
			if !features.EnableProtocolSniffingForInbound {
				return ListenerProtocolTCP
			}
		case core.TrafficDirection_OUTBOUND:
			if !features.EnableProtocolSniffingForOutbound {
				return ListenerProtocolTCP
			}
		default:
			// Should not reach here.
		}
		return ListenerProtocolAuto
	default:
		// Should not reach here.
		return ListenerProtocolAuto
	}
}

type TransportProtocol uint8

const (
	// TransportProtocolTCP is a TCP listener
	TransportProtocolTCP = iota
	// TransportProtocolQUIC is a QUIC listener
	TransportProtocolQUIC
)

func (tp TransportProtocol) String() string {
	switch tp {
	case TransportProtocolTCP:
		return "tcp"
	case TransportProtocolQUIC:
		return "quic"
	}
	return "unknown"
}

func (tp TransportProtocol) ToEnvoySocketProtocol() core.SocketAddress_Protocol {
	if tp == TransportProtocolQUIC {
		return core.SocketAddress_UDP
	}
	return core.SocketAddress_TCP
}

// FilterChain describes a set of filters (HTTP or TCP) with a shared TLS context.
type FilterChain struct {
	// FilterChainMatch is the match used to select the filter chain.
	FilterChainMatch *listener.FilterChainMatch
	// TLSContext is the TLS settings for this filter chains.
	TLSContext *tls.DownstreamTlsContext
	// ListenerFilters are the filters needed for the whole listener, not particular to this
	// filter chain.
	ListenerFilters []*listener.ListenerFilter
	// ListenerProtocol indicates whether this filter chain is for HTTP or TCP
	// Note that HTTP filter chains can also have network filters
	ListenerProtocol ListenerProtocol
	// TransportProtocol indicates the type of transport used - TCP, UDP, QUIC
	// This would be TCP by default
	TransportProtocol TransportProtocol
	// IstioMutualGateway is set only when this filter chain is part of a Gateway, and
	// the Server corresponding to this filter chain is doing TLS termination with ISTIO_MUTUAL as the TLS mode.
	// This allows the authN plugin to add the istio_authn filter to gateways in addition to sidecars.
	IstioMutualGateway bool

	// HTTP is the set of HTTP filters for this filter chain
	HTTP []*http_conn.HttpFilter
	// TCP is the set of network (TCP) filters for this filter chain.
	TCP []*listener.Filter
	// IsFallthrough indicates if the filter chain is fallthrough.
	IsFallThrough bool
}

// MutableObjects is a set of objects passed to On*Listener callbacks. Fields may be nil or empty.
// Any lists should not be overridden, but rather only appended to.
// Non-list fields may be mutated; however it's not recommended to do this since it can affect other plugins in the
// chain in unpredictable ways.
type MutableObjects struct {
	// Listener is the listener being built. Must be initialized before Plugin methods are called.
	Listener *listener.Listener

	// FilterChains is the set of filter chains that will be attached to Listener.
	FilterChains []FilterChain
}

const (
	NoTunnelTypeName = "notunnel"
	H2TunnelTypeName = "H2Tunnel"
)

type (
	TunnelType    int
	TunnelAbility int
)

const (
	// Bind the no tunnel support to a name.
	NoTunnel TunnelType = 0
	// Enumeration of tunnel type below. Each type should own a unique bit field.
	H2Tunnel TunnelType = 1 << 0
)

func MakeTunnelAbility(ttypes ...TunnelType) TunnelAbility {
	ability := int(NoTunnel)
	for _, tunnelType := range ttypes {
		ability |= int(tunnelType)
	}
	return TunnelAbility(ability)
}

func (t TunnelType) ToString() string {
	switch t {
	case H2Tunnel:
		return H2TunnelTypeName
	default:
		return NoTunnelTypeName
	}
}

func (t TunnelAbility) SupportH2Tunnel() bool {
	return (int(t) & int(H2Tunnel)) != 0
}

// ListenerClass defines the class of the listener
type ListenerClass int

const (
	ListenerClassUndefined ListenerClass = iota
	ListenerClassSidecarInbound
	ListenerClassSidecarOutbound
	ListenerClassGateway
)

func BuildCatchAllVirtualHost(allowAnyoutbound bool, sidecarDestination string) *route.VirtualHost {
	if allowAnyoutbound {
		egressCluster := PassthroughCluster
		notimeout := durationpb.New(0)

		if sidecarDestination != "" {
			// user has provided an explicit destination for all the unknown traffic.
			// build a cluster out of this destination
			egressCluster = sidecarDestination
		}

		routeAction := &route.RouteAction{
			ClusterSpecifier: &route.RouteAction_Cluster{Cluster: egressCluster},
			// Disable timeout instead of assuming some defaults.
			Timeout: notimeout,
			// Use deprecated value for now as the replacement MaxStreamDuration has some regressions.
			// nolint: staticcheck
			MaxGrpcTimeout: notimeout,
		}

		return &route.VirtualHost{
			Name:    Passthrough,
			Domains: []string{"*"},
			Routes: []*route.Route{
				{
					Name: Passthrough,
					Match: &route.RouteMatch{
						PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"},
					},
					Action: &route.Route_Route{
						Route: routeAction,
					},
				},
			},
			IncludeRequestAttemptCount: true,
		}
	}

	return &route.VirtualHost{
		Name:    BlackHole,
		Domains: []string{"*"},
		Routes: []*route.Route{
			{
				Name: BlackHole,
				Match: &route.RouteMatch{
					PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"},
				},
				Action: &route.Route_DirectResponse{
					DirectResponse: &route.DirectResponseAction{
						Status: 502,
					},
				},
			},
		},
		IncludeRequestAttemptCount: true,
	}
}

type TelemetryMode int

const (
	TelemetryModeServer TelemetryMode = iota
	TelemetryModeClient
)

func TelemetryModeForClass(class ListenerClass) TelemetryMode {
	switch class {
	case ListenerClassSidecarInbound:
		return TelemetryModeServer
	default:
		return TelemetryModeClient
	}
}

// MessageToAnyWithError converts from proto message to proto Any
func MessageToAnyWithError(msg proto.Message) (*anypb.Any, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return &anypb.Any{
		// nolint: staticcheck
		TypeUrl: "type.googleapis.com/" + string(proto.MessageName(msg)),
		Value:   b,
	}, nil
}

// MessageToAny converts from proto message to proto Any
func MessageToAny(msg proto.Message) *anypb.Any {
	out, err := MessageToAnyWithError(msg)
	if err != nil {
		log.Error(fmt.Sprintf("error marshaling Any %s: %v", prototext.Format(msg), err))
		return nil
	}
	return out
}
