/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/mitchellh/hashstructure"

	apiv1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"

	"k8s.io/ingress-nginx/internal/ingress"
	"k8s.io/ingress-nginx/internal/ingress/annotations/proxy"
	ngx_config "k8s.io/ingress-nginx/internal/ingress/controller/config"
	"k8s.io/ingress-nginx/internal/k8s"
)

const (
	defUpstreamName = "upstream-default-backend"
	defServerName   = "_"
	rootLocation    = "/"
)

// Configuration contains all the settings required by an Ingress controller
type Configuration struct {
	APIServerHost  string
	KubeConfigFile string
	Client         clientset.Interface

	ResyncPeriod time.Duration

	ConfigMapName  string
	DefaultService string

	Namespace string

	ForceNamespaceIsolation bool

	DefaultHealthzURL     string
	HealthCheckTimeout    time.Duration
	DefaultSSLCertificate string

	// +optional
	PublishService       string
	PublishStatusAddress string

	UpdateStatus           bool
	UseNodeInternalIP      bool
	ElectionID             string
	UpdateStatusOnShutdown bool

	SortBackends bool

	ListenPorts *ngx_config.ListenPorts

	EnableSSLPassthrough bool

	EnableProfiling bool

	EnableSSLChainCompletion bool

	FakeCertificatePath string
	FakeCertificateSHA  string

	SyncRateLimit float32

	DynamicCertificatesEnabled bool
}

// GetPublishService returns the Service used to set the load-balancer status of Ingresses.
func (n NGINXController) GetPublishService() *apiv1.Service {
	s, err := n.store.GetService(n.cfg.PublishService)
	if err != nil {
		return nil
	}

	return s
}

// syncIngress collects all the pieces required to assemble the NGINX
// configuration file and passes the resulting data structures to the backend
// (OnUpdate) when a reload is deemed necessary.
func (n *NGINXController) syncIngress(interface{}) error {
	n.syncRateLimiter.Accept()

	if n.syncQueue.IsShuttingDown() {
		return nil
	}

	// sort Ingresses using the ResourceVersion field
	ings := n.store.ListIngresses()
	sort.SliceStable(ings, func(i, j int) bool {
		ir := ings[i].ResourceVersion
		jr := ings[j].ResourceVersion
		return ir < jr
	})

	upstreams, servers := n.getBackendServers(ings)
	var passUpstreams []*ingress.SSLPassthroughBackend

	hosts := sets.NewString()

	for _, server := range servers {
		if !hosts.Has(server.Hostname) {
			hosts.Insert(server.Hostname)
		}

		if !server.SSLPassthrough {
			continue
		}

		for _, loc := range server.Locations {
			if loc.Path != rootLocation {
				glog.Warningf("Ignoring SSL Passthrough for location %q in server %q", loc.Path, server.Hostname)
				continue
			}
			passUpstreams = append(passUpstreams, &ingress.SSLPassthroughBackend{
				Backend:  loc.Backend,
				Hostname: server.Hostname,
				Service:  loc.Service,
				Port:     loc.Port,
			})
			break
		}
	}

	pcfg := &ingress.Configuration{
		Backends:              upstreams,
		Servers:               servers,
		PassthroughBackends:   passUpstreams,
		BackendConfigChecksum: n.store.GetBackendConfiguration().Checksum,
	}

	if n.runningConfig.Equal(pcfg) {
		glog.V(3).Infof("No configuration change detected, skipping backend reload.")
		return nil
	}

	if !n.IsDynamicConfigurationEnough(pcfg) {
		glog.Infof("Configuration changes detected, backend reload required.")

		hash, _ := hashstructure.Hash(pcfg, &hashstructure.HashOptions{
			TagName: "json",
		})

		pcfg.ConfigurationChecksum = fmt.Sprintf("%v", hash)

		err := n.OnUpdate(*pcfg)
		if err != nil {
			n.metricCollector.IncReloadErrorCount()
			n.metricCollector.ConfigSuccess(hash, false)
			glog.Errorf("Unexpected failure reloading the backend:\n%v", err)
			return err
		}

		n.metricCollector.SetHosts(hosts)

		glog.Infof("Backend successfully reloaded.")
		n.metricCollector.ConfigSuccess(hash, true)
		n.metricCollector.IncReloadCount()
		n.metricCollector.SetSSLExpireTime(servers)
	}

	retry := wait.Backoff{
		Steps:    15,
		Duration: 1 * time.Second,
		Factor:   0.8,
		Jitter:   0.1,
	}

	err := wait.ExponentialBackoff(retry, func() (bool, error) {
		err := configureDynamically(pcfg, n.cfg.ListenPorts.Status, n.cfg.DynamicCertificatesEnabled)
		if err == nil {
			glog.V(2).Infof("Dynamic reconfiguration succeeded.")
			return true, nil
		}

		glog.Warningf("Dynamic reconfiguration failed: %v", err)
		return false, err
	})
	if err != nil {
		glog.Errorf("Unexpected failure reconfiguring NGINX:\n%v", err)
		return err
	}

	ri := getRemovedIngresses(n.runningConfig, pcfg)
	re := getRemovedHosts(n.runningConfig, pcfg)
	n.metricCollector.RemoveMetrics(ri, re)

	n.runningConfig = pcfg

	return nil
}

// getDefaultUpstream returns the upstream associated with the default backend.
// Configures the upstream to return HTTP code 503 in case of error.
func (n *NGINXController) getDefaultUpstream() *ingress.Backend {
	upstream := &ingress.Backend{
		Name: defUpstreamName,
	}
	svcKey := n.cfg.DefaultService

	if len(svcKey) == 0 {
		upstream.Endpoints = append(upstream.Endpoints, n.DefaultEndpoint())
		return upstream
	}

	svc, err := n.store.GetService(svcKey)
	if err != nil {
		glog.Warningf("Error getting default backend %q: %v", svcKey, err)
		upstream.Endpoints = append(upstream.Endpoints, n.DefaultEndpoint())
		return upstream
	}

	endps := getEndpoints(svc, &svc.Spec.Ports[0], apiv1.ProtocolTCP, n.store.GetServiceEndpoints)
	if len(endps) == 0 {
		glog.Warningf("Service %q does not have any active Endpoint", svcKey)
		endps = []ingress.Endpoint{n.DefaultEndpoint()}
	}

	upstream.Service = svc
	upstream.Endpoints = append(upstream.Endpoints, endps...)
	return upstream
}

// getBackendServers returns a list of Upstream and Server to be used by the
// backend.  An upstream can be used in multiple servers if the namespace,
// service name and port are the same.
func (n *NGINXController) getBackendServers(ingresses []*extensions.Ingress) ([]*ingress.Backend, []*ingress.Server) {
	du := n.getDefaultUpstream()
	upstreams := n.createUpstreams(ingresses, du)
	servers := n.createServers(ingresses, upstreams, du)

	for _, ing := range ingresses {
		ingKey := k8s.MetaNamespaceKey(ing)

		anns, err := n.store.GetIngressAnnotations(ingKey)
		if err != nil {
			glog.Errorf("Error getting Ingress annotations %q: %v", ingKey, err)
		}

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}

			server := servers[host]
			if server == nil {
				server = servers[defServerName]
			}

			if rule.HTTP == nil &&
				host != defServerName {
				glog.V(3).Infof("Ingress %q does not contain any HTTP rule, using default backend", ingKey)
				continue
			}

			if server.AuthTLSError == "" && anns.CertificateAuth.AuthTLSError != "" {
				server.AuthTLSError = anns.CertificateAuth.AuthTLSError
			}

			if server.CertificateAuth.CAFileName == "" {
				server.CertificateAuth = anns.CertificateAuth
				if server.CertificateAuth.Secret != "" && server.CertificateAuth.CAFileName == "" {
					glog.V(3).Infof("Secret %q has no 'ca.crt' key, mutual authentication disabled for Ingress %q",
						server.CertificateAuth.Secret, ingKey)
				}
			} else {
				glog.V(3).Infof("Server %q is already configured for mutual authentication (Ingress %q)",
					server.Hostname, ingKey)
			}

			if rule.HTTP == nil {
				glog.V(3).Infof("Ingress %q does not contain any HTTP rule, using default backend", ingKey)
				continue
			}

			for _, path := range rule.HTTP.Paths {
				upsName := upstreamName(ing.Namespace, path.Backend.ServiceName, path.Backend.ServicePort)

				ups := upstreams[upsName]

				// Backend is not referenced to by a server
				if ups.NoServer {
					continue
				}

				nginxPath := rootLocation
				if path.Path != "" {
					nginxPath = path.Path
				}

				addLoc := true
				for _, loc := range server.Locations {
					if loc.Path == nginxPath {
						addLoc = false

						if !loc.IsDefBackend {
							glog.V(3).Infof("Location %q already configured for server %q with upstream %q (Ingress %q)",
								loc.Path, server.Hostname, loc.Backend, ingKey)
							break
						}

						glog.V(3).Infof("Replacing location %q for server %q with upstream %q to use upstream %q (Ingress %q)",
							loc.Path, server.Hostname, loc.Backend, ups.Name, ingKey)

						loc.Backend = ups.Name
						loc.IsDefBackend = false
						loc.Port = ups.Port
						loc.Service = ups.Service
						loc.Ingress = ing
						loc.BasicDigestAuth = anns.BasicDigestAuth
						loc.ClientBodyBufferSize = anns.ClientBodyBufferSize
						loc.ConfigurationSnippet = anns.ConfigurationSnippet
						loc.CorsConfig = anns.CorsConfig
						loc.ExternalAuth = anns.ExternalAuth
						loc.Proxy = anns.Proxy
						loc.RateLimit = anns.RateLimit
						loc.Redirect = anns.Redirect
						loc.Rewrite = anns.Rewrite
						loc.UpstreamVhost = anns.UpstreamVhost
						loc.Whitelist = anns.Whitelist
						loc.Denied = anns.Denied
						loc.XForwardedPrefix = anns.XForwardedPrefix
						loc.UsePortInRedirects = anns.UsePortInRedirects
						loc.Connection = anns.Connection
						loc.Logs = anns.Logs
						loc.LuaRestyWAF = anns.LuaRestyWAF
						loc.InfluxDB = anns.InfluxDB
						loc.DefaultBackend = anns.DefaultBackend
						loc.BackendProtocol = anns.BackendProtocol

						if loc.Redirect.FromToWWW {
							server.RedirectFromToWWW = true
						}
						break
					}
				}

				// new location
				if addLoc {
					glog.V(3).Infof("Adding location %q for server %q with upstream %q (Ingress %q)",
						nginxPath, server.Hostname, ups.Name, ingKey)

					loc := &ingress.Location{
						Path:                 nginxPath,
						Backend:              ups.Name,
						IsDefBackend:         false,
						Service:              ups.Service,
						Port:                 ups.Port,
						Ingress:              ing,
						BasicDigestAuth:      anns.BasicDigestAuth,
						ClientBodyBufferSize: anns.ClientBodyBufferSize,
						ConfigurationSnippet: anns.ConfigurationSnippet,
						CorsConfig:           anns.CorsConfig,
						ExternalAuth:         anns.ExternalAuth,
						Proxy:                anns.Proxy,
						RateLimit:            anns.RateLimit,
						Redirect:             anns.Redirect,
						Rewrite:              anns.Rewrite,
						UpstreamVhost:        anns.UpstreamVhost,
						Whitelist:            anns.Whitelist,
						Denied:               anns.Denied,
						XForwardedPrefix:     anns.XForwardedPrefix,
						UsePortInRedirects:   anns.UsePortInRedirects,
						Connection:           anns.Connection,
						Logs:                 anns.Logs,
						LuaRestyWAF:          anns.LuaRestyWAF,
						InfluxDB:             anns.InfluxDB,
						DefaultBackend:       anns.DefaultBackend,
						BackendProtocol:      anns.BackendProtocol,
					}

					if loc.Redirect.FromToWWW {
						server.RedirectFromToWWW = true
					}
					server.Locations = append(server.Locations, loc)
				}

				if ups.SessionAffinity.AffinityType == "" {
					ups.SessionAffinity.AffinityType = anns.SessionAffinity.Type
				}

				if anns.SessionAffinity.Type == "cookie" {
					ups.SessionAffinity.CookieSessionAffinity.Name = anns.SessionAffinity.Cookie.Name
					ups.SessionAffinity.CookieSessionAffinity.Hash = anns.SessionAffinity.Cookie.Hash

					locs := ups.SessionAffinity.CookieSessionAffinity.Locations
					if _, ok := locs[host]; !ok {
						locs[host] = []string{}
					}

					locs[host] = append(locs[host], path.Path)
				}
			}
		}

		if anns.Canary.Enabled {
			glog.Infof("Canary ingress %v detected. Finding eligible backends to merge into.", ing.Name)
			mergeAlternativeBackends(ing, upstreams, servers)
		}
	}

	aUpstreams := make([]*ingress.Backend, 0, len(upstreams))

	for _, upstream := range upstreams {
		isHTTPSfrom := []*ingress.Server{}
		for _, server := range servers {
			for _, location := range server.Locations {
				if upstream.Name == location.Backend {
					if len(upstream.Endpoints) == 0 {
						glog.V(3).Infof("Upstream %q has no active Endpoint", upstream.Name)

						location.Backend = "" // for nginx.tmpl checking

						// check if the location contains endpoints and a custom default backend
						if location.DefaultBackend != nil {
							sp := location.DefaultBackend.Spec.Ports[0]
							endps := getEndpoints(location.DefaultBackend, &sp, apiv1.ProtocolTCP, n.store.GetServiceEndpoints)
							if len(endps) > 0 {
								glog.V(3).Infof("Using custom default backend for location %q in server %q (Service \"%v/%v\")",
									location.Path, server.Hostname, location.DefaultBackend.Namespace, location.DefaultBackend.Name)

								nb := upstream.DeepCopy()
								name := fmt.Sprintf("custom-default-backend-%v", upstream.Name)
								nb.Name = name
								nb.Endpoints = endps
								aUpstreams = append(aUpstreams, nb)
								location.Backend = name
							}
						}
					}

					if server.SSLPassthrough {
						if location.Path == rootLocation {
							if location.Backend == defUpstreamName {
								glog.Warningf("Server %q has no default backend, ignoring SSL Passthrough.", server.Hostname)
								continue
							}
							isHTTPSfrom = append(isHTTPSfrom, server)
						}
					}
				}
			}
		}

		if len(isHTTPSfrom) > 0 {
			upstream.SSLPassthrough = true
		}
	}

	// create the list of upstreams and skip those without Endpoints
	for _, upstream := range upstreams {
		if len(upstream.Endpoints) == 0 {
			continue
		}
		aUpstreams = append(aUpstreams, upstream)
	}

	aServers := make([]*ingress.Server, 0, len(servers))
	for _, value := range servers {
		sort.SliceStable(value.Locations, func(i, j int) bool {
			return value.Locations[i].Path > value.Locations[j].Path
		})

		sort.SliceStable(value.Locations, func(i, j int) bool {
			return len(value.Locations[i].Path) > len(value.Locations[j].Path)
		})
		aServers = append(aServers, value)
	}

	sort.SliceStable(aUpstreams, func(a, b int) bool {
		return aUpstreams[a].Name < aUpstreams[b].Name
	})

	sort.SliceStable(aServers, func(i, j int) bool {
		return aServers[i].Hostname < aServers[j].Hostname
	})

	return aUpstreams, aServers
}

// createUpstreams creates the NGINX upstreams (Endpoints) for each Service
// referenced in Ingress rules.
func (n *NGINXController) createUpstreams(data []*extensions.Ingress, du *ingress.Backend) map[string]*ingress.Backend {
	upstreams := make(map[string]*ingress.Backend)
	upstreams[defUpstreamName] = du

	for _, ing := range data {
		ingKey := k8s.MetaNamespaceKey(ing)

		anns, err := n.store.GetIngressAnnotations(ingKey)
		if err != nil {
			glog.Errorf("Error getting Ingress annotations %q: %v", ingKey, err)
		}

		var defBackend string
		if ing.Spec.Backend != nil {
			defBackend = upstreamName(ing.Namespace, ing.Spec.Backend.ServiceName, ing.Spec.Backend.ServicePort)

			glog.V(3).Infof("Creating upstream %q", defBackend)
			upstreams[defBackend] = newUpstream(defBackend)
			if upstreams[defBackend].SecureCACert.Secret == "" {
				upstreams[defBackend].SecureCACert = anns.SecureUpstream.CACert
			}
			if upstreams[defBackend].UpstreamHashBy == "" {
				upstreams[defBackend].UpstreamHashBy = anns.UpstreamHashBy
			}
			if upstreams[defBackend].LoadBalancing == "" {
				upstreams[defBackend].LoadBalancing = anns.LoadBalancing
			}

			svcKey := fmt.Sprintf("%v/%v", ing.Namespace, ing.Spec.Backend.ServiceName)

			// add the service ClusterIP as a single Endpoint instead of individual Endpoints
			if anns.ServiceUpstream {
				endpoint, err := n.getServiceClusterEndpoint(svcKey, ing.Spec.Backend)
				if err != nil {
					glog.Errorf("Failed to determine a suitable ClusterIP Endpoint for Service %q: %v", svcKey, err)
				} else {
					upstreams[defBackend].Endpoints = []ingress.Endpoint{endpoint}
				}
			}

			// configure traffic shaping for canary
			if anns.Canary.Enabled {
				upstreams[defBackend].NoServer = true
				upstreams[defBackend].TrafficShapingPolicy = ingress.TrafficShapingPolicy{
					Weight: anns.Canary.Weight,
					Header: anns.Canary.Header,
					Cookie: anns.Canary.Cookie,
				}
			}

			if len(upstreams[defBackend].Endpoints) == 0 {
				endps, err := n.serviceEndpoints(svcKey, ing.Spec.Backend.ServicePort.String())
				upstreams[defBackend].Endpoints = append(upstreams[defBackend].Endpoints, endps...)
				if err != nil {
					glog.Warningf("Error creating upstream %q: %v", defBackend, err)
				}
			}

		}

		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}

			for _, path := range rule.HTTP.Paths {
				name := upstreamName(ing.Namespace, path.Backend.ServiceName, path.Backend.ServicePort)

				if _, ok := upstreams[name]; ok {
					continue
				}

				glog.V(3).Infof("Creating upstream %q", name)
				upstreams[name] = newUpstream(name)
				upstreams[name].Port = path.Backend.ServicePort

				if upstreams[name].SecureCACert.Secret == "" {
					upstreams[name].SecureCACert = anns.SecureUpstream.CACert
				}

				if upstreams[name].UpstreamHashBy == "" {
					upstreams[name].UpstreamHashBy = anns.UpstreamHashBy
				}

				if upstreams[name].LoadBalancing == "" {
					upstreams[name].LoadBalancing = anns.LoadBalancing
				}

				svcKey := fmt.Sprintf("%v/%v", ing.Namespace, path.Backend.ServiceName)

				// add the service ClusterIP as a single Endpoint instead of individual Endpoints
				if anns.ServiceUpstream {
					endpoint, err := n.getServiceClusterEndpoint(svcKey, &path.Backend)
					if err != nil {
						glog.Errorf("Failed to determine a suitable ClusterIP Endpoint for Service %q: %v", svcKey, err)
					} else {
						upstreams[name].Endpoints = []ingress.Endpoint{endpoint}
					}
				}

				// configure traffic shaping for canary
				if anns.Canary.Enabled {
					upstreams[name].NoServer = true
					upstreams[name].TrafficShapingPolicy = ingress.TrafficShapingPolicy{
						Weight: anns.Canary.Weight,
						Header: anns.Canary.Header,
						Cookie: anns.Canary.Cookie,
					}
				}

				if len(upstreams[name].Endpoints) == 0 {
					endp, err := n.serviceEndpoints(svcKey, path.Backend.ServicePort.String())
					if err != nil {
						glog.Warningf("Error obtaining Endpoints for Service %q: %v", svcKey, err)
						continue
					}
					upstreams[name].Endpoints = endp
				}

				s, err := n.store.GetService(svcKey)
				if err != nil {
					glog.Warningf("Error obtaining Service %q: %v", svcKey, err)
					continue
				}

				upstreams[name].Service = s
			}
		}
	}

	return upstreams
}

// getServiceClusterEndpoint returns an Endpoint corresponding to the ClusterIP
// field of a Service.
func (n *NGINXController) getServiceClusterEndpoint(svcKey string, backend *extensions.IngressBackend) (endpoint ingress.Endpoint, err error) {
	svc, err := n.store.GetService(svcKey)
	if err != nil {
		return endpoint, fmt.Errorf("service %q does not exist", svcKey)
	}

	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return endpoint, fmt.Errorf("no ClusterIP found for Service %q", svcKey)
	}

	endpoint.Address = svc.Spec.ClusterIP

	// if the Service port is referenced by name in the Ingress, lookup the
	// actual port in the service spec
	if backend.ServicePort.Type == intstr.String {
		var port int32 = -1
		for _, svcPort := range svc.Spec.Ports {
			if svcPort.Name == backend.ServicePort.String() {
				port = svcPort.Port
				break
			}
		}
		if port == -1 {
			return endpoint, fmt.Errorf("service %q does not have a port named %q", svc.Name, backend.ServicePort)
		}
		endpoint.Port = fmt.Sprintf("%d", port)
	} else {
		endpoint.Port = backend.ServicePort.String()
	}

	return endpoint, err
}

// serviceEndpoints returns the upstream servers (Endpoints) associated with a Service.
func (n *NGINXController) serviceEndpoints(svcKey, backendPort string) ([]ingress.Endpoint, error) {
	svc, err := n.store.GetService(svcKey)

	var upstreams []ingress.Endpoint
	if err != nil {
		return upstreams, err
	}

	glog.V(3).Infof("Obtaining ports information for Service %q", svcKey)
	for _, servicePort := range svc.Spec.Ports {
		// targetPort could be a string, use either the port name or number (int)
		if strconv.Itoa(int(servicePort.Port)) == backendPort ||
			servicePort.TargetPort.String() == backendPort ||
			servicePort.Name == backendPort {

			endps := getEndpoints(svc, &servicePort, apiv1.ProtocolTCP, n.store.GetServiceEndpoints)
			if len(endps) == 0 {
				glog.Warningf("Service %q does not have any active Endpoint.", svcKey)
			}

			if n.cfg.SortBackends {
				sort.SliceStable(endps, func(i, j int) bool {
					iName := endps[i].Address
					jName := endps[j].Address
					if iName != jName {
						return iName < jName
					}

					return endps[i].Port < endps[j].Port
				})
			}
			upstreams = append(upstreams, endps...)
			break
		}
	}

	// Ingress with an ExternalName Service and no port defined for that Service
	if len(svc.Spec.Ports) == 0 && svc.Spec.Type == apiv1.ServiceTypeExternalName {
		externalPort, err := strconv.Atoi(backendPort)
		if err != nil {
			glog.Warningf("Only numeric ports are allowed in ExternalName Services: %q is not a valid port number.", backendPort)
			return upstreams, nil
		}

		servicePort := apiv1.ServicePort{
			Protocol:   "TCP",
			Port:       int32(externalPort),
			TargetPort: intstr.FromString(backendPort),
		}
		endps := getEndpoints(svc, &servicePort, apiv1.ProtocolTCP, n.store.GetServiceEndpoints)
		if len(endps) == 0 {
			glog.Warningf("Service %q does not have any active Endpoint.", svcKey)
			return upstreams, nil
		}

		upstreams = append(upstreams, endps...)
		return upstreams, nil
	}

	if !n.cfg.SortBackends {
		rand.Seed(time.Now().UnixNano())
		for i := range upstreams {
			j := rand.Intn(i + 1)
			upstreams[i], upstreams[j] = upstreams[j], upstreams[i]
		}
	}

	return upstreams, nil
}

// createServers builds a map of host name to Server structs from a map of
// already computed Upstream structs. Each Server is configured with at least
// one root location, which uses a default backend if left unspecified.
func (n *NGINXController) createServers(data []*extensions.Ingress,
	upstreams map[string]*ingress.Backend,
	du *ingress.Backend) map[string]*ingress.Server {

	servers := make(map[string]*ingress.Server, len(data))
	aliases := make(map[string]string, len(data))

	bdef := n.store.GetDefaultBackend()
	ngxProxy := proxy.Config{
		BodySize:          bdef.ProxyBodySize,
		ConnectTimeout:    bdef.ProxyConnectTimeout,
		SendTimeout:       bdef.ProxySendTimeout,
		ReadTimeout:       bdef.ProxyReadTimeout,
		BufferSize:        bdef.ProxyBufferSize,
		CookieDomain:      bdef.ProxyCookieDomain,
		CookiePath:        bdef.ProxyCookiePath,
		NextUpstream:      bdef.ProxyNextUpstream,
		NextUpstreamTries: bdef.ProxyNextUpstreamTries,
		RequestBuffering:  bdef.ProxyRequestBuffering,
		ProxyRedirectFrom: bdef.ProxyRedirectFrom,
		ProxyBuffering:    bdef.ProxyBuffering,
	}

	// generated on Start() with createDefaultSSLCertificate()
	defaultPemFileName := n.cfg.FakeCertificatePath
	defaultPemSHA := n.cfg.FakeCertificateSHA

	// read custom default SSL certificate, fall back to generated default certificate
	defaultCertificate, err := n.store.GetLocalSSLCert(n.cfg.DefaultSSLCertificate)
	if err == nil {
		defaultPemFileName = defaultCertificate.PemFileName
		defaultPemSHA = defaultCertificate.PemSHA
	}

	// initialize default server and root location
	servers[defServerName] = &ingress.Server{
		Hostname: defServerName,
		SSLCert: ingress.SSLCert{
			PemFileName: defaultPemFileName,
			PemSHA:      defaultPemSHA,
		},
		Locations: []*ingress.Location{
			{
				Path:         rootLocation,
				IsDefBackend: true,
				Backend:      du.Name,
				Proxy:        ngxProxy,
				Service:      du.Service,
			},
		}}

	// initialize all other servers
	for _, ing := range data {
		ingKey := k8s.MetaNamespaceKey(ing)

		anns, err := n.store.GetIngressAnnotations(ingKey)
		if err != nil {
			glog.Errorf("Error getting Ingress annotations %q: %v", ingKey, err)
		}

		// default upstream name
		un := du.Name

		if ing.Spec.Backend != nil {
			defUpstream := fmt.Sprintf("%v-%v-%v", ing.Namespace, ing.Spec.Backend.ServiceName, ing.Spec.Backend.ServicePort.String())

			if backendUpstream, ok := upstreams[defUpstream]; ok {
				// use backend specified in Ingress as the default backend for all its rules
				un = backendUpstream.Name

				// special "catch all" case, Ingress with a backend but no rule
				defLoc := servers[defServerName].Locations[0]
				if defLoc.IsDefBackend && len(ing.Spec.Rules) == 0 {
					glog.Infof("Ingress %q defines a backend but no rule. Using it to configure the catch-all server %q",
						ingKey, defServerName)

					defLoc.IsDefBackend = false
					defLoc.Backend = backendUpstream.Name
					defLoc.Service = backendUpstream.Service
					defLoc.Ingress = ing

					// customize using Ingress annotations
					defLoc.Logs = anns.Logs
					defLoc.BasicDigestAuth = anns.BasicDigestAuth
					defLoc.ClientBodyBufferSize = anns.ClientBodyBufferSize
					defLoc.ConfigurationSnippet = anns.ConfigurationSnippet
					defLoc.CorsConfig = anns.CorsConfig
					defLoc.ExternalAuth = anns.ExternalAuth
					defLoc.Proxy = anns.Proxy
					defLoc.RateLimit = anns.RateLimit
					// TODO: Redirect and rewrite can affect the catch all behavior, skip for now
					// defLoc.Redirect = anns.Redirect
					// defLoc.Rewrite = anns.Rewrite
					defLoc.UpstreamVhost = anns.UpstreamVhost
					defLoc.Whitelist = anns.Whitelist
					defLoc.Denied = anns.Denied
					defLoc.LuaRestyWAF = anns.LuaRestyWAF
					defLoc.InfluxDB = anns.InfluxDB
					defLoc.BackendProtocol = anns.BackendProtocol
				} else {
					glog.V(3).Infof("Ingress %q defines both a backend and rules. Using its backend as default upstream for all its rules.",
						ingKey)
				}
			}
		}

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}
			if _, ok := servers[host]; ok {
				// server already configured
				continue
			}

			servers[host] = &ingress.Server{
				Hostname: host,
				Locations: []*ingress.Location{
					{
						Path:         rootLocation,
						IsDefBackend: true,
						Backend:      un,
						Proxy:        ngxProxy,
						Service:      &apiv1.Service{},
					},
				},
				SSLPassthrough: anns.SSLPassthrough,
				SSLCiphers:     anns.SSLCiphers,
			}
		}
	}

	// configure default location, alias, and SSL
	for _, ing := range data {
		ingKey := k8s.MetaNamespaceKey(ing)

		anns, err := n.store.GetIngressAnnotations(ingKey)
		if err != nil {
			glog.Errorf("Error getting Ingress annotations %q: %v", ingKey, err)
		}

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}

			if anns.Alias != "" {
				if servers[host].Alias == "" {
					servers[host].Alias = anns.Alias
					if _, ok := aliases["Alias"]; !ok {
						aliases["Alias"] = host
					}
				} else {
					glog.Warningf("Aliases already configured for server %q, skipping (Ingress %q)",
						host, ingKey)
				}
			}

			if anns.ServerSnippet != "" {
				if servers[host].ServerSnippet == "" {
					servers[host].ServerSnippet = anns.ServerSnippet
				} else {
					glog.Warningf("Server snippet already configured for server %q, skipping (Ingress %q)",
						host, ingKey)
				}
			}

			// only add SSL ciphers if the server does not have them previously configured
			if servers[host].SSLCiphers == "" && anns.SSLCiphers != "" {
				servers[host].SSLCiphers = anns.SSLCiphers
			}

			// only add a certificate if the server does not have one previously configured
			if servers[host].SSLCert.PemFileName != "" {
				continue
			}

			if len(ing.Spec.TLS) == 0 {
				glog.V(3).Infof("Ingress %q does not contains a TLS section.", ingKey)
				continue
			}

			tlsSecretName := extractTLSSecretName(host, ing, n.store.GetLocalSSLCert)

			if tlsSecretName == "" {
				glog.V(3).Infof("Host %q is listed in the TLS section but secretName is empty. Using default certificate.", host)
				servers[host].SSLCert.PemFileName = defaultPemFileName
				servers[host].SSLCert.PemSHA = defaultPemSHA
				continue
			}

			secrKey := fmt.Sprintf("%v/%v", ing.Namespace, tlsSecretName)
			cert, err := n.store.GetLocalSSLCert(secrKey)
			if err != nil {
				glog.Warningf("Error getting SSL certificate %q: %v. Using default certificate", secrKey, err)
				servers[host].SSLCert.PemFileName = defaultPemFileName
				servers[host].SSLCert.PemSHA = defaultPemSHA
				continue
			}

			err = cert.Certificate.VerifyHostname(host)
			if err != nil {
				glog.Warningf("Unexpected error validating SSL certificate %q for server %q: %v", secrKey, host, err)
				glog.Warning("Validating certificate against DNS names. This will be deprecated in a future version.")
				// check the Common Name field
				// https://github.com/golang/go/issues/22922
				err := verifyHostname(host, cert.Certificate)
				if err != nil {
					glog.Warningf("SSL certificate %q does not contain a Common Name or Subject Alternative Name for server %q: %v",
						secrKey, host, err)
					glog.Warningf("Using default certificate")
					servers[host].SSLCert.PemFileName = defaultPemFileName
					servers[host].SSLCert.PemSHA = defaultPemSHA
					continue
				}
			}

			if n.cfg.DynamicCertificatesEnabled {
				// useless placeholders: just to shut up NGINX configuration loader errors:
				cert.PemFileName = defaultPemFileName
				cert.PemSHA = defaultPemSHA
			}

			servers[host].SSLCert = *cert

			if cert.ExpireTime.Before(time.Now().Add(240 * time.Hour)) {
				glog.Warningf("SSL certificate for server %q is about to expire (%v)", host, cert.ExpireTime)
			}
		}
	}

	for alias, host := range aliases {
		if _, ok := servers[alias]; ok {
			glog.Warningf("Conflicting hostname (%v) and alias (%v). Removing alias to avoid conflicts.", host, alias)
			servers[host].Alias = ""
		}
	}

	return servers
}

// Compares an Ingress of a potential alternative backend's rules with each existing server and finds matching host + path pairs.
// If a match is found, we know that this server should back the alternative backend and add the alternative backend
// to a backend's alternative list.
// If no match is found, then the serverless backend is deleted.
func mergeAlternativeBackends(ing *extensions.Ingress, upstreams map[string]*ingress.Backend,
	servers map[string]*ingress.Server) {

	// merge catch-all alternative backends
	if ing.Spec.Backend != nil {
		upsName := upstreamName(ing.Namespace, ing.Spec.Backend.ServiceName, ing.Spec.Backend.ServicePort)

		ups := upstreams[upsName]

		defLoc := servers[defServerName].Locations[0]

		glog.Infof("matching backend %v found for alternative backend %v",
			upstreams[defLoc.Backend].Name, ups.Name)

		upstreams[defLoc.Backend].AlternativeBackends =
			append(upstreams[defLoc.Backend].AlternativeBackends, ups.Name)
	}

	for _, rule := range ing.Spec.Rules {
		for _, path := range rule.HTTP.Paths {
			upsName := upstreamName(ing.Namespace, path.Backend.ServiceName, path.Backend.ServicePort)

			ups := upstreams[upsName]

			merged := false

			server := servers[rule.Host]

			// find matching paths
			for _, location := range server.Locations {
				if location.Backend == defUpstreamName {
					continue
				}

				if location.Path == path.Path && !upstreams[location.Backend].NoServer {
					glog.Infof("matching backend %v found for alternative backend %v",
						upstreams[location.Backend].Name, ups.Name)

					upstreams[location.Backend].AlternativeBackends =
						append(upstreams[location.Backend].AlternativeBackends, ups.Name)

					merged = true
				}
			}

			if !merged {
				glog.Warningf("unable to find real backend for alternative backend %v. Deleting.", ups.Name)
				delete(upstreams, ups.Name)
			}
		}
	}
}

// extractTLSSecretName returns the name of the Secret containing a SSL
// certificate for the given host name, or an empty string.
func extractTLSSecretName(host string, ing *extensions.Ingress,
	getLocalSSLCert func(string) (*ingress.SSLCert, error)) string {

	if ing == nil {
		return ""
	}

	// naively return Secret name from TLS spec if host name matches
	for _, tls := range ing.Spec.TLS {
		if sets.NewString(tls.Hosts...).Has(host) {
			return tls.SecretName
		}
	}

	// no TLS host matching host name, try each TLS host for matching SAN or CN
	for _, tls := range ing.Spec.TLS {

		if tls.SecretName == "" {
			// There's no secretName specified, so it will never be available
			continue
		}

		secrKey := fmt.Sprintf("%v/%v", ing.Namespace, tls.SecretName)

		cert, err := getLocalSSLCert(secrKey)
		if err != nil {
			glog.Warningf("Error getting SSL certificate %q: %v", secrKey, err)
			continue
		}

		if cert == nil { // for tests
			continue
		}

		err = cert.Certificate.VerifyHostname(host)
		if err != nil {
			continue
		}
		glog.V(3).Infof("Found SSL certificate matching host %q: %q", host, secrKey)
		return tls.SecretName
	}

	return ""
}

// getRemovedHosts returns a list of the hostsnames
// that are not associated anymore to the NGINX configuration.
func getRemovedHosts(rucfg, newcfg *ingress.Configuration) []string {
	old := sets.NewString()
	new := sets.NewString()

	for _, s := range rucfg.Servers {
		if !old.Has(s.Hostname) {
			old.Insert(s.Hostname)
		}
	}

	for _, s := range newcfg.Servers {
		if !new.Has(s.Hostname) {
			new.Insert(s.Hostname)
		}
	}

	return old.Difference(new).List()
}

func getRemovedIngresses(rucfg, newcfg *ingress.Configuration) []string {
	oldIngresses := sets.NewString()
	newIngresses := sets.NewString()

	for _, server := range rucfg.Servers {
		for _, location := range server.Locations {
			if location.Ingress == nil {
				continue
			}

			ingKey := k8s.MetaNamespaceKey(location.Ingress)
			if !oldIngresses.Has(ingKey) {
				oldIngresses.Insert(ingKey)
			}
		}
	}

	for _, server := range newcfg.Servers {
		for _, location := range server.Locations {
			if location.Ingress == nil {
				continue
			}

			ingKey := k8s.MetaNamespaceKey(location.Ingress)
			if !newIngresses.Has(ingKey) {
				newIngresses.Insert(ingKey)
			}
		}
	}

	return oldIngresses.Difference(newIngresses).List()
}
