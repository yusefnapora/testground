//+build linux

package sidecar

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/ipfs/testground/pkg/dockermanager"
	"github.com/ipfs/testground/sdk/runtime"
	"github.com/ipfs/testground/sdk/sync"
)

type K8sInstanceManager struct {
	redis   net.IP
	manager *dockermanager.Manager
}

func NewK8sManager() (InstanceManager, error) {
	// TODO: Generalize this to a list of services.
	redisHost := os.Getenv(EnvRedisHost)

	redisIp, err := net.ResolveIPAddr("ip4", redisHost)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve redis host: %w", err)
	}

	docker, err := dockermanager.NewManager()
	if err != nil {
		return nil, err
	}

	return &K8sInstanceManager{
		manager: docker,
		redis:   redisIp.IP,
	}, nil
}

func (d *K8sInstanceManager) Manage(
	ctx context.Context,
	worker func(ctx context.Context, inst *Instance) error,
) error {
	return d.manager.Manage(ctx, func(ctx context.Context, container *dockermanager.Container) error {
		inst, err := d.manageContainer(ctx, container)
		if err != nil {
			return fmt.Errorf("when initializing the container: %w", err)
		}
		err = worker(ctx, inst)
		if err != nil {
			return fmt.Errorf("container worker failed: %w", err)
		}
		return nil
	}, "testground.runid")
}

func (d *K8sInstanceManager) Close() error {
	return d.manager.Close()
}

func (d *K8sInstanceManager) manageContainer(ctx context.Context, container *dockermanager.Container) (inst *Instance, err error) {
	// Get the state/config of the cluster
	info, err := container.Inspect(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect failed: %w", err)
	}

	if !info.State.Running {
		return nil, fmt.Errorf("not running")
	}

	// TODO: cache this?
	networks, err := container.Manager.NetworkList(ctx, types.NetworkListOptions{
		Filters: filters.NewArgs(
			filters.Arg(
				"label",
				"testground.runid="+info.Config.Labels["testground.runid"],
			),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list networks: %w", err)
	}

	// Construct the runtime environment

	runenv, err := runtime.ParseRunEnv(info.Config.Env)
	if err != nil {
		return nil, fmt.Errorf("failed to parse run environment: %w", err)
	}

	// Get a netlink handle.
	nshandle, err := netns.GetFromPid(info.State.Pid)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup the net namespace: %s", err)
	}
	defer nshandle.Close()

	netlinkHandle, err := netlink.NewHandleAt(nshandle)
	if err != nil {
		return nil, fmt.Errorf("failed to get handle to network namespace: %w", err)
	}

	defer func() {
		if err != nil {
			netlinkHandle.Delete()
		}
	}()

	// Map _current_ networks to links.
	links, err := dockerLinks(netlinkHandle, info.NetworkSettings)
	if err != nil {
		return nil, fmt.Errorf("failed to enumerate links: %w", err)
	}

	// Finally, construct the network manager.
	network := &DockerNetwork{
		container:      container,
		activeLinks:    make(map[string]*dockerLink, len(info.NetworkSettings.Networks)),
		availableLinks: make(map[string]string, len(networks)),
		nl:             netlinkHandle,
	}

	for _, n := range networks {
		name := n.Labels["testground.name"]
		id := n.ID
		network.availableLinks[name] = id
	}

	reverseIndex := make(map[string]string, len(network.availableLinks))
	for name, id := range network.availableLinks {
		reverseIndex[id] = name
	}

	// TODO: Some of this code could be factored out into helpers.

	// Get the routes to redis. We need to keep these.
	redisRoutes, err := netlinkHandle.RouteGet(d.redis)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve route to redis: %w", err)
	}

	for id, link := range links {
		if name, ok := reverseIndex[id]; ok {
			// manage this network
			handle, err := NewNetlinkLink(netlinkHandle, link.Link)
			if err != nil {
				return nil, fmt.Errorf(
					"failed to initialize link %s (%s): %w",
					name,
					link.Attrs().Name,
					err,
				)
			}
			network.activeLinks[name] = &dockerLink{
				NetlinkLink: handle,
				IPv4:        link.IPv4,
				IPv6:        link.IPv6,
			}
			continue
		}

		// We've found a control network (or some other network).

		// Get the current routes.
		linkRoutes, err := netlinkHandle.RouteList(link, netlink.FAMILY_ALL)
		if err != nil {
			return nil, fmt.Errorf("failed to list routes for link %s", link.Attrs().Name)
		}

		// Add specific routes to redis if redis uses this link.
		for _, route := range redisRoutes {
			if route.LinkIndex != link.Attrs().Index {
				continue
			}
			if err := netlinkHandle.RouteAdd(&route); err != nil {
				return nil, fmt.Errorf("failed to add new route: %w", err)
			}
			break
		}

		// Remove the original routes
		for _, route := range linkRoutes {
			if err := netlinkHandle.RouteDel(&route); err != nil {
				return nil, fmt.Errorf("failed to remove existing route: %w", err)
			}
		}
	}
	return NewInstance(runenv, info.Config.Hostname, network)
}

func localAddresses() {
	ifaces, err := net.Interfaces()
	if err != nil {
		fmt.Print(fmt.Errorf("localAddresses: %+v\n", err.Error()))
		return
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			fmt.Print(fmt.Errorf("localAddresses: %+v\n", err.Error()))
			continue
		}
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPAddr:
				fmt.Printf("%v : %s (%s)\n", i.Name, v, v.IP.DefaultMask())
			}

		}
	}
}

type K8sNetwork struct {
}

func (n *K8sNetwork) Close() {
}

func (n *K8sNetwork) ConfigureNetwork(ctx context.Context, cfg *sync.NetworkConfig) error {
	return errors.New("not implemented")
}

func (n *K8sNetwork) ListActive() []string {
	return []string{}
}

func (n *K8sNetwork) ListAvailable() []string {
	return []string{}
}
