// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	cfdclient "github.com/cloudflare/cloudflared/client"
	"github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/diagnostic"
	"github.com/cloudflare/cloudflared/edgediscovery"
	"github.com/cloudflare/cloudflared/edgediscovery/allregions"
	"github.com/cloudflare/cloudflared/features"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/ingress/origins"
	cfdmetrics "github.com/cloudflare/cloudflared/metrics"
	"github.com/cloudflare/cloudflared/orchestration"
	"github.com/cloudflare/cloudflared/signal"
	"github.com/cloudflare/cloudflared/stream"
	"github.com/cloudflare/cloudflared/supervisor"
	"github.com/cloudflare/cloudflared/tlsconfig"
	"github.com/cloudflare/cloudflared/tracing"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"github.com/cloudflare/cloudflared/tunnelstate"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// originProxy implements connection.OriginProxy by dispatching HTTP
// requests directly to an in-process http.Handler. WebSocket upgrades
// use RoundTrip instead of ServeHTTP so that the 101 response headers
// are written via WriteRespHeaders (proper QUIC framing) rather than
// httputil.ReverseProxy's Hijack-based approach.
type originProxy struct {
	handler http.Handler
	rt      http.RoundTripper
	log     *zerolog.Logger
}

func (p *originProxy) ProxyHTTP(w connection.ResponseWriter, tr *tracing.TracedHTTPRequest, isWebsocket bool) error {
	if isWebsocket {
		return p.proxyWebSocket(w, tr)
	}
	if isGRPC(tr.Request) {
		return p.proxyGRPC(w, tr)
	}
	p.handler.ServeHTTP(w, tr.Request)
	return nil
}

func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

func (p *originProxy) proxyWebSocket(w connection.ResponseWriter, tr *tracing.TracedHTTPRequest) error {
	req := tr.Request.Clone(tr.Request.Context())
	req.RequestURI = ""
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-Websocket-Version", "13")
	req.ContentLength = 0
	req.Body = nil

	resp, err := p.rt.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("websocket roundtrip: %w", err)
	}
	defer resp.Body.Close()

	if err := w.WriteRespHeaders(resp.StatusCode, resp.Header); err != nil {
		return fmt.Errorf("writing websocket response headers: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		_, _ = io.Copy(w, resp.Body)
		return nil
	}

	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return fmt.Errorf("websocket response body is not a ReadWriteCloser")
	}

	eyeballStream := &bidirectionalStream{
		writer: w,
		reader: tr.Request.Body,
	}
	stream.Pipe(eyeballStream, rwc, p.log)
	return nil
}

func (p *originProxy) proxyGRPC(w connection.ResponseWriter, tr *tracing.TracedHTTPRequest) error {
	req := tr.Request.Clone(tr.Request.Context())
	req.RequestURI = ""

	resp, err := p.rt.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("grpc roundtrip: %w", err)
	}
	defer resp.Body.Close()

	if err := w.WriteRespHeaders(resp.StatusCode, resp.Header); err != nil {
		return fmt.Errorf("writing grpc response headers: %w", err)
	}

	// Copy body with flushing for streaming RPCs.
	buf := make([]byte, 32*1024)
	flushWriter, hasFlusher := w.(http.Flusher)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if hasFlusher {
				flushWriter.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	// Forward trailers (grpc-status, grpc-message, etc.).
	for name, values := range resp.Trailer {
		for _, v := range values {
			w.AddTrailer(name, v)
		}
	}

	return nil
}

func (p *originProxy) ProxyTCP(_ context.Context, _ connection.ReadWriteAcker, _ *connection.TCPRequest) error {
	return fmt.Errorf("TCP proxying not yet supported")
}

type bidirectionalStream struct {
	reader io.Reader
	writer io.Writer
}

func (s *bidirectionalStream) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *bidirectionalStream) Write(p []byte) (int, error) { return s.writer.Write(p) }

// orchestratorWrapper embeds the real orchestrator but returns our
// custom in-process proxy instead of cloudflared's ingress-based one.
type orchestratorWrapper struct {
	*orchestration.Orchestrator
	proxy connection.OriginProxy
}

func (w *orchestratorWrapper) GetOriginProxy() (connection.OriginProxy, error) {
	return w.proxy, nil
}

// cloudflaredVersion returns the version of the cloudflared module from build info.
func cloudflaredVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "DEV"
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/cloudflare/cloudflared" {
			return dep.Version
		}
	}
	return "DEV"
}

// RunTunnel starts a cloudflared tunnel that dispatches HTTP requests to
// the given handler in-process (no localhost TCP). It also starts a
// Prometheus metrics server on metricsAddr with /metrics, /ready, and
// /healthcheck endpoints.
func RunTunnel(ctx context.Context, handler http.Handler, rt http.RoundTripper, metricsAddr string, graceShutdownC <-chan struct{}) error {
	_ = os.Setenv("QUIC_GO_DISABLE_ECN", "1")

	log := zerolog.New(os.Stdout).With().Timestamp().Logger()

	version := cloudflaredVersion()
	platform := runtime.GOOS + "/" + runtime.GOARCH

	// Parse tunnel token.
	token, err := parseTunnelToken(os.Getenv("TUNNEL_TOKEN"))
	if err != nil {
		return fmt.Errorf("parsing tunnel token: %w", err)
	}
	creds := token.Credentials()
	namedTunnel := &connection.TunnelProperties{Credentials: creds}

	// Feature selector + client config.
	featureSelector, err := features.NewFeatureSelector(ctx, creds.AccountTag, nil, false, &log)
	if err != nil {
		return fmt.Errorf("creating feature selector: %w", err)
	}
	clientConfig, err := cfdclient.NewConfig(version, platform, featureSelector)
	if err != nil {
		return fmt.Errorf("creating client config: %w", err)
	}
	connectorID := clientConfig.ConnectorID

	// Protocol selector.
	protocolSelector, err := connection.NewProtocolSelector(
		connection.HTTP2.String(),
		creds.AccountTag,
		true,  // tunnelTokenProvided
		false, // needPQ
		edgediscovery.ProtocolPercentage,
		connection.ResolveTTL,
		&log,
	)
	if err != nil {
		return fmt.Errorf("creating protocol selector: %w", err)
	}

	// Edge TLS configs.
	edgeTLSConfigs := make(map[connection.Protocol]*tls.Config)
	for _, p := range connection.ProtocolList {
		settings := p.TLSSettings()
		tlsCfg, err := createTunnelTLSConfig(settings.ServerName)
		if err != nil {
			return fmt.Errorf("creating TLS config for %v: %w", p, err)
		}
		if len(settings.NextProtos) > 0 {
			tlsCfg.NextProtos = settings.NextProtos
		}
		edgeTLSConfigs[p] = tlsCfg
	}

	// Origin dialer (needed by supervisor for session manager).
	warpRouting := ingress.NewWarpRoutingConfig(&config.WarpRoutingConfig{})
	originDialerService := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer: ingress.NewDialer(warpRouting),
	}, &log)

	// DNS resolver service (supervisor calls StartRefreshLoop on it).
	originMetrics := origins.NewMetrics(prometheus.DefaultRegisterer)
	dnsService := origins.NewDNSResolverService(origins.NewDNSDialer(), &log, originMetrics)
	originDialerService.AddReservedService(dnsService, []netip.AddrPort{origins.VirtualDNSServiceAddr})

	// Observer.
	observer := connection.NewObserver(&log, &log)

	// Metrics server.
	tracker := tunnelstate.NewConnTracker(&log)
	observer.RegisterSink(tracker)

	metricsListener, err := net.Listen("tcp", metricsAddr)
	if err != nil {
		return fmt.Errorf("listening on metrics address %s: %w", metricsAddr, err)
	}
	go func() {
		readyServer := cfdmetrics.NewReadyServer(connectorID, tracker)
		diagHandler := diagnostic.NewDiagnosticHandler(
			&log, 0, nil, creds.TunnelID, connectorID, tracker, map[string]string{}, nil,
		)
		metricsCfg := cfdmetrics.Config{
			ReadyServer:       readyServer,
			DiagnosticHandler: diagHandler,
		}
		if err := cfdmetrics.ServeMetrics(metricsListener, ctx, metricsCfg, &log); err != nil {
			log.Err(err).Msg("Metrics server error")
		}
	}()

	// Tunnel config.
	tunnelConfig := &supervisor.TunnelConfig{
		ClientConfig:                        clientConfig,
		GracePeriod:                         30 * time.Second,
		Region:                              creds.Endpoint,
		EdgeIPVersion:                       allregions.Auto,
		HAConnections:                       4,
		Tags:                                []pogs.Tag{{Name: "ID", Value: connectorID.String()}},
		Log:                                 &log,
		LogTransport:                        &log,
		Observer:                            observer,
		ReportedVersion:                     version,
		Retries:                             5,
		MaxEdgeAddrRetries:                  8,
		NamedTunnel:                         namedTunnel,
		ProtocolSelector:                    protocolSelector,
		EdgeTLSConfigs:                      edgeTLSConfigs,
		OriginDNSService:                    dnsService,
		OriginDialerService:                 originDialerService,
		RPCTimeout:                          5 * time.Second,
		QUICConnectionLevelFlowControlLimit: 30 << 20,
		QUICStreamLevelFlowControlLimit:     6 << 20,
	}

	// Orchestrator (real one, for flow limiting and config ack to edge).
	ingressRules := ingress.Ingress{}
	orchestratorConfig := &orchestration.Config{
		Ingress:             &ingressRules,
		WarpRouting:         warpRouting,
		OriginDialerService: originDialerService,
	}
	orch, err := orchestration.NewOrchestrator(ctx, orchestratorConfig, tunnelConfig.Tags, nil, &log)
	if err != nil {
		return fmt.Errorf("creating orchestrator: %w", err)
	}

	// Wrap orchestrator to return our in-process proxy.
	wrapper := &orchestratorWrapper{
		Orchestrator: orch,
		proxy:        &originProxy{handler: handler, rt: rt, log: &log},
	}

	connectedSignal := signal.New(make(chan struct{}))
	reconnectCh := make(chan supervisor.ReconnectSignal, tunnelConfig.HAConnections)

	return supervisor.StartTunnelDaemon(ctx, tunnelConfig, wrapper, connectedSignal, reconnectCh, graceShutdownC)
}

func parseTunnelToken(tokenStr string) (*connection.TunnelToken, error) {
	content, err := base64.StdEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, err
	}
	var token connection.TunnelToken
	if err := json.Unmarshal(content, &token); err != nil {
		return nil, err
	}
	return &token, nil
}

func createTunnelTLSConfig(serverName string) (*tls.Config, error) {
	tlsCfg, err := tlsconfig.GetConfig(&tlsconfig.TLSParameters{ServerName: serverName})
	if err != nil {
		return nil, err
	}
	if tlsCfg.RootCAs == nil {
		rootCAs, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("getting system cert pool: %w", err)
		}
		cfCAs, err := tlsconfig.GetCloudflareRootCA()
		if err != nil {
			return nil, fmt.Errorf("getting Cloudflare root CAs: %w", err)
		}
		for _, ca := range cfCAs {
			rootCAs.AddCert(ca)
		}
		tlsCfg.RootCAs = rootCAs
	}
	return tlsCfg, nil
}
