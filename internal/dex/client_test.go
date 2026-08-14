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

package dex

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	dexapi "github.com/dexidp/dex/api/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBuildTLSConfig(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := mustGenerateTLSCert(t)

	t.Run("empty config defaults to TLS 1.2", func(t *testing.T) {
		g := NewWithT(t)

		cfg, err := buildTLSConfig(TLSConfig{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cfg.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
		g.Expect(cfg.InsecureSkipVerify).To(BeFalse())
		g.Expect(cfg.ServerName).To(BeEmpty())
		g.Expect(cfg.RootCAs).To(BeNil())
		g.Expect(cfg.Certificates).To(BeEmpty())
	})

	t.Run("invalid CA PEM", func(t *testing.T) {
		g := NewWithT(t)

		_, err := buildTLSConfig(TLSConfig{CACertPEM: []byte("not-a-certificate")})
		g.Expect(err).To(MatchError(ContainSubstring("parse dex CA certificate")))
	})

	t.Run("CA only", func(t *testing.T) {
		g := NewWithT(t)

		cfg, err := buildTLSConfig(TLSConfig{CACertPEM: certPEM})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cfg.RootCAs).NotTo(BeNil())
		g.Expect(cfg.Certificates).To(BeEmpty())
	})

	t.Run("mTLS keypair", func(t *testing.T) {
		g := NewWithT(t)

		cfg, err := buildTLSConfig(TLSConfig{
			CACertPEM:     certPEM,
			ClientCertPEM: certPEM,
			ClientKeyPEM:  keyPEM,
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cfg.RootCAs).NotTo(BeNil())
		g.Expect(cfg.Certificates).To(HaveLen(1))
	})

	t.Run("cert without key", func(t *testing.T) {
		g := NewWithT(t)

		_, err := buildTLSConfig(TLSConfig{ClientCertPEM: certPEM})
		g.Expect(err).To(MatchError(ContainSubstring("parse dex client certificate")))
	})

	t.Run("key without cert", func(t *testing.T) {
		g := NewWithT(t)

		_, err := buildTLSConfig(TLSConfig{ClientKeyPEM: keyPEM})
		g.Expect(err).To(MatchError(ContainSubstring("parse dex client certificate")))
	})

	t.Run("InsecureSkipVerify and ServerName", func(t *testing.T) {
		g := NewWithT(t)

		cfg, err := buildTLSConfig(TLSConfig{
			InsecureSkipVerify: true,
			ServerName:         "dex.example.com",
		})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cfg.InsecureSkipVerify).To(BeTrue())
		g.Expect(cfg.ServerName).To(Equal("dex.example.com"))
	})
}

func TestDialOptions(t *testing.T) {
	t.Parallel()

	t.Run("plaintext", func(t *testing.T) {
		g := NewWithT(t)

		opts, err := dialOptions(DialConfig{Plaintext: true})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(opts).To(HaveLen(1))
	})

	t.Run("TLS fails on bad CA without dialing", func(t *testing.T) {
		g := NewWithT(t)

		_, err := dialOptions(DialConfig{TLS: TLSConfig{CACertPEM: []byte("not-a-certificate")}})
		g.Expect(err).To(MatchError(ContainSubstring("parse dex CA certificate")))
	})

	t.Run("TLS with empty config", func(t *testing.T) {
		g := NewWithT(t)

		opts, err := dialOptions(DialConfig{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(opts).To(HaveLen(1))
	})
}

func TestIsNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "codes.NotFound", err: status.Error(codes.NotFound, "missing"), want: true},
		{name: "message not found", err: status.Error(codes.Unknown, "not found"), want: true},
		{name: "message Not Found", err: status.Error(codes.Unknown, "Not Found"), want: true},
		{name: "other code", err: status.Error(codes.Unavailable, "down"), want: false},
		{name: "plain error", err: errors.New("not found"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(isNotFound(tc.err)).To(Equal(tc.want))
		})
	}
}

func TestDial(t *testing.T) {
	t.Parallel()

	t.Run("plaintext succeeds without connecting", func(t *testing.T) {
		g := NewWithT(t)

		client, err := Dial(context.Background(), DialConfig{Address: "localhost:5557", Plaintext: true})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(client.Close()).To(Succeed())
	})

	t.Run("invalid TLS fails before dialing", func(t *testing.T) {
		g := NewWithT(t)

		_, err := Dial(context.Background(), DialConfig{
			Address: "localhost:5557",
			TLS:     TLSConfig{CACertPEM: []byte("not-a-certificate")},
		})
		g.Expect(err).To(MatchError(ContainSubstring("parse dex CA certificate")))
	})
}

func TestGRPCClientMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("GetClient maps gRPC NotFound", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			getClient: func(context.Context, *dexapi.GetClientReq) (*dexapi.GetClientResp, error) {
				return nil, status.Error(codes.NotFound, "missing")
			},
		}}

		_, err := client.GetClient(ctx, "missing")
		g.Expect(err).To(MatchError(ErrNotFound))
	})

	t.Run("GetClient maps not found message", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			getClient: func(context.Context, *dexapi.GetClientReq) (*dexapi.GetClientResp, error) {
				return nil, status.Error(codes.Unknown, "not found")
			},
		}}

		_, err := client.GetClient(ctx, "missing")
		g.Expect(err).To(MatchError(ErrNotFound))
	})

	t.Run("GetClient maps nil client", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			getClient: func(context.Context, *dexapi.GetClientReq) (*dexapi.GetClientResp, error) {
				return &dexapi.GetClientResp{}, nil
			},
		}}

		_, err := client.GetClient(ctx, "missing")
		g.Expect(err).To(MatchError(ErrNotFound))
	})

	t.Run("GetClient wraps transport errors", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			getClient: func(context.Context, *dexapi.GetClientReq) (*dexapi.GetClientResp, error) {
				return nil, errors.New("connection refused")
			},
		}}

		_, err := client.GetClient(ctx, "app")
		g.Expect(err).To(MatchError(ContainSubstring("get dex client")))
	})

	t.Run("GetClient success", func(t *testing.T) {
		g := NewWithT(t)
		want := &dexapi.Client{Id: "app", Secret: "s"}
		client := grpcClient{api: &fakeDexAPI{
			getClient: func(context.Context, *dexapi.GetClientReq) (*dexapi.GetClientResp, error) {
				return &dexapi.GetClientResp{Client: want}, nil
			},
		}}

		got, err := client.GetClient(ctx, "app")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got.GetId()).To(Equal("app"))
	})

	t.Run("CreateClient maps AlreadyExists", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			createClient: func(context.Context, *dexapi.CreateClientReq) (*dexapi.CreateClientResp, error) {
				return &dexapi.CreateClientResp{AlreadyExists: true}, nil
			},
		}}

		_, err := client.CreateClient(ctx, &dexapi.Client{Id: "app"})
		g.Expect(err).To(MatchError(ErrAlreadyExists))
	})

	t.Run("CreateClient wraps transport errors", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			createClient: func(context.Context, *dexapi.CreateClientReq) (*dexapi.CreateClientResp, error) {
				return nil, errors.New("boom")
			},
		}}

		_, err := client.CreateClient(ctx, &dexapi.Client{Id: "app"})
		g.Expect(err).To(MatchError(ContainSubstring("create dex client")))
	})

	t.Run("CreateClient success", func(t *testing.T) {
		g := NewWithT(t)
		want := &dexapi.Client{Id: "app"}
		client := grpcClient{api: &fakeDexAPI{
			createClient: func(context.Context, *dexapi.CreateClientReq) (*dexapi.CreateClientResp, error) {
				return &dexapi.CreateClientResp{Client: want}, nil
			},
		}}

		got, err := client.CreateClient(ctx, want)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got.GetId()).To(Equal("app"))
	})

	t.Run("UpdateClient maps NotFound flag", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			updateClient: func(context.Context, *dexapi.UpdateClientReq) (*dexapi.UpdateClientResp, error) {
				return &dexapi.UpdateClientResp{NotFound: true}, nil
			},
		}}

		err := client.UpdateClient(ctx, &dexapi.UpdateClientReq{Id: "missing"})
		g.Expect(err).To(MatchError(ErrNotFound))
	})

	t.Run("UpdateClient wraps transport errors", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			updateClient: func(context.Context, *dexapi.UpdateClientReq) (*dexapi.UpdateClientResp, error) {
				return nil, errors.New("boom")
			},
		}}

		err := client.UpdateClient(ctx, &dexapi.UpdateClientReq{Id: "app"})
		g.Expect(err).To(MatchError(ContainSubstring("update dex client")))
	})

	t.Run("UpdateClient success", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			updateClient: func(context.Context, *dexapi.UpdateClientReq) (*dexapi.UpdateClientResp, error) {
				return &dexapi.UpdateClientResp{}, nil
			},
		}}

		g.Expect(client.UpdateClient(ctx, &dexapi.UpdateClientReq{Id: "app"})).To(Succeed())
	})

	t.Run("DeleteClient maps NotFound flag", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			deleteClient: func(context.Context, *dexapi.DeleteClientReq) (*dexapi.DeleteClientResp, error) {
				return &dexapi.DeleteClientResp{NotFound: true}, nil
			},
		}}

		err := client.DeleteClient(ctx, "missing")
		g.Expect(err).To(MatchError(ErrNotFound))
	})

	t.Run("DeleteClient wraps transport errors", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			deleteClient: func(context.Context, *dexapi.DeleteClientReq) (*dexapi.DeleteClientResp, error) {
				return nil, errors.New("boom")
			},
		}}

		err := client.DeleteClient(ctx, "app")
		g.Expect(err).To(MatchError(ContainSubstring("delete dex client")))
	})

	t.Run("DeleteClient success", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			deleteClient: func(context.Context, *dexapi.DeleteClientReq) (*dexapi.DeleteClientResp, error) {
				return &dexapi.DeleteClientResp{}, nil
			},
		}}

		g.Expect(client.DeleteClient(ctx, "app")).To(Succeed())
	})

	t.Run("GetVersion wraps transport errors", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			getVersion: func(context.Context, *dexapi.VersionReq) (*dexapi.VersionResp, error) {
				return nil, errors.New("boom")
			},
		}}

		_, err := client.GetVersion(ctx)
		g.Expect(err).To(MatchError(ContainSubstring("get dex version")))
	})

	t.Run("GetVersion success", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			getVersion: func(context.Context, *dexapi.VersionReq) (*dexapi.VersionResp, error) {
				return &dexapi.VersionResp{Server: "dex"}, nil
			},
		}}

		got, err := client.GetVersion(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got.GetServer()).To(Equal("dex"))
	})

	t.Run("CreateConnector maps AlreadyExists", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			createConnector: func(context.Context, *dexapi.CreateConnectorReq) (*dexapi.CreateConnectorResp, error) {
				return &dexapi.CreateConnectorResp{AlreadyExists: true}, nil
			},
		}}

		err := client.CreateConnector(ctx, &dexapi.Connector{Id: "oidc"})
		g.Expect(err).To(MatchError(ErrAlreadyExists))
	})

	t.Run("CreateConnector wraps transport errors", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			createConnector: func(context.Context, *dexapi.CreateConnectorReq) (*dexapi.CreateConnectorResp, error) {
				return nil, errors.New("boom")
			},
		}}

		err := client.CreateConnector(ctx, &dexapi.Connector{Id: "oidc"})
		g.Expect(err).To(MatchError(ContainSubstring("create dex connector")))
	})

	t.Run("CreateConnector success", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			createConnector: func(context.Context, *dexapi.CreateConnectorReq) (*dexapi.CreateConnectorResp, error) {
				return &dexapi.CreateConnectorResp{}, nil
			},
		}}

		g.Expect(client.CreateConnector(ctx, &dexapi.Connector{Id: "oidc"})).To(Succeed())
	})

	t.Run("UpdateConnector maps NotFound flag", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			updateConnector: func(context.Context, *dexapi.UpdateConnectorReq) (*dexapi.UpdateConnectorResp, error) {
				return &dexapi.UpdateConnectorResp{NotFound: true}, nil
			},
		}}

		err := client.UpdateConnector(ctx, &dexapi.UpdateConnectorReq{Id: "missing"})
		g.Expect(err).To(MatchError(ErrNotFound))
	})

	t.Run("UpdateConnector wraps transport errors", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			updateConnector: func(context.Context, *dexapi.UpdateConnectorReq) (*dexapi.UpdateConnectorResp, error) {
				return nil, errors.New("boom")
			},
		}}

		err := client.UpdateConnector(ctx, &dexapi.UpdateConnectorReq{Id: "oidc"})
		g.Expect(err).To(MatchError(ContainSubstring("update dex connector")))
	})

	t.Run("UpdateConnector success", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			updateConnector: func(context.Context, *dexapi.UpdateConnectorReq) (*dexapi.UpdateConnectorResp, error) {
				return &dexapi.UpdateConnectorResp{}, nil
			},
		}}

		g.Expect(client.UpdateConnector(ctx, &dexapi.UpdateConnectorReq{Id: "oidc"})).To(Succeed())
	})

	t.Run("DeleteConnector maps NotFound flag", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			deleteConnector: func(context.Context, *dexapi.DeleteConnectorReq) (*dexapi.DeleteConnectorResp, error) {
				return &dexapi.DeleteConnectorResp{NotFound: true}, nil
			},
		}}

		err := client.DeleteConnector(ctx, "missing")
		g.Expect(err).To(MatchError(ErrNotFound))
	})

	t.Run("DeleteConnector wraps transport errors", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			deleteConnector: func(context.Context, *dexapi.DeleteConnectorReq) (*dexapi.DeleteConnectorResp, error) {
				return nil, errors.New("boom")
			},
		}}

		err := client.DeleteConnector(ctx, "oidc")
		g.Expect(err).To(MatchError(ContainSubstring("delete dex connector")))
	})

	t.Run("DeleteConnector success", func(t *testing.T) {
		g := NewWithT(t)
		client := grpcClient{api: &fakeDexAPI{
			deleteConnector: func(context.Context, *dexapi.DeleteConnectorReq) (*dexapi.DeleteConnectorResp, error) {
				return &dexapi.DeleteConnectorResp{}, nil
			},
		}}

		g.Expect(client.DeleteConnector(ctx, "oidc")).To(Succeed())
	})
}

func mustGenerateTLSCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dex.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
}

type fakeDexAPI struct {
	getClient       func(context.Context, *dexapi.GetClientReq) (*dexapi.GetClientResp, error)
	createClient    func(context.Context, *dexapi.CreateClientReq) (*dexapi.CreateClientResp, error)
	updateClient    func(context.Context, *dexapi.UpdateClientReq) (*dexapi.UpdateClientResp, error)
	deleteClient    func(context.Context, *dexapi.DeleteClientReq) (*dexapi.DeleteClientResp, error)
	getVersion      func(context.Context, *dexapi.VersionReq) (*dexapi.VersionResp, error)
	createConnector func(context.Context, *dexapi.CreateConnectorReq) (*dexapi.CreateConnectorResp, error)
	updateConnector func(context.Context, *dexapi.UpdateConnectorReq) (*dexapi.UpdateConnectorResp, error)
	deleteConnector func(context.Context, *dexapi.DeleteConnectorReq) (*dexapi.DeleteConnectorResp, error)
}

var _ dexapi.DexClient = (*fakeDexAPI)(nil)

func (f *fakeDexAPI) GetClient(ctx context.Context, in *dexapi.GetClientReq, _ ...grpc.CallOption) (*dexapi.GetClientResp, error) {
	if f.getClient != nil {
		return f.getClient(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "GetClient")
}

func (f *fakeDexAPI) CreateClient(ctx context.Context, in *dexapi.CreateClientReq, _ ...grpc.CallOption) (*dexapi.CreateClientResp, error) {
	if f.createClient != nil {
		return f.createClient(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "CreateClient")
}

func (f *fakeDexAPI) UpdateClient(ctx context.Context, in *dexapi.UpdateClientReq, _ ...grpc.CallOption) (*dexapi.UpdateClientResp, error) {
	if f.updateClient != nil {
		return f.updateClient(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "UpdateClient")
}

func (f *fakeDexAPI) DeleteClient(ctx context.Context, in *dexapi.DeleteClientReq, _ ...grpc.CallOption) (*dexapi.DeleteClientResp, error) {
	if f.deleteClient != nil {
		return f.deleteClient(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "DeleteClient")
}

func (f *fakeDexAPI) ListClients(context.Context, *dexapi.ListClientReq, ...grpc.CallOption) (*dexapi.ListClientResp, error) {
	return nil, status.Error(codes.Unimplemented, "ListClients")
}

func (f *fakeDexAPI) CreatePassword(context.Context, *dexapi.CreatePasswordReq, ...grpc.CallOption) (*dexapi.CreatePasswordResp, error) {
	return nil, status.Error(codes.Unimplemented, "CreatePassword")
}

func (f *fakeDexAPI) UpdatePassword(context.Context, *dexapi.UpdatePasswordReq, ...grpc.CallOption) (*dexapi.UpdatePasswordResp, error) {
	return nil, status.Error(codes.Unimplemented, "UpdatePassword")
}

func (f *fakeDexAPI) DeletePassword(context.Context, *dexapi.DeletePasswordReq, ...grpc.CallOption) (*dexapi.DeletePasswordResp, error) {
	return nil, status.Error(codes.Unimplemented, "DeletePassword")
}

func (f *fakeDexAPI) ListPasswords(context.Context, *dexapi.ListPasswordReq, ...grpc.CallOption) (*dexapi.ListPasswordResp, error) {
	return nil, status.Error(codes.Unimplemented, "ListPasswords")
}

func (f *fakeDexAPI) CreateConnector(ctx context.Context, in *dexapi.CreateConnectorReq, _ ...grpc.CallOption) (*dexapi.CreateConnectorResp, error) {
	if f.createConnector != nil {
		return f.createConnector(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "CreateConnector")
}

func (f *fakeDexAPI) UpdateConnector(ctx context.Context, in *dexapi.UpdateConnectorReq, _ ...grpc.CallOption) (*dexapi.UpdateConnectorResp, error) {
	if f.updateConnector != nil {
		return f.updateConnector(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "UpdateConnector")
}

func (f *fakeDexAPI) DeleteConnector(ctx context.Context, in *dexapi.DeleteConnectorReq, _ ...grpc.CallOption) (*dexapi.DeleteConnectorResp, error) {
	if f.deleteConnector != nil {
		return f.deleteConnector(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "DeleteConnector")
}

func (f *fakeDexAPI) ListConnectors(context.Context, *dexapi.ListConnectorReq, ...grpc.CallOption) (*dexapi.ListConnectorResp, error) {
	return nil, status.Error(codes.Unimplemented, "ListConnectors")
}

func (f *fakeDexAPI) GetVersion(ctx context.Context, in *dexapi.VersionReq, _ ...grpc.CallOption) (*dexapi.VersionResp, error) {
	if f.getVersion != nil {
		return f.getVersion(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "GetVersion")
}

func (f *fakeDexAPI) GetDiscovery(context.Context, *dexapi.DiscoveryReq, ...grpc.CallOption) (*dexapi.DiscoveryResp, error) {
	return nil, status.Error(codes.Unimplemented, "GetDiscovery")
}

func (f *fakeDexAPI) ListRefresh(context.Context, *dexapi.ListRefreshReq, ...grpc.CallOption) (*dexapi.ListRefreshResp, error) {
	return nil, status.Error(codes.Unimplemented, "ListRefresh")
}

func (f *fakeDexAPI) RevokeRefresh(context.Context, *dexapi.RevokeRefreshReq, ...grpc.CallOption) (*dexapi.RevokeRefreshResp, error) {
	return nil, status.Error(codes.Unimplemented, "RevokeRefresh")
}

func (f *fakeDexAPI) VerifyPassword(context.Context, *dexapi.VerifyPasswordReq, ...grpc.CallOption) (*dexapi.VerifyPasswordResp, error) {
	return nil, status.Error(codes.Unimplemented, "VerifyPassword")
}
