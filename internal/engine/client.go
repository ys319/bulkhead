package engine

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// Client is a connection to the Docker Engine.
type Client struct {
	api      *client.Client
	endpoint Endpoint
}

// Connect opens a client for the resolved endpoint. It does not contact the
// engine; the first API call does.
func Connect(endpoint Endpoint) (*Client, error) {
	// FromEnv covers DOCKER_HOST and its TLS settings, and leaves the platform
	// default in place when nothing is set. A context host goes on top.
	opts := []client.Opt{client.FromEnv}
	if endpoint.Source == SourceContext {
		opts = append(opts, client.WithHost(endpoint.Host))
	}
	api, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating the docker client: %w", err)
	}
	return &Client{api: api, endpoint: endpoint}, nil
}

func (c *Client) Close() error { return c.api.Close() }

// Host is the address the client actually dials.
func (c *Client) Host() string { return c.api.DaemonHost() }

// Info is what `bh version` shows about the engine.
type Info struct {
	Version    string
	APIVersion string
	OS         string
	Arch       string
	Builder    string // "1" classic, "2" BuildKit
}

// Info asks the engine who it is. It is the cheapest end-to-end check that
// the endpoint resolution, the transport and API negotiation all work.
func (c *Client) Info(ctx context.Context) (Info, error) {
	ping, err := c.api.Ping(ctx, client.PingOptions{})
	if err != nil {
		return Info{}, err
	}
	version, err := c.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return Info{}, err
	}
	return Info{
		Version:    version.Version,
		APIVersion: version.APIVersion,
		OS:         version.Os,
		Arch:       version.Arch,
		Builder:    string(ping.BuilderVersion),
	}, nil
}
