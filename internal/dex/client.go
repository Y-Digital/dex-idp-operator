/*
Copyright 2026.

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

// Package dex contains the small adapter used by controllers to talk to Dex.
package dex

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	dexapi "github.com/dexidp/dex/api/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var (
	// ErrNotFound reports that Dex does not have the requested resource.
	ErrNotFound = errors.New("dex resource not found")
	// ErrAlreadyExists reports that Dex already has the requested resource.
	ErrAlreadyExists = errors.New("dex resource already exists")
)

// Client manages Dex OAuth2 clients and connectors.
type Client interface {
	GetClient(ctx context.Context, id string) (*dexapi.Client, error)
	CreateClient(ctx context.Context, client *dexapi.Client) (*dexapi.Client, error)
	UpdateClient(ctx context.Context, req *dexapi.UpdateClientReq) error
	DeleteClient(ctx context.Context, id string) error
	GetVersion(ctx context.Context) (*dexapi.VersionResp, error)
	CreateConnector(ctx context.Context, connector *dexapi.Connector) error
	UpdateConnector(ctx context.Context, req *dexapi.UpdateConnectorReq) error
	DeleteConnector(ctx context.Context, id string) error
	Close() error
}

// DialConfig configures a Dex gRPC client connection.
type DialConfig struct {
	Address   string
	Plaintext bool
	TLS       TLSConfig
}

// TLSConfig contains TLS material for Dex gRPC.
type TLSConfig struct {
	CACertPEM          []byte
	ClientCertPEM      []byte
	ClientKeyPEM       []byte
	InsecureSkipVerify bool
	ServerName         string
}

// Dial creates a Dex API client.
func Dial(_ context.Context, cfg DialConfig) (Client, error) {
	opts, err := dialOptions(cfg)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(cfg.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("create dex grpc client: %w", err)
	}

	return &grpcClient{
		conn: conn,
		api:  dexapi.NewDexClient(conn),
	}, nil
}

type grpcClient struct {
	conn *grpc.ClientConn
	api  dexapi.DexClient
}

func (c *grpcClient) GetClient(ctx context.Context, id string) (*dexapi.Client, error) {
	resp, err := c.api.GetClient(ctx, &dexapi.GetClientReq{Id: id})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get dex client: %w", err)
	}
	if resp.GetClient() == nil {
		return nil, ErrNotFound
	}
	return resp.GetClient(), nil
}

func isNotFound(err error) bool {
	if status.Code(err) == codes.NotFound {
		return true
	}
	grpcStatus, ok := status.FromError(err)
	return ok && strings.EqualFold(grpcStatus.Message(), "not found")
}

func (c *grpcClient) CreateClient(ctx context.Context, client *dexapi.Client) (*dexapi.Client, error) {
	resp, err := c.api.CreateClient(ctx, &dexapi.CreateClientReq{Client: client})
	if err != nil {
		return nil, fmt.Errorf("create dex client: %w", err)
	}
	if resp.GetAlreadyExists() {
		return nil, ErrAlreadyExists
	}
	return resp.GetClient(), nil
}

func (c *grpcClient) UpdateClient(ctx context.Context, req *dexapi.UpdateClientReq) error {
	resp, err := c.api.UpdateClient(ctx, req)
	if err != nil {
		return fmt.Errorf("update dex client: %w", err)
	}
	if resp.GetNotFound() {
		return ErrNotFound
	}
	return nil
}

func (c *grpcClient) DeleteClient(ctx context.Context, id string) error {
	resp, err := c.api.DeleteClient(ctx, &dexapi.DeleteClientReq{Id: id})
	if err != nil {
		return fmt.Errorf("delete dex client: %w", err)
	}
	if resp.GetNotFound() {
		return ErrNotFound
	}
	return nil
}

func (c *grpcClient) GetVersion(ctx context.Context) (*dexapi.VersionResp, error) {
	resp, err := c.api.GetVersion(ctx, &dexapi.VersionReq{})
	if err != nil {
		return nil, fmt.Errorf("get dex version: %w", err)
	}
	return resp, nil
}

func (c *grpcClient) CreateConnector(ctx context.Context, connector *dexapi.Connector) error {
	resp, err := c.api.CreateConnector(ctx, &dexapi.CreateConnectorReq{Connector: connector})
	if err != nil {
		return fmt.Errorf("create dex connector: %w", err)
	}
	if resp.GetAlreadyExists() {
		return ErrAlreadyExists
	}
	return nil
}

func (c *grpcClient) UpdateConnector(ctx context.Context, req *dexapi.UpdateConnectorReq) error {
	resp, err := c.api.UpdateConnector(ctx, req)
	if err != nil {
		return fmt.Errorf("update dex connector: %w", err)
	}
	if resp.GetNotFound() {
		return ErrNotFound
	}
	return nil
}

func (c *grpcClient) DeleteConnector(ctx context.Context, id string) error {
	resp, err := c.api.DeleteConnector(ctx, &dexapi.DeleteConnectorReq{Id: id})
	if err != nil {
		return fmt.Errorf("delete dex connector: %w", err)
	}
	if resp.GetNotFound() {
		return ErrNotFound
	}
	return nil
}

func (c *grpcClient) Close() error {
	return c.conn.Close()
}

func dialOptions(cfg DialConfig) ([]grpc.DialOption, error) {
	if cfg.Plaintext {
		return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, nil
	}

	tlsConfig, err := buildTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))}, nil
}

func buildTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // Explicit CRD opt-in for non-production endpoints.
		MinVersion:         tls.VersionTLS12,
		ServerName:         cfg.ServerName,
	}

	if len(cfg.CACertPEM) > 0 {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(cfg.CACertPEM) {
			return nil, fmt.Errorf("parse dex CA certificate")
		}
		tlsConfig.RootCAs = roots
	}

	if len(cfg.ClientCertPEM) > 0 || len(cfg.ClientKeyPEM) > 0 {
		cert, err := tls.X509KeyPair(cfg.ClientCertPEM, cfg.ClientKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("parse dex client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}
