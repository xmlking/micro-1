// Package handler is the handler for the `micro debug stats` service
package handler

import (
	"context"
	"sync"
	"time"

	"github.com/micro/go-micro/v2/client"
	"github.com/micro/go-micro/v2/config/cmd"
	debug "github.com/micro/go-micro/v2/debug/service/proto"
	"github.com/micro/go-micro/v2/errors"
	"github.com/micro/go-micro/v2/registry"
	"github.com/micro/go-micro/v2/registry/cache"
	"github.com/micro/go-micro/v2/util/log"
	"github.com/micro/go-micro/v2/util/ring"
	stats "github.com/micro/micro/v2/debug/stats/proto"
)

// New initialises and returns a new Stats service handler
func New(done <-chan bool, windowSize int) (*Stats, error) {
	s := &Stats{
		registry:            cache.New(*cmd.DefaultOptions().Registry),
		client:              *cmd.DefaultOptions().Client,
		historicalSnapshots: ring.New(windowSize),
	}

	if err := s.scan(); err != nil {
		return nil, err
	}

	s.Start(done)
	return s, nil
}

// Stats is the Debug.Stats handler
type Stats struct {
	registry registry.Registry
	client   client.Client

	sync.RWMutex
	// current snapshots for each service
	snapshots []*stats.Snapshot
	// historical snapshots from the start
	historicalSnapshots *ring.Buffer
	cached              []*registry.Service
}

// Read returns gets a snapshot of all current stats
func (s *Stats) Read(ctx context.Context, req *stats.ReadRequest, rsp *stats.ReadResponse) error {
	allSnapshots := []*stats.Snapshot{}
	func() {
		s.RLock()
		defer s.RUnlock()
		if req.Past {
			entries := s.historicalSnapshots.Get(3600)
			for _, entry := range entries {
				allSnapshots = append(allSnapshots, entry.Value.([]*stats.Snapshot)...)
			}
		} else {
			// Using an else since the latest snapshot is already in the ring buffer
			allSnapshots = append(allSnapshots, s.snapshots...)
		}
	}()
	if req.Service == nil {
		rsp.Stats = allSnapshots
		return nil
	}
	filter := func(a, b string) bool {
		if len(b) == 0 {
			return true
		}
		return a == b
	}
	filteredSnapshots := []*stats.Snapshot{}
	for _, s := range allSnapshots {
		if !filter(s.Service.Name, req.Service.Name) {
			continue
		}
		if !filter(s.Service.Version, req.Service.Version) {
			continue
		}
		filteredSnapshots = append(filteredSnapshots, s)
	}
	rsp.Stats = filteredSnapshots
	return nil
}

func (s *Stats) Write(ctx context.Context, req *stats.WriteRequest, rsp *stats.WriteResponse) error {
	return errors.BadRequest("go.micro.debug.stats", "not implemented")
}

// Stream starts streaming stats
func (s *Stats) Stream(ctx context.Context, req *stats.StreamRequest, rsp stats.Stats_StreamStream) error {
	return errors.BadRequest("go.micro.debug.stats", "not implemented")
}

// Start Starts scraping other services until the provided channel is closed
func (s *Stats) Start(done <-chan bool) {
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				s.scrape()
				time.Sleep(time.Second)
			}
		}
	}()

	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()

		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := s.scan(); err != nil {
					log.Debug(err)
				}
			}
		}
	}()
}

func (s *Stats) scan() error {
	services, err := s.registry.ListServices()
	if err != nil {
		return err
	}

	if err != nil {
		return err
	}

	serviceMap := make(map[string]*registry.Service)

	// check each service has nodes
	for _, service := range services {
		if len(service.Nodes) > 0 {
			serviceMap[service.Name+service.Version] = service
			continue
		}

		// get nodes that does not exist
		newServices, err := s.registry.GetService(service.Name)
		if err != nil {
			continue
		}

		// store service by version
		for _, service := range newServices {
			serviceMap[service.Name+service.Version] = service
		}
	}

	// flatten the map
	var serviceList []*registry.Service

	for _, service := range serviceMap {
		serviceList = append(serviceList, service)
	}

	// save the list
	s.Lock()
	s.cached = serviceList
	s.Unlock()
	return nil
}

func (s *Stats) scrape() {
	s.RLock()
	// Create a local copy of cached services
	services := make([]*registry.Service, len(s.cached))
	copy(services, s.cached)
	s.RUnlock()

	// Start building the next list of snapshots
	var mtx sync.Mutex
	next := make([]*stats.Snapshot, 0)

	// Call each node of each service in goroutines
	var wg sync.WaitGroup

	protocol := s.client.String()

	for _, svc := range services {
		// Ignore nodeless and non mucp services
		if len(svc.Nodes) == 0 {
			continue
		}
		// Call every node
		for _, node := range svc.Nodes {
			if node.Metadata["protocol"] != protocol {
				continue
			}

			wg.Add(1)

			go func(st *Stats, service *registry.Service, node *registry.Node) {
				defer wg.Done()

				// create new context to cancel within a few seconds
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()

				req := s.client.NewRequest(service.Name, "Debug.Stats", &debug.StatsRequest{})
				rsp := new(debug.StatsResponse)
				if err := s.client.Call(ctx, req, rsp, client.WithAddress(node.Address)); err != nil {
					log.Errorf("Error calling %s@%s (%s)", service.Name, node.Address, err.Error())
					return
				}

				// Append the new snapshot
				snap := &stats.Snapshot{
					Service: &stats.Service{
						Name:    service.Name,
						Version: service.Version,
						Node: &stats.Node{
							Id:      node.Id,
							Address: node.Address,
						},
					},
					Started:  int64(rsp.Started),
					Uptime:   rsp.Uptime,
					Memory:   rsp.Memory,
					Threads:  rsp.Threads,
					Gc:       rsp.Gc,
					Requests: rsp.Requests,
					Errors:   rsp.Errors,
				}
				timestamp := time.Now().Unix()
				snap.Timestamp = uint64(timestamp)
				mtx.Lock()
				next = append(next, snap)
				mtx.Unlock()
			}(s, svc, node)
		}
	}
	wg.Wait()

	// Swap in the snapshots
	s.Lock()
	s.snapshots = next
	s.historicalSnapshots.Put(next)
	s.Unlock()
}
