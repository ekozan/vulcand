package service

import (
	"fmt"
	"github.com/coreos/go-etcd/etcd"
	"github.com/gorilla/mux"
	log "github.com/mailgun/gotools-log"
	runtime "github.com/mailgun/gotools-runtime"
	"github.com/mailgun/vulcan"
	"github.com/mailgun/vulcan/loadbalance/roundrobin"
	"github.com/mailgun/vulcan/location/httploc"
	"github.com/mailgun/vulcan/netutils"
	"github.com/mailgun/vulcan/route/hostroute"
	"github.com/mailgun/vulcan/route/pathroute"
	"github.com/mailgun/vulcand/api"
	. "github.com/mailgun/vulcand/backend"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"
)

type Service struct {
	client    *etcd.Client
	proxy     *vulcan.Proxy
	backend   Backend
	options   Options
	router    *hostroute.HostRouter
	apiRouter *mux.Router
	changes   chan *Change
}

func NewService(options Options) *Service {
	return &Service{
		options: options,
		changes: make(chan *Change),
	}
}

func (s *Service) Start() error {
	// Init logging
	log.Init([]*log.LogConfig{&log.LogConfig{Name: "console"}})

	backend, err := NewEtcdBackend(s.options.EtcdNodes, s.options.EtcdKey, s.options.EtcdConsistency, s.changes)
	if err != nil {
		return err
	}
	s.backend = backend

	if s.options.PidPath != "" {
		if err := runtime.WritePid(s.options.PidPath); err != nil {
			return fmt.Errorf("Failed to write PID file: %v\n", err)
		}
	}

	if err := s.createProxy(); err != nil {
		return err
	}

	if err := s.configureProxy(); err != nil {
		return err
	}

	if err := s.configureApi(); err != nil {
		return err
	}

	go s.startProxy()
	go s.startApi()
	go s.watchChanges()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)

	// Block until a signal is received.
	log.Infof("Got signal %s, exiting now", <-c)
	return nil
}

func (s *Service) createProxy() error {
	s.router = hostroute.NewHostRouter()
	proxy, err := vulcan.NewProxy(s.router)
	if err != nil {
		return err
	}
	s.proxy = proxy
	return nil
}

func (s *Service) configureApi() error {
	s.apiRouter = mux.NewRouter()
	api.InitProxyController(s.backend, s.apiRouter)
	return nil
}

func (s *Service) configureProxy() error {
	hosts, err := s.backend.GetHosts()
	if err != nil {
		return err
	}
	for _, host := range hosts {
		log.Infof("Configuring %s", host)

		if err := s.addHost(host); err != nil {
			log.Errorf("Failed adding %s, err: %s", host, err)
			continue
		}
		if err := s.configureHost(host); err != nil {
			log.Errorf("Failed configuring %s", host)
			continue
		}
	}
	return nil
}

func (s *Service) configureHost(host *Host) error {
	for _, loc := range host.Locations {
		if err := s.addLocation(host, loc); err != nil {
			log.Errorf("Failed adding %s to %s, err: %s", loc, host, err)
		} else {
			log.Infof("Added %s to %s", loc, host)
		}
	}
	return nil
}

func (s *Service) configureLocation(loc *Location) error {
	// Add all endpoints from the upstream to the router
	for _, e := range loc.Upstream.Endpoints {
		if err := s.addEndpoint(loc.Upstream, e); err != nil {
			log.Errorf("Failed to add %s to %s, err: %s", e, loc, err)
		} else {
			log.Infof("Added %s to %s", e, loc)
		}
	}
	return nil
}

func (s *Service) watchChanges() {
	for {
		change := <-s.changes
		log.Infof("Service got change: %s", change)
		s.processChange(change)
	}
}

func (s *Service) processChange(change *Change) {
	var err error
	switch child := (change.Child).(type) {
	case *Endpoint:
		switch change.Action {
		case "create":
			err = s.addEndpoint((change.Parent).(*Upstream), child)
		case "delete":
			err = s.deleteEndpoint((change.Parent).(*Upstream), child)
		}
	case *Location:
		switch change.Action {
		case "create":
			err = s.addLocation((change.Parent).(*Host), child)
		case "delete":
			err = s.deleteLocation((change.Parent).(*Host), child)
		}
	case *Host:
		switch change.Action {
		case "create":
			err = s.addHost(child)
		case "delete":
			err = s.deleteHost(child)
		}
	}
	if err != nil {
		log.Errorf("Processing change failed: %s", err)
	}
}

func (s *Service) getPathRouter(hostname string) (*pathroute.PathRouter, error) {
	r := s.router.GetRouter(hostname)
	if r == nil {
		return nil, fmt.Errorf("Location with hostname %s not found, err: %s", hostname)
	}
	router, ok := r.(*pathroute.PathRouter)
	if !ok {
		return nil, fmt.Errorf("Unknown router type: %T", r)
	}
	return router, nil
}

// Returns active locations using given upstream
func (s *Service) getLocations(upstreamId string) ([]*httploc.HttpLocation, error) {
	out := []*httploc.HttpLocation{}

	hosts, err := s.backend.GetHosts()
	if err != nil {
		return nil, fmt.Errorf("Failed to get hosts: %s", hosts)
	}
	for _, h := range hosts {
		router, err := s.getPathRouter(h.Name)
		if err != nil {
			return nil, err
		}
		for _, l := range h.Locations {
			if l.Upstream.Name != upstreamId {
				continue
			}
			ilo := router.GetLocationByPattern(l.Path)
			if ilo == nil {
				return nil, fmt.Errorf("Failed to get location by path: %s", l.Path)
			}
			loc, ok := ilo.(*httploc.HttpLocation)
			if !ok {
				return nil, fmt.Errorf("Unsupported location type: %T", ilo)
			}
			out = append(out, loc)
		}
	}
	return out, nil
}

func (s *Service) addEndpoint(upstream *Upstream, e *Endpoint) error {
	endpoint, err := EndpointFromUrl(e.Name, e.Url)
	if err != nil {
		return fmt.Errorf("Failed to parse endpoint url: %s", endpoint)
	}
	locations, err := s.getLocations(upstream.Name)
	if err != nil {
		return err
	}
	for _, l := range locations {
		rr, ok := l.GetLoadBalancer().(*roundrobin.RoundRobin)
		if !ok {
			return fmt.Errorf("Unexpected load balancer type: %T", l.GetLoadBalancer())
		}
		if err := rr.AddEndpoint(endpoint); err != nil {
			log.Errorf("Failed to add %s, err: %s", e, err)
		} else {
			log.Infof("Added %s", e)
		}
	}
	return nil
}

func (s *Service) deleteEndpoint(upstream *Upstream, e *Endpoint) error {
	endpoint, err := EndpointFromUrl(e.Name, "http://delete.me:4000")
	if err != nil {
		return fmt.Errorf("Failed to parse endpoint url: %s", endpoint)
	}
	locations, err := s.getLocations(upstream.Name)
	if err != nil {
		return err
	}
	for _, l := range locations {
		rr, ok := l.GetLoadBalancer().(*roundrobin.RoundRobin)
		if !ok {
			return fmt.Errorf("Unexpected load balancer type: %T", l.GetLoadBalancer())
		}
		if err := rr.RemoveEndpoint(endpoint); err != nil {
			log.Errorf("Failed to remove endpoint: %s", err)
		} else {
			log.Infof("Removed %s", e)
		}
	}
	return nil
}

func (s *Service) addLocation(host *Host, loc *Location) error {
	router, err := s.getPathRouter(host.Name)
	if err != nil {
		return err
	}
	// Create a load balancer that handles all the endpoints within the given location
	rr, err := roundrobin.NewRoundRobin()
	if err != nil {
		return err
	}
	// Create a location itself
	location, err := httploc.NewLocation(loc.Name, rr)
	if err != nil {
		return err
	}
	// Add the location to the router
	if err := router.AddLocation(loc.Path, location); err != nil {
		return err
	}
	// Once the location added, configure all endpoints
	return s.configureLocation(loc)
}

func (s *Service) deleteLocation(host *Host, loc *Location) error {
	router, err := s.getPathRouter(host.Name)
	if err != nil {
		return err
	}
	location := router.GetLocationById(loc.Name)
	if location == nil {
		return fmt.Errorf("%s not found", loc)
	}
	err = router.RemoveLocation(location)
	if err == nil {
		log.Infof("Removed %s", loc)
	}
	return err
}

func (s *Service) addHost(host *Host) error {
	router := pathroute.NewPathRouter()
	return s.router.SetRouter(host.Name, router)
}

func (s *Service) deleteHost(host *Host) error {
	s.router.RemoveRouter(host.Name)
	log.Infof("Removed %s", host)
	return nil
}

func (s *Service) startProxy() error {
	addr := fmt.Sprintf("%s:%d", s.options.Interface, s.options.Port)
	server := &http.Server{
		Addr:           addr,
		Handler:        s.proxy,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	return server.ListenAndServe()
}

func (s *Service) startApi() error {
	addr := fmt.Sprintf("%s:%d", s.options.ApiInterface, s.options.ApiPort)

	server := &http.Server{
		Addr:           addr,
		Handler:        s.apiRouter,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	return server.ListenAndServe()
}

type VulcanEndpoint struct {
	Url *url.URL
	Id  string
}

func EndpointFromUrl(id string, u string) (*VulcanEndpoint, error) {
	url, err := netutils.ParseUrl(u)
	if err != nil {
		return nil, err
	}
	return &VulcanEndpoint{Url: url, Id: id}, nil
}

func (e *VulcanEndpoint) String() string {
	return fmt.Sprintf("endpoint(id=%s, url=%s)", e.Id, e.Url.String())
}

func (e *VulcanEndpoint) GetId() string {
	return e.Id
}

func (e *VulcanEndpoint) GetUrl() *url.URL {
	return e.Url
}